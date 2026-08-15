// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package genai

import (
	"bytes"
	"io"
	"sync/atomic"
	"time"
)

// streamReader reassembles a server-sent-event response body while the caller
// reads it, so the span carries usage data the provider only reports in the
// final chunks.
//
// The SSE framing, buffering and finalisation below are lifted unchanged from
// instrumentation/github.com/openai/openai-go/internal/streaming, the module
// v1, v2 and v3 already shared. The one substantive change is that per-chunk
// JSON decoding, which was a switch over the provider's operation types, is
// now delegated to the adapter's ParseStreamChunk.
type streamReader struct {
	reader     io.ReadCloser
	teeReader  io.Reader
	logBuffer  *bytes.Buffer
	lineBuffer *bytes.Buffer
	first      time.Time
	acc        ResponseInfo
	adapter    StreamAdapter
	inst       *instrumentation
	state      *spanState
	done       atomic.Bool
}

func newStreamReader(
	body io.ReadCloser,
	adapter StreamAdapter,
	inst *instrumentation,
	state *spanState,
) io.ReadCloser {
	return &streamReader{
		reader:  body,
		adapter: adapter,
		inst:    inst,
		state:   state,
	}
}

func (r *streamReader) Read(p []byte) (int, error) {
	if r.teeReader == nil {
		r.logBuffer = &bytes.Buffer{}
		r.lineBuffer = &bytes.Buffer{}
		r.teeReader = io.TeeReader(r.reader, r.logBuffer)
	}

	n, err := r.teeReader.Read(p)

	if n > 0 {
		r.processSSELines()
	}

	if err != nil && r.done.CompareAndSwap(false, true) {
		// Only a clean EOF means lineBuffer holds a complete, unterminated
		// final line. On any other read error the buffered bytes may be a
		// truncated mid-chunk fragment, so leave them unparsed rather than
		// attaching stale attributes to a span that ended in failure.
		r.finalize(err == io.EOF)
	}

	return n, err
}

func (r *streamReader) Close() error {
	if r.done.CompareAndSwap(false, true) {
		r.finalize(true)
	}
	if r.reader != nil {
		return r.reader.Close()
	}
	return nil
}

func (r *streamReader) finalize(flush bool) {
	if flush {
		r.flushRemaining()
	}

	// The four attributes below are emitted unconditionally, where the
	// non-streaming path emits each only when the adapter reported it. That
	// asymmetry predates this core — it is how the shared streaming reader
	// has always behaved — and is preserved rather than quietly unified.
	span := r.state.span
	span.SetAttributes(
		GenAIResponseFinishReasons(r.acc.FinishReasons),
		GenAIUsageInputTokens(derefInt64(r.acc.Usage.InputTokens)),
		GenAIUsageOutputTokens(derefInt64(r.acc.Usage.OutputTokens)),
		GenAIUsageTotalTokens(derefInt64(r.acc.Usage.TotalTokens)),
	)
	if r.acc.ID != "" {
		span.SetAttributes(GenAIResponseID(r.acc.ID))
	}
	if r.acc.Model != "" {
		span.SetAttributes(GenAIResponseModel(r.acc.Model))
	}
	if !r.first.IsZero() {
		firstTokenUs := r.first.Sub(r.state.start).Microseconds()
		span.SetAttributes(GenAIResponseTimeToFirstToken(firstTokenUs))
	}
	if extra := extensionAttributes(r.acc.Ext); len(extra) > 0 {
		span.SetAttributes(extra...)
	}

	r.inst.recordTokens(r.state.ctx, r.acc.Usage, r.state.baseAttrs)
	r.inst.recordDuration(r.state.ctx, r.state.start, r.state.baseAttrs)

	span.End()
}

// flushRemaining parses whatever is left in lineBuffer as a final line. The
// underlying reader can report an error (typically io.EOF) right after the
// last data line, with no trailing newline to trigger processSSELines, so
// that last chunk would otherwise sit unparsed in lineBuffer forever.
func (r *streamReader) flushRemaining() {
	if r.lineBuffer == nil || r.lineBuffer.Len() == 0 {
		return
	}

	line := bytes.TrimSpace(r.lineBuffer.Bytes())
	r.lineBuffer.Reset()
	if len(line) == 0 {
		return
	}

	payload, done := parseSSELine(line)
	if done || payload == nil {
		return
	}
	r.processChunk(payload)
}

func (r *streamReader) processSSELines() {
	if r.logBuffer == nil || r.logBuffer.Len() == 0 {
		return
	}

	data := r.logBuffer.Bytes()
	_, _ = r.lineBuffer.Write(data)
	r.logBuffer.Reset()

	allData := r.lineBuffer.Bytes()
	lines := bytes.Split(allData, []byte("\n"))

	var incompleteLine []byte
	for i, line := range lines {
		if i == len(lines)-1 {
			if len(line) > 0 {
				incompleteLine = line
			}
			break
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		payload, done := parseSSELine(line)
		if done {
			continue
		}
		if payload != nil {
			r.processChunk(payload)
		}
	}

	r.lineBuffer.Reset()
	if len(incompleteLine) > 0 {
		_, _ = r.lineBuffer.Write(incompleteLine)
	}
}

func parseSSELine(line []byte) ([]byte, bool) {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return nil, false
	}
	payload := bytes.TrimPrefix(line, []byte("data: "))
	if bytes.Equal(payload, []byte("[DONE]")) {
		return nil, true
	}
	return payload, false
}

func (r *streamReader) processChunk(payload []byte) {
	if r.first.IsZero() {
		r.first = time.Now()
	}
	r.adapter.ParseStreamChunk(payload, &r.acc)
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
