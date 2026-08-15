// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package genai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otelsemconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otelc/pkg/runtime"
)

const (
	maxRequestBodySize  = 1 << 20 // 1 MB
	maxResponseBodySize = 4 << 20 // 4 MB
)

// RoundTripFunc is the shape of the per-request handler the provider SDKs'
// middleware hooks expect: it is handed a request and the rest of the chain.
type RoundTripFunc func(*http.Request, func(*http.Request) (*http.Response, error)) (*http.Response, error)

// HTTPMiddleware returns an HTTP middleware that creates spans for a
// provider's API calls following GenAI semantic conventions. It is the shared
// implementation of what each provider module used to carry its own copy of.
//
// The body handling — the bounded read for parsing and the reassembly that
// hands the SDK back an intact stream — is lifted from that per-provider
// middleware unchanged. Getting it wrong breaks the caller's application, not
// just its telemetry.
func HTTPMiddleware(a Adapter, opts ...Option) RoundTripFunc {
	streamAdapter, _ := a.(StreamAdapter)
	h := &httpHandler{
		adapter: a,
		stream:  streamAdapter,
		inst:    newInstrumentation(opts...),
	}
	return h.roundTrip
}

type httpHandler struct {
	adapter Adapter
	stream  StreamAdapter
	inst    *instrumentation
}

// spanState is the per-call bookkeeping shared between the middleware's entry
// point and the handlers that close the span out.
type spanState struct {
	ctx       context.Context //nolint:containedctx // outlives the call on the streaming path
	span      trace.Span
	start     time.Time
	op        Operation
	baseAttrs []attribute.KeyValue
}

func (h *httpHandler) roundTrip(
	req *http.Request,
	next func(*http.Request) (*http.Response, error),
) (*http.Response, error) {
	if req.Body == nil {
		return next(req)
	}

	op := h.adapter.Classify(Call{
		Phase:  PhaseRequest,
		Method: req.Method,
		Host:   req.URL.Host,
		Path:   req.URL.Path,
		Header: req.Header,
	})
	if op == OperationUnknown {
		return next(req)
	}

	start := time.Now()
	provider := ProviderName(req.URL.Host, h.adapter.Provider())

	// Read a bounded copy for attribute parsing, but preserve the full body for the SDK.
	var buf bytes.Buffer
	tee := io.TeeReader(req.Body, &buf)
	bodyBytes, err := io.ReadAll(io.LimitReader(tee, maxRequestBodySize))
	// Reassemble: buffered bytes + remaining unread body.
	req.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(&buf, req.Body), req.Body}
	if err != nil {
		return next(req)
	}

	info, err := h.adapter.ParseRequest(Call{
		Phase:  PhaseRequest,
		Op:     op,
		Method: req.Method,
		Host:   req.URL.Host,
		Path:   req.URL.Path,
		Header: req.Header,
		Body:   bodyBytes,
	})
	// An unparsable body and a body with no model are both pass-throughs,
	// not errors: a span with no model would be worse than no span.
	if err != nil || info.Model == "" {
		return next(req)
	}

	baseAttrs := []attribute.KeyValue{
		GenAISystem(string(h.adapter.Provider())),
		GenAIOperationName(string(op)),
		GenAIRequestModel(info.Model),
		GenAIProviderName(provider),
	}
	optional := requestAttributes(info)
	spanAttrs := make([]attribute.KeyValue, 0, len(baseAttrs)+len(optional))
	spanAttrs = append(spanAttrs, baseAttrs...)
	spanAttrs = append(spanAttrs, optional...)

	// spancheck cannot follow the span into failed/succeeded/streamReader, each
	// of which ends it on every path; the stream case ends it after this
	// function has already returned.
	//nolint:spancheck // span is ended by failed, succeeded, or streamReader.finalize
	ctx, span := h.inst.tracer.Start(req.Context(), string(op)+" "+info.Model,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(spanAttrs...),
	)
	ctx = runtime.SuppressHTTPClientInstrumentation(ctx)
	req = req.WithContext(ctx)

	state := &spanState{ctx: ctx, span: span, start: start, op: op, baseAttrs: baseAttrs}

	resp, err := next(req)
	if err != nil {
		h.failed(state, err)
		return resp, err //nolint:spancheck // h.failed ended the span
	}
	return h.succeeded(state, req, resp)
}

// failed closes out a span whose call never reached the provider.
func (h *httpHandler) failed(state *spanState, err error) {
	state.span.SetStatus(codes.Error, err.Error())
	state.span.RecordError(err)
	state.span.SetAttributes(otelsemconv.ErrorType(err))
	h.inst.recordDuration(state.ctx, state.start, state.baseAttrs)
	state.span.End()
}

// succeeded closes out a span whose call reached the provider, whatever the
// provider then answered.
func (h *httpHandler) succeeded(
	state *spanState,
	req *http.Request,
	resp *http.Response,
) (*http.Response, error) {
	if resp.StatusCode >= http.StatusBadRequest {
		state.span.RecordError(errors.New(resp.Status))
		state.span.SetStatus(codes.Error, resp.Status)
		// The bare status code, matching openai-go's shape. anthropic-sdk-go
		// emits the full status line here instead; the two had already
		// diverged, and the core has to pick one.
		state.span.SetAttributes(otelsemconv.ErrorTypeKey.String(strconv.Itoa(resp.StatusCode)))
		h.inst.recordDuration(state.ctx, state.start, state.baseAttrs)
		state.span.End()
		return resp, nil
	}

	contentType := resp.Header.Get("Content-Type")
	isStreaming := strings.HasPrefix(contentType, "text/event-stream")

	if isStreaming {
		state.span.SetAttributes(GenAIRequestIsStream(true))
		if h.stream == nil {
			// No stream support: end the span with request attributes only
			// rather than hold it open on a body nobody parses.
			h.inst.recordDuration(state.ctx, state.start, state.baseAttrs)
			state.span.End()
			return resp, nil
		}
		resp.Body = newStreamReader(resp.Body, h.stream, h.inst, state)
		return resp, nil
	}

	h.handleNonStreamingResponse(state, req, resp)
	return resp, nil
}

func (h *httpHandler) handleNonStreamingResponse(
	state *spanState,
	req *http.Request,
	resp *http.Response,
) {
	defer state.span.End()
	defer h.inst.recordDuration(state.ctx, state.start, state.baseAttrs)

	if resp.Body == nil {
		return
	}

	// Read a bounded preview for parsing, but reassemble the full body for callers.
	var buf bytes.Buffer
	tee := io.TeeReader(resp.Body, &buf)
	bodyBytes, err := io.ReadAll(io.LimitReader(tee, maxResponseBodySize))
	// Reassemble: preview bytes + remaining unread body.
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(&buf, resp.Body), resp.Body}
	if err != nil {
		return
	}

	info, err := h.adapter.ParseResponse(Call{
		Phase:      PhaseResponse,
		Op:         state.op,
		Method:     req.Method,
		Host:       req.URL.Host,
		Path:       req.URL.Path,
		Header:     resp.Header,
		Body:       bodyBytes,
		StatusCode: resp.StatusCode,
	})
	if err != nil {
		return
	}

	state.span.SetAttributes(responseAttributes(info)...)
	h.inst.recordTokens(state.ctx, info.Usage, state.baseAttrs)
}
