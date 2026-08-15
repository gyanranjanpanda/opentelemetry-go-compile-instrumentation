// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package genai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otelc/pkg/runtime"
)

type recorders struct {
	spans   *tracetest.SpanRecorder
	metrics *sdkmetric.ManualReader
}

// newTestMiddleware wires an adapter to in-memory span and metric recorders.
// It deliberately passes the tracer and meter in rather than mutating the
// global providers, so tests do not interfere with one another.
func newTestMiddleware(
	t *testing.T,
	a Adapter,
) (func(*http.Request, func(*http.Request) (*http.Response, error)) (*http.Response, error), *recorders) {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	mr := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(mr))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	mw := HTTPMiddleware(a,
		WithTracer(tp.Tracer("test")),
		WithMeter(mp.Meter("test")),
	)
	return mw, &recorders{spans: sr, metrics: mr}
}

func request(t *testing.T, url, body string) *http.Request {
	t.Helper()
	var rc io.ReadCloser
	if body != "" {
		rc = io.NopCloser(bytes.NewReader([]byte(body)))
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, rc)
	require.NoError(t, err)
	return req
}

func respondWith(status int, contentType, body string) func(*http.Request) (*http.Response, error) {
	return func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Status:     http.StatusText(status),
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		}, nil
	}
}

func attrMap(attrs []attribute.KeyValue) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(attrs))
	for _, a := range attrs {
		out[string(a.Key)] = a.Value
	}
	return out
}

func TestHTTPMiddleware_UnaryChat(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1","max_tokens":100,"temperature":0.7,"top_k":40}`)
	resp, err := mw(req, respondWith(http.StatusOK, "application/json",
		`{"id":"r-1","model":"m-1","reason":"stop","in":10,"out":20,"total":30}`))
	require.NoError(t, err)
	require.NotNil(t, resp)

	spans := rec.spans.Ended()
	require.Len(t, spans, 1)

	span := spans[0]
	assert.Equal(t, "chat m-1", span.Name())
	assert.Equal(t, trace.SpanKindClient, span.SpanKind())
	assert.Equal(t, otelcodes.Unset, span.Status().Code)

	got := attrMap(span.Attributes())
	assert.Equal(t, "stub", got["gen_ai.system"].AsString())
	assert.Equal(t, "chat", got["gen_ai.operation.name"].AsString())
	assert.Equal(t, "m-1", got["gen_ai.request.model"].AsString())
	assert.Equal(t, "openai", got["gen_ai.provider.name"].AsString(), "host table wins over the adapter fallback")
	assert.Equal(t, int64(100), got["gen_ai.request.max_tokens"].AsInt64())
	assert.InDelta(t, 0.7, got["gen_ai.request.temperature"].AsFloat64(), 0.001)
	assert.Equal(t, int64(40), got["gen_ai.request.top_k"].AsInt64())
	assert.Equal(t, "r-1", got["gen_ai.response.id"].AsString())
	assert.Equal(t, []string{"stop"}, got["gen_ai.response.finish_reasons"].AsStringSlice())
	assert.Equal(t, int64(10), got["gen_ai.usage.input_tokens"].AsInt64())
	assert.Equal(t, int64(20), got["gen_ai.usage.output_tokens"].AsInt64())
	assert.Equal(t, int64(30), got["gen_ai.usage.total_tokens"].AsInt64())

	// Parameters the adapter left unset produce no attribute at all.
	assert.NotContains(t, got, "gen_ai.request.top_p")
	assert.NotContains(t, got, "gen_ai.request.frequency_penalty")
}

// TestHTTPMiddleware_MessagesNeverEmitted pins the content-capture boundary:
// the adapter populates RequestInfo.Messages on every call and the core must
// not put any of it on the span.
func TestHTTPMiddleware_MessagesNeverEmitted(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1"}`)
	_, err := mw(req, respondWith(http.StatusOK, "application/json", `{"id":"r-1"}`))
	require.NoError(t, err)

	spans := rec.spans.Ended()
	require.Len(t, spans, 1)
	for _, a := range spans[0].Attributes() {
		assert.NotContains(t, a.Value.String(), "secret prompt")
	}
	assert.Empty(t, spans[0].Events())
}

// TestHTTPMiddleware_PresenceIsTheAdaptersToDeclare covers the rule that
// distinguishes an operation with no output-token concept from one reporting
// zero: embeddings leaves the pointers nil and gets no attributes.
func TestHTTPMiddleware_PresenceIsTheAdaptersToDeclare(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	req := request(t, "http://api.openai.com/v1/embed", `{"model":"e-1"}`)
	_, err := mw(req, respondWith(http.StatusOK, "application/json", `{"model":"e-1","in":2,"total":2}`))
	require.NoError(t, err)

	spans := rec.spans.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, "embeddings e-1", spans[0].Name())

	got := attrMap(spans[0].Attributes())
	assert.Equal(t, int64(2), got["gen_ai.usage.input_tokens"].AsInt64())
	assert.Equal(t, int64(2), got["gen_ai.usage.total_tokens"].AsInt64())
	assert.NotContains(t, got, "gen_ai.usage.output_tokens")
	assert.NotContains(t, got, "gen_ai.response.finish_reasons")
	assert.NotContains(t, got, "gen_ai.response.id")
}

// TestHTTPMiddleware_EmptyFinishReasonsStillEmitted is the other half of the
// presence rule: an empty but non-nil slice means "reported, and there were
// none", which must still produce an attribute.
func TestHTTPMiddleware_EmptyFinishReasonsStillEmitted(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1"}`)
	_, err := mw(req, respondWith(http.StatusOK, "application/json", `{"id":"r-1","model":"m-1"}`))
	require.NoError(t, err)

	spans := rec.spans.Ended()
	require.Len(t, spans, 1)
	got := attrMap(spans[0].Attributes())
	require.Contains(t, got, "gen_ai.response.finish_reasons")
	assert.Empty(t, got["gen_ai.response.finish_reasons"].AsStringSlice())
}

func TestHTTPMiddleware_PassThrough(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
		why  string
	}{
		{"unknown operation", "/v1/models", `{"model":"m-1"}`, "Classify returned OperationUnknown"},
		{"unparsable body", "/v1/chat", `not json`, "ParseRequest returned an error"},
		{"no model", "/v1/chat", `{"max_tokens":10}`, "ParseRequest returned an empty Model"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

			req := request(t, "http://api.openai.com"+tt.path, tt.body)
			called := false
			resp, err := mw(req, func(r *http.Request) (*http.Response, error) {
				called = true
				body, readErr := io.ReadAll(r.Body)
				require.NoError(t, readErr)
				assert.Equal(t, tt.body, string(body), "the SDK must still see the whole body")
				return respondWith(http.StatusOK, "application/json", `{}`)(r)
			})
			require.NoError(t, err)
			require.NotNil(t, resp)
			assert.True(t, called)
			assert.Empty(t, rec.spans.Ended(), tt.why)
		})
	}
}

func TestHTTPMiddleware_NilBody(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	req := request(t, "http://api.openai.com/v1/chat", "")
	_, err := mw(req, respondWith(http.StatusOK, "application/json", `{}`))
	require.NoError(t, err)
	assert.Empty(t, rec.spans.Ended())
}

func TestHTTPMiddleware_TransportError(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	wantErr := errors.New("dial failed")
	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1"}`)
	_, err := mw(req, func(*http.Request) (*http.Response, error) { return nil, wantErr })
	require.ErrorIs(t, err, wantErr)

	spans := rec.spans.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, otelcodes.Error, spans[0].Status().Code)
	require.Len(t, spans[0].Events(), 1)
	assert.Equal(t, "exception", spans[0].Events()[0].Name)
	assert.Equal(t, "*errors.errorString", attrMap(spans[0].Attributes())["error.type"].AsString())
}

func TestHTTPMiddleware_HTTPErrorStatus(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1"}`)
	_, err := mw(req, respondWith(http.StatusTooManyRequests, "application/json", ``))
	require.NoError(t, err)

	spans := rec.spans.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, otelcodes.Error, spans[0].Status().Code)
	require.Len(t, spans[0].Events(), 1)
	// error.type carries the bare status code, matching openai-go's shape
	// rather than anthropic-sdk-go's full status line.
	assert.Equal(t, "429", attrMap(spans[0].Attributes())["error.type"].AsString())
	assert.NotContains(t, attrMap(spans[0].Attributes()), "gen_ai.usage.input_tokens")
}

// TestHTTPMiddleware_BodyReassembly is the behaviour that matters most to a
// user application: whatever the core reads for parsing, the SDK downstream
// must still see a complete, intact body.
func TestHTTPMiddleware_BodyReassembly(t *testing.T) {
	mw, _ := newTestMiddleware(t, stubAdapter{provider: "stub"})

	reqBody := `{"model":"m-1","padding":"` + string(bytes.Repeat([]byte("x"), 4096)) + `"}`
	respBody := `{"id":"r-1","model":"m-1","in":1,"out":1,"total":2}`

	req := request(t, "http://api.openai.com/v1/chat", reqBody)
	resp, err := mw(req, func(r *http.Request) (*http.Response, error) {
		got, readErr := io.ReadAll(r.Body)
		require.NoError(t, readErr)
		assert.Equal(t, reqBody, string(got))
		return respondWith(http.StatusOK, "application/json", respBody)(r)
	})
	require.NoError(t, err)

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, respBody, string(got))
}

type partialReadErrorReader struct {
	prefix []byte
	err    error
	read   bool
}

func (r *partialReadErrorReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, r.prefix), nil
	}
	return 0, r.err
}

func TestHTTPMiddleware_RequestBodyReadError(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	wantErr := errors.New("read fail")
	req := request(t, "http://api.openai.com/v1/chat", "placeholder")
	req.Body = io.NopCloser(&partialReadErrorReader{prefix: []byte(`{"model":"m-1"}`), err: wantErr})

	called := false
	_, err := mw(req, func(r *http.Request) (*http.Response, error) {
		called = true
		body, readErr := io.ReadAll(r.Body)
		require.ErrorIs(t, readErr, wantErr)
		//nolint:testifylint // byte fidelity is the assertion; JSONEq would not catch a mangled body
		assert.Equal(t, []byte(`{"model":"m-1"}`), body, "bytes already consumed must be handed back")
		return respondWith(http.StatusOK, "application/json", `{}`)(r)
	})
	require.NoError(t, err)
	assert.True(t, called)
	assert.Empty(t, rec.spans.Ended())
}

func TestHTTPMiddleware_ResponseBodyReadError(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	wantErr := errors.New("response read fail")
	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1"}`)
	resp, err := mw(req, func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(&partialReadErrorReader{prefix: []byte(`{"id":"r-1"}`), err: wantErr}),
		}, nil
	})
	require.NoError(t, err)

	body, err := io.ReadAll(resp.Body)
	require.ErrorIs(t, err, wantErr)
	//nolint:testifylint // byte fidelity is the assertion; JSONEq would not catch a mangled body
	assert.Equal(t, []byte(`{"id":"r-1"}`), body)
	assert.Len(t, rec.spans.Ended(), 1, "the span still ends, just without response attributes")
}

func TestHTTPMiddleware_Extensions(t *testing.T) {
	tests := []struct {
		name   string
		ext    Extension
		assert func(*testing.T, map[string]attribute.Value)
	}{
		{
			name: "known extension is emitted",
			ext:  CacheUsage{ReadInputTokens: 7, CreationInputTokens: 3},
			assert: func(t *testing.T, got map[string]attribute.Value) {
				t.Helper()
				assert.Equal(t, int64(7), got["gen_ai.usage.cache_read.input_tokens"].AsInt64())
				assert.Equal(t, int64(3), got["gen_ai.usage.cache_creation.input_tokens"].AsInt64())
			},
		},
		{
			name: "zero counts are not emitted",
			ext:  CacheUsage{},
			assert: func(t *testing.T, got map[string]attribute.Value) {
				t.Helper()
				assert.NotContains(t, got, "gen_ai.usage.cache_read.input_tokens")
				assert.NotContains(t, got, "gen_ai.usage.cache_creation.input_tokens")
			},
		},
		{
			name: "unknown extension is ignored, not fatal",
			ext:  unknownExtension{Note: "from a newer adapter"},
			assert: func(t *testing.T, got map[string]attribute.Value) {
				t.Helper()
				assert.Equal(t, "r-1", got["gen_ai.response.id"].AsString())
				for k := range got {
					assert.NotContains(t, k, "note")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := stubAdapter{
				provider: "stub",
				parseResp: func(Call) (ResponseInfo, error) {
					return ResponseInfo{ID: String("r-1"), Ext: tt.ext}, nil
				},
			}
			mw, rec := newTestMiddleware(t, a)

			req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1"}`)
			_, err := mw(req, respondWith(http.StatusOK, "application/json", `{}`))
			require.NoError(t, err)

			spans := rec.spans.Ended()
			require.Len(t, spans, 1)
			tt.assert(t, attrMap(spans[0].Attributes()))
		})
	}
}

func collectMetrics(t *testing.T, mr *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, mr.Collect(context.Background(), &rm))

	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func TestHTTPMiddleware_Metrics(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1"}`)
	_, err := mw(req, respondWith(http.StatusOK, "application/json",
		`{"id":"r-1","model":"m-1","reason":"stop","in":10,"out":20,"total":30}`))
	require.NoError(t, err)

	metrics := collectMetrics(t, rec.metrics)

	duration, ok := metrics[MetricOperationDuration]
	require.True(t, ok, "operation duration histogram must be recorded")
	assert.Equal(t, "s", duration.Unit)
	durationData, ok := duration.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, durationData.DataPoints, 1)
	assert.Equal(t, uint64(1), durationData.DataPoints[0].Count)

	tokens, ok := metrics[MetricTokenUsage]
	require.True(t, ok, "token usage histogram must be recorded")
	assert.Equal(t, "{token}", tokens.Unit)
	tokenData, ok := tokens.Data.(metricdata.Histogram[int64])
	require.True(t, ok)
	require.Len(t, tokenData.DataPoints, 2, "one series per gen_ai.token.type")

	byType := map[string]int64{}
	for _, dp := range tokenData.DataPoints {
		tokenType, present := dp.Attributes.Value(GenAITokenTypeKey)
		require.True(t, present)
		byType[tokenType.AsString()] = dp.Sum
	}
	assert.Equal(t, int64(10), byType[TokenTypeInput])
	assert.Equal(t, int64(20), byType[TokenTypeOutput])
}

func TestHTTPMiddleware_MetricsOnErrorPaths(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1"}`)
	_, err := mw(req, respondWith(http.StatusInternalServerError, "application/json", ``))
	require.NoError(t, err)

	metrics := collectMetrics(t, rec.metrics)
	_, hasDuration := metrics[MetricOperationDuration]
	assert.True(t, hasDuration, "a failed call still has a duration")
	_, hasTokens := metrics[MetricTokenUsage]
	assert.False(t, hasTokens, "a failed call reports no tokens")
}

// TestHTTPMiddleware_NoMetricsWithoutSpan guards the same boundary the
// per-provider middleware drew: nothing is recorded for a call that was never
// traced in the first place.
func TestHTTPMiddleware_NoMetricsWithoutSpan(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	req := request(t, "http://api.openai.com/v1/models", `{"model":"m-1"}`)
	_, err := mw(req, respondWith(http.StatusOK, "application/json", `{}`))
	require.NoError(t, err)

	assert.Empty(t, collectMetrics(t, rec.metrics))
}

// TestHTTPMiddleware_SuppressesHTTPClientSpan checks the core still hands the
// downstream net/http client hook the suppression flag, so a GenAI call does
// not also produce a generic HTTP client span.
func TestHTTPMiddleware_SuppressesHTTPClientSpan(t *testing.T) {
	mw, _ := newTestMiddleware(t, stubAdapter{provider: "stub"})

	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1"}`)
	suppressed := false
	_, err := mw(req, func(r *http.Request) (*http.Response, error) {
		suppressed = runtime.IsHTTPClientInstrumentationSuppressed(r.Context())
		return respondWith(http.StatusOK, "application/json", `{}`)(r)
	})
	require.NoError(t, err)
	assert.True(t, suppressed)
}
