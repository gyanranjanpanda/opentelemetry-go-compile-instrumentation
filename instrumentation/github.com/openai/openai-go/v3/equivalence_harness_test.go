// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package v3

// Equivalence harness.
//
// This file captures the complete telemetry the openai-go v3 instrumentation
// emits -- every span name, kind, scope, status and attribute, plus every
// metric -- for a set of representative scenarios, and asserts it against
// golden data recorded from the current implementation.
//
// It exists to be written BEFORE the module is migrated onto pkg/genai. The
// golden data therefore encodes what otelc does today, not what the shared
// core happens to do; written the other way round it would pass by
// construction and prove nothing. The harness drives OtelMiddleware, whose
// name and signature the migration does not change, so the same file runs
// unchanged against both implementations.
//
// The spans section of the golden data is frozen: -update-golden-spans exists
// only to record the original baseline, and re-running it after a migration
// would defeat the point. Metrics are separate because OpenAI emits none today
// and the shared core emits two, a deliberate addition rather than a
// regression.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otelc/pkg/runtime"
)

//nolint:gochecknoglobals // test flags must be package level for the flag package
var (
	updateGoldenSpans = flag.Bool("update-golden-spans", false,
		"record the span baseline. Only legitimate before a migration.")
	updateGoldenMetrics = flag.Bool("update-golden-metrics", false,
		"record the metric baseline, for deliberate additions.")
)

const goldenDir = "testdata/equivalence"

// nondeterministic marks attribute values that cannot be compared literally.
// The attribute must still be present, and its value must still be positive;
// only the exact number is discarded.
const nondeterministic = "<positive-int64>"

// volatileEventAttributes are dropped from captured events: a stack trace
// embeds file paths and line numbers of the test binary.
var volatileEventAttributes = map[string]bool{ //nolint:gochecknoglobals // test fixture
	"exception.stacktrace": true,
}

type capturedAttribute struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

type capturedEvent struct {
	Name       string              `json:"name"`
	Attributes []capturedAttribute `json:"attributes"`
}

type capturedSpan struct {
	Name              string              `json:"name"`
	Kind              string              `json:"kind"`
	Scope             string              `json:"scope"`
	StatusCode        string              `json:"status_code"`
	StatusDescription string              `json:"status_description"`
	Attributes        []capturedAttribute `json:"attributes"`
	Events            []capturedEvent     `json:"events"`
}

type capturedDataPoint struct {
	Attributes []capturedAttribute `json:"attributes"`
	Count      uint64              `json:"count"`
	// Sum is recorded only for integer histograms. A duration sum is wall
	// clock and would never reproduce.
	Sum *int64 `json:"sum,omitempty"`
}

type capturedMetric struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Unit        string              `json:"unit"`
	Scope       string              `json:"scope"`
	DataPoints  []capturedDataPoint `json:"data_points"`
}

type telemetry struct {
	Spans   []capturedSpan   `json:"spans"`
	Metrics []capturedMetric `json:"metrics"`
}

func captureAttributes(attrs []attribute.KeyValue) []capturedAttribute {
	out := make([]capturedAttribute, 0, len(attrs))
	for _, kv := range attrs {
		captured := capturedAttribute{Key: string(kv.Key), Type: kv.Value.Type().String()}
		switch kv.Value.Type() { //nolint:exhaustive // the default arm renders every other type
		case attribute.BOOL:
			captured.Value = kv.Value.AsBool()
		case attribute.INT64:
			captured.Value = kv.Value.AsInt64()
		case attribute.FLOAT64:
			captured.Value = kv.Value.AsFloat64()
		case attribute.STRING:
			captured.Value = kv.Value.AsString()
		case attribute.STRINGSLICE:
			captured.Value = kv.Value.AsStringSlice()
		default:
			// No GenAI attribute uses these types today; capturing the
			// rendered form keeps an unexpected one visible rather than
			// silently dropped.
			captured.Value = kv.Value.String()
		}
		out = append(out, captured)
	}
	slices.SortFunc(out, func(a, b capturedAttribute) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

func captureSpans(t *testing.T, sr *tracetest.SpanRecorder) []capturedSpan {
	t.Helper()

	ended := sr.Ended()
	out := make([]capturedSpan, 0, len(ended))
	for _, span := range ended {
		attrs := captureAttributes(span.Attributes())
		for i, a := range attrs {
			if a.Key != "gen_ai.response.time_to_first_token" {
				continue
			}
			value, ok := a.Value.(int64)
			require.True(t, ok, "time_to_first_token must be an int64")
			assert.Positive(t, value, "time_to_first_token must be positive")
			attrs[i].Value = nondeterministic
		}

		events := []capturedEvent{}
		for _, e := range span.Events() {
			kept := make([]attribute.KeyValue, 0, len(e.Attributes))
			for _, a := range e.Attributes {
				if !volatileEventAttributes[string(a.Key)] {
					kept = append(kept, a)
				}
			}
			events = append(events, capturedEvent{Name: e.Name, Attributes: captureAttributes(kept)})
		}

		out = append(out, capturedSpan{
			Name:              span.Name(),
			Kind:              span.SpanKind().String(),
			Scope:             span.InstrumentationScope().Name,
			StatusCode:        span.Status().Code.String(),
			StatusDescription: span.Status().Description,
			Attributes:        attrs,
			Events:            events,
		})
	}
	return out
}

func captureMetrics(t *testing.T, reader *sdkmetric.ManualReader) []capturedMetric {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	out := []capturedMetric{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			captured := capturedMetric{
				Name:        m.Name,
				Description: m.Description,
				Unit:        m.Unit,
				Scope:       sm.Scope.Name,
				DataPoints:  []capturedDataPoint{},
			}
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					captured.DataPoints = append(captured.DataPoints, capturedDataPoint{
						Attributes: captureAttributes(dp.Attributes.ToSlice()),
						Count:      dp.Count,
					})
				}
			case metricdata.Histogram[int64]:
				for _, dp := range data.DataPoints {
					sum := dp.Sum
					captured.DataPoints = append(captured.DataPoints, capturedDataPoint{
						Attributes: captureAttributes(dp.Attributes.ToSlice()),
						Count:      dp.Count,
						Sum:        &sum,
					})
				}
			default:
				t.Fatalf("metric %q has unhandled data type %T", m.Name, m.Data)
			}
			slices.SortFunc(captured.DataPoints, func(a, b capturedDataPoint) int {
				return cmp.Compare(renderAttributes(a.Attributes), renderAttributes(b.Attributes))
			})
			out = append(out, captured)
		}
	}
	slices.SortFunc(out, func(a, b capturedMetric) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

func renderAttributes(attrs []capturedAttribute) string {
	var sb strings.Builder
	for _, a := range attrs {
		sb.WriteString(a.Key)
		sb.WriteString("=")
		encoded, _ := json.Marshal(a.Value)
		sb.Write(encoded)
		sb.WriteString(";")
	}
	return sb.String()
}

// scenario is one request/response exchange to run through the middleware.
type scenario struct {
	name string
	// request builds the outbound request.
	request func(t *testing.T) *http.Request
	// next stands in for the rest of the SDK's transport chain.
	next func(*http.Request) (*http.Response, error)
	// wantErr is whether the middleware is expected to surface an error.
	wantErr bool
	// drainStream reads the response body to completion and closes it, which
	// is what finalises a streamed span.
	drainStream bool
}

func jsonResponse(body string) func(*http.Request) (*http.Response, error) {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     http.StatusText(http.StatusOK),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		}, nil
	}
}

func postRequest(url, body string) func(t *testing.T) *http.Request {
	return func(t *testing.T) *http.Request {
		t.Helper()
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodPost, url, io.NopCloser(bytes.NewReader([]byte(body))))
		require.NoError(t, err)
		return req
	}
}

const streamingChatBody = "data: {\"id\":\"chatcmpl-stream\",\"model\":\"gpt-4\"," +
	"\"choices\":[{\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-stream\",\"model\":\"gpt-4\"," +
	"\"choices\":[{\"delta\":{\"content\":\" world\"},\"finish_reason\":\"stop\"}]," +
	"\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
	"data: [DONE]\n\n"

func equivalenceScenarios() []scenario {
	return []scenario{
		{
			name: "unary_chat_completion",
			request: postRequest("http://api.openai.com/v1/chat/completions",
				`{"model":"gpt-4","max_tokens":100,"temperature":0.7,"top_p":0.9,`+
					`"frequency_penalty":0.5,"presence_penalty":0.3,`+
					`"messages":[{"role":"user","content":"hello"}]}`),
			next: jsonResponse(
				`{"id":"chatcmpl-123","model":"gpt-4","choices":[{"finish_reason":"stop"}],` +
					`"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`),
		},
		{
			name: "text_completion",
			request: postRequest("http://api.openai.com/v1/completions",
				`{"model":"gpt-3.5-turbo-instruct","max_tokens":50,"temperature":0.5}`),
			next: jsonResponse(
				`{"id":"cmpl-456","model":"gpt-3.5-turbo-instruct","choices":[{"finish_reason":"length"}],` +
					`"usage":{"prompt_tokens":5,"completion_tokens":50,"total_tokens":55}}`),
		},
		{
			name: "embeddings",
			request: postRequest("http://api.openai.com/v1/embeddings",
				`{"model":"text-embedding-ada-002","input":"hello world"}`),
			next: jsonResponse(
				`{"model":"text-embedding-ada-002","usage":{"prompt_tokens":2,"total_tokens":2}}`),
		},
		{
			name:    "error_response_429",
			request: postRequest("http://api.openai.com/v1/chat/completions", `{"model":"gpt-4"}`),
			next: func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Status:     "429 Too Many Requests",
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader(nil)),
				}, nil
			},
		},
		{
			name:    "transport_error",
			request: postRequest("http://api.openai.com/v1/chat/completions", `{"model":"gpt-4"}`),
			next: func(*http.Request) (*http.Response, error) {
				return nil, assert.AnError
			},
			wantErr: true,
		},
		{
			name: "streaming_chat_completion",
			request: postRequest("http://api.openai.com/v1/chat/completions",
				`{"model":"gpt-4","stream":true}`),
			next: func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     http.StatusText(http.StatusOK),
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(bytes.NewReader([]byte(streamingChatBody))),
				}, nil
			},
			drainStream: true,
		},
		{
			name: "azure_deployment_path",
			request: postRequest("http://myendpoint.azure.com/openai/deployments/gpt-4/chat/completions",
				`{"model":"gpt-4"}`),
			next: jsonResponse(
				`{"id":"azure-123","model":"gpt-4","choices":[{"finish_reason":"stop"}],` +
					`"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`),
		},
		{
			name:    "untraced_unknown_operation",
			request: postRequest("http://api.openai.com/v1/models", `{"model":"gpt-4"}`),
			next:    jsonResponse(`{}`),
		},
	}
}

// runScenario drives one exchange through OtelMiddleware and returns
// everything the instrumentation emitted.
func runScenario(t *testing.T, sc scenario) telemetry {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	// Set both globals and the package-level tracer, so the harness works
	// whether the implementation resolves its handles from the global
	// providers or from the variables the hook initialises. The scope name is
	// the real one rather than a test placeholder, so a migration that
	// silently reattributes spans to a different instrumentation scope shows
	// up as a difference.
	previousTracerProvider := otel.GetTracerProvider()
	previousMeterProvider := otel.GetMeterProvider()
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previousTracerProvider)
		otel.SetMeterProvider(previousMeterProvider)
	})
	tracer = tp.Tracer(instrumentationName, trace.WithInstrumentationVersion(runtime.ModuleVersion()))

	middleware := OtelMiddleware()
	resp, err := middleware(sc.request(t), sc.next)
	if sc.wantErr {
		require.Error(t, err)
	} else {
		require.NoError(t, err)
		require.NotNil(t, resp)
	}

	if resp != nil && resp.Body != nil {
		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, readErr)
		require.NoError(t, resp.Body.Close())
		if sc.drainStream {
			assert.Equal(t, streamingChatBody, string(body),
				"the caller must still receive the whole stream")
		}
	}

	return telemetry{Spans: captureSpans(t, sr), Metrics: captureMetrics(t, reader)}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.MarshalIndent(v, "", "  ")
	require.NoError(t, err)
	return string(encoded)
}

func goldenPath(name string) string {
	return filepath.Join(goldenDir, name+".json")
}

func loadGolden(t *testing.T, name string) telemetry {
	t.Helper()
	raw, err := os.ReadFile(goldenPath(name))
	require.NoError(t, err, "golden data missing; record it with -update-golden-spans")

	var golden telemetry
	require.NoError(t, json.Unmarshal(raw, &golden))
	return golden
}

// writeGolden rewrites only the requested section, so recording a deliberate
// metric addition cannot quietly rewrite the frozen span baseline.
func writeGolden(t *testing.T, name string, got telemetry, spans, metrics bool) {
	t.Helper()
	require.NoError(t, os.MkdirAll(goldenDir, 0o750))

	merged := telemetry{Spans: []capturedSpan{}, Metrics: []capturedMetric{}}
	if raw, err := os.ReadFile(goldenPath(name)); err == nil {
		require.NoError(t, json.Unmarshal(raw, &merged))
	}
	if spans {
		merged.Spans = got.Spans
	}
	if metrics {
		merged.Metrics = got.Metrics
	}
	require.NoError(t, os.WriteFile(goldenPath(name), []byte(mustJSON(t, merged)+"\n"), 0o600))
}

func TestGenAIEquivalence(t *testing.T) {
	for _, sc := range equivalenceScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			got := runScenario(t, sc)

			if *updateGoldenSpans || *updateGoldenMetrics {
				writeGolden(t, sc.name, got, *updateGoldenSpans, *updateGoldenMetrics)
				t.Skip("golden data recorded")
			}

			golden := loadGolden(t, sc.name)

			t.Run("spans", func(t *testing.T) {
				assert.Equal(t, mustJSON(t, golden.Spans), mustJSON(t, got.Spans),
					"span telemetry must match the recorded baseline attribute for attribute")
			})
			t.Run("metrics", func(t *testing.T) {
				assert.Equal(t, mustJSON(t, golden.Metrics), mustJSON(t, got.Metrics),
					"metric telemetry must match the recorded baseline")
			})
		})
	}
}
