// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package genai

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func streamBody(lines ...string) string {
	return strings.Join(lines, "\n\n") + "\n\n"
}

func TestHTTPMiddleware_Streaming(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubStreamAdapter{stubAdapter: stubAdapter{provider: "stub"}})

	body := streamBody(
		`data: {"id":"s-1","model":"m-1"}`,
		`data: {"reason":"stop","in":5,"out":2,"total":7}`,
		`data: [DONE]`,
	)

	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1","stream":true}`)
	resp, err := mw(req, respondWith(http.StatusOK, "text/event-stream", body))
	require.NoError(t, err)

	assert.Empty(t, rec.spans.Ended(), "the span stays open until the stream is consumed")

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(got), "the caller must still see every byte of the stream")
	require.NoError(t, resp.Body.Close())

	spans := rec.spans.Ended()
	require.Len(t, spans, 1)

	attrs := attrMap(spans[0].Attributes())
	assert.Equal(t, "chat m-1", spans[0].Name())
	assert.True(t, attrs["gen_ai.request.is_stream"].AsBool())
	assert.Equal(t, "s-1", attrs["gen_ai.response.id"].AsString())
	assert.Equal(t, "m-1", attrs["gen_ai.response.model"].AsString())
	assert.Equal(t, []string{"stop"}, attrs["gen_ai.response.finish_reasons"].AsStringSlice())
	assert.Equal(t, int64(5), attrs["gen_ai.usage.input_tokens"].AsInt64())
	assert.Equal(t, int64(2), attrs["gen_ai.usage.output_tokens"].AsInt64())
	assert.Equal(t, int64(7), attrs["gen_ai.usage.total_tokens"].AsInt64())
	assert.Positive(t, attrs["gen_ai.response.time_to_first_token"].AsInt64())

	metrics := collectMetrics(t, rec.metrics)
	tokens, ok := metrics[MetricTokenUsage]
	require.True(t, ok, "a streamed call reports tokens too")
	tokenData, ok := tokens.Data.(metricdata.Histogram[int64])
	require.True(t, ok)
	assert.Len(t, tokenData.DataPoints, 2)
}

// TestHTTPMiddleware_StreamingChunkSplitAcrossReads exercises the line
// buffering: an SSE payload that arrives in pieces must still parse once.
func TestHTTPMiddleware_StreamingChunkSplitAcrossReads(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubStreamAdapter{stubAdapter: stubAdapter{provider: "stub"}})

	body := streamBody(
		`data: {"id":"s-1","model":"m-1","reason":"stop","in":5,"out":2,"total":7}`,
		`data: [DONE]`,
	)

	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1","stream":true}`)
	resp, err := mw(req, respondWith(http.StatusOK, "text/event-stream", body))
	require.NoError(t, err)

	// Drain one byte at a time so every chunk straddles a read boundary.
	var seen strings.Builder
	buf := make([]byte, 1)
	for {
		n, readErr := resp.Body.Read(buf)
		seen.Write(buf[:n])
		if readErr != nil {
			require.ErrorIs(t, readErr, io.EOF)
			break
		}
	}
	assert.Equal(t, body, seen.String())

	spans := rec.spans.Ended()
	require.Len(t, spans, 1)
	attrs := attrMap(spans[0].Attributes())
	assert.Equal(t, "s-1", attrs["gen_ai.response.id"].AsString())
	assert.Equal(t, int64(7), attrs["gen_ai.usage.total_tokens"].AsInt64())
}

// TestHTTPMiddleware_StreamingUnterminatedFinalLine covers the flush path: a
// stream whose last data line has no trailing newline before EOF.
func TestHTTPMiddleware_StreamingUnterminatedFinalLine(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubStreamAdapter{stubAdapter: stubAdapter{provider: "stub"}})

	body := `data: {"id":"s-1","in":5,"out":2,"total":7}`

	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1","stream":true}`)
	resp, err := mw(req, respondWith(http.StatusOK, "text/event-stream", body))
	require.NoError(t, err)

	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)

	spans := rec.spans.Ended()
	require.Len(t, spans, 1)
	assert.Equal(t, int64(7), attrMap(spans[0].Attributes())["gen_ai.usage.total_tokens"].AsInt64())
}

type errAfterPrefixReadCloser struct {
	prefix []byte
	err    error
	read   bool
}

func (r *errAfterPrefixReadCloser) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, r.prefix), nil
	}
	return 0, r.err
}

func (r *errAfterPrefixReadCloser) Close() error { return nil }

// TestHTTPMiddleware_StreamingReadErrorLeavesFragmentUnparsed pins the
// deliberate asymmetry in the lifted reader: only a clean EOF flushes the
// trailing buffer, because any other error may have truncated it mid-chunk.
func TestHTTPMiddleware_StreamingReadErrorLeavesFragmentUnparsed(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubStreamAdapter{stubAdapter: stubAdapter{provider: "stub"}})

	wantErr := errors.New("stream broke")
	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1","stream":true}`)
	resp, err := mw(req, func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: &errAfterPrefixReadCloser{
				prefix: []byte(`data: {"id":"s-1","in":5,"out":2,"total":7}`),
				err:    wantErr,
			},
		}, nil
	})
	require.NoError(t, err)

	_, err = io.ReadAll(resp.Body)
	require.ErrorIs(t, err, wantErr)

	spans := rec.spans.Ended()
	require.Len(t, spans, 1)
	attrs := attrMap(spans[0].Attributes())
	assert.Equal(t, int64(0), attrs["gen_ai.usage.total_tokens"].AsInt64(),
		"a truncated fragment must not attach stale attributes")
	assert.NotContains(t, attrs, "gen_ai.response.id")
}

// TestHTTPMiddleware_StreamingWithoutStreamAdapter documents the fallback for
// an adapter that implements only the four required methods.
func TestHTTPMiddleware_StreamingWithoutStreamAdapter(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubAdapter{provider: "stub"})

	body := streamBody(`data: {"id":"s-1","in":5}`, `data: [DONE]`)
	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1","stream":true}`)
	resp, err := mw(req, respondWith(http.StatusOK, "text/event-stream", body))
	require.NoError(t, err)

	spans := rec.spans.Ended()
	require.Len(t, spans, 1, "the span ends immediately rather than waiting on a body nobody parses")

	attrs := attrMap(spans[0].Attributes())
	assert.True(t, attrs["gen_ai.request.is_stream"].AsBool())
	assert.NotContains(t, attrs, "gen_ai.response.id")

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(got), "the body is still untouched")
}

// TestHTTPMiddleware_StreamingFinalizesOnce checks the atomic guard survived
// the lift: reading to EOF and then closing must not end the span twice.
func TestHTTPMiddleware_StreamingFinalizesOnce(t *testing.T) {
	mw, rec := newTestMiddleware(t, stubStreamAdapter{stubAdapter: stubAdapter{provider: "stub"}})

	body := streamBody(`data: {"id":"s-1","in":5,"out":2,"total":7}`, `data: [DONE]`)
	req := request(t, "http://api.openai.com/v1/chat", `{"model":"m-1","stream":true}`)
	resp, err := mw(req, respondWith(http.StatusOK, "text/event-stream", body))
	require.NoError(t, err)

	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, resp.Body.Close())

	assert.Len(t, rec.spans.Ended(), 1)

	durationData, ok := collectMetrics(t, rec.metrics)[MetricOperationDuration].
		Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, durationData.DataPoints, 1)
	assert.Equal(t, uint64(1), durationData.DataPoints[0].Count, "duration recorded exactly once")
}
