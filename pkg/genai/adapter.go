// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package genai implements the provider-agnostic core of otelc's GenAI
// instrumentation: span lifecycle, semantic-convention attribute assembly,
// metrics, and SSE stream reassembly.
//
// Providers plug in through [Adapter]. An adapter is pure parsing: it turns
// bytes on the wire into the neutral structs in this file and never touches
// OpenTelemetry. Everything an adapter needs is declared in this file, and this
// file imports nothing outside the standard library, so an adapter cannot
// accidentally take a dependency on the tracing, attribute, or metric APIs.
package genai

// ProviderID identifies the provider an adapter speaks for. It supplies the
// gen_ai.system attribute and the fallback for gen_ai.provider.name when the
// request host matches no entry in the shared host table.
type ProviderID string

// Operation is the value of the gen_ai.operation.name attribute. The zero
// value means "this is not a GenAI call" and instructs the core not to trace.
type Operation string

const (
	// OperationUnknown tells the core to pass the call through untraced.
	OperationUnknown Operation = ""

	OperationChat           Operation = "chat"
	OperationTextCompletion Operation = "text_completion"
	OperationEmbeddings     Operation = "embeddings"
)

// Phase identifies which observation point a [Call] describes.
type Phase int

const (
	// PhaseRequest is passed to Classify and ParseRequest.
	PhaseRequest Phase = iota
	// PhaseResponse is passed to ParseResponse.
	PhaseResponse
)

// Call is one observation point on a provider API call, stripped of transport
// and OpenTelemetry types. Header is map[string][]string rather than
// http.Header so the contract does not presuppose HTTP attachment; http.Header
// is assignable to it directly.
type Call struct {
	Phase  Phase
	Op     Operation // zero on the Classify call, set on both Parse calls
	Method string
	Host   string
	Path   string
	Header map[string][]string
	Body   []byte

	// StatusCode is set on PhaseResponse calls only.
	StatusCode int
}

// Message is one turn of a conversation.
type Message struct {
	Role    string
	Content string
}

// RequestInfo is what an adapter can read out of a request body.
//
// The optional scalar fields are pointers so an adapter can distinguish "the
// provider did not send this" from "the provider sent zero". The core emits an
// attribute only for the fields that are set.
type RequestInfo struct {
	// Model is required. An empty Model tells the core to pass the call
	// through untraced, matching the behaviour of the per-provider
	// middleware this core replaces.
	Model string

	// Stream reports whether the caller asked for a streamed response.
	Stream bool

	MaxTokens        *int64
	Temperature      *float64
	TopP             *float64
	TopK             *int64
	FrequencyPenalty *float64
	PresencePenalty  *float64

	// Messages is always populated by adapters but never emitted.
	// gated at emission; see proposal §4.5
	Messages []Message

	// Ext carries provider-specific facts. See [Extension].
	Ext Extension
}

// Usage holds token counts. Each field is optional: an operation that has no
// notion of output tokens (embeddings, for instance) leaves OutputTokens nil
// and the core emits no gen_ai.usage.output_tokens attribute for it.
type Usage struct {
	InputTokens  *int64
	OutputTokens *int64
	TotalTokens  *int64
}

// ResponseInfo is what an adapter can read out of a response body.
//
// Presence is the adapter's to declare. ID and Model are emitted when
// non-empty; FinishReasons is emitted when non-nil, so an adapter signals "this
// operation reports finish reasons, and there were none" with an empty non-nil
// slice and "this operation has no such concept" with nil.
type ResponseInfo struct {
	ID            string
	Model         string
	FinishReasons []string
	Usage         Usage

	// Ext carries provider-specific facts. See [Extension].
	Ext Extension
}

// Extension carries provider-specific facts that have no field on RequestInfo
// or ResponseInfo. The core type-switches on the extensions it knows how to
// emit and silently ignores every other implementation, so an adapter may
// return an extension the running core has never heard of.
type Extension interface {
	// GenAIExtension is a marker. It is exported so adapters outside this
	// module can satisfy the interface.
	GenAIExtension()
}

// CacheUsage reports prompt-cache token counts. Providers that support prompt
// caching (Anthropic, for one) return it from ParseResponse; the core emits
// each count only when it is non-zero.
type CacheUsage struct {
	ReadInputTokens     int64
	CreationInputTokens int64
}

// GenAIExtension implements [Extension].
func (CacheUsage) GenAIExtension() {}

// Adapter is the provider contract. Implementations parse bytes and classify
// paths; they do not create spans, assemble attributes, or record metrics.
type Adapter interface {
	// Provider returns the provider this adapter speaks for.
	Provider() ProviderID

	// Classify maps a request to an operation. Returning OperationUnknown
	// tells the core not to trace the call at all. The Call passed to
	// Classify carries no Body: classification must be decidable from the
	// request line and headers, before the core reads any payload.
	Classify(call Call) Operation

	// ParseRequest extracts the request facts the core needs. Returning an
	// error, or a RequestInfo with an empty Model, makes the core pass the
	// call through untraced rather than emit a partial span.
	ParseRequest(call Call) (RequestInfo, error)

	// ParseResponse extracts the response facts the core needs. An error
	// leaves the span with only its request attributes.
	ParseResponse(call Call) (ResponseInfo, error)
}

// StreamAdapter is an optional extension to [Adapter] for providers that
// return server-sent events. The core owns SSE framing — it splits lines,
// strips the "data: " prefix, and recognises the terminating "[DONE]" sentinel
// — and hands each payload here to be merged into the accumulating response.
//
// An adapter that does not implement StreamAdapter still gets a span for a
// streamed call, carrying the request attributes and gen_ai.request.is_stream,
// but no response attributes.
type StreamAdapter interface {
	Adapter

	// ParseStreamChunk merges one SSE payload into acc. It is called once
	// per data line, in order, from the goroutine reading the response
	// body. Fields absent from a chunk must be left alone rather than
	// zeroed, since later chunks typically carry the usage totals.
	ParseStreamChunk(payload []byte, acc *ResponseInfo)
}

// Int64 returns a pointer to v, for populating the optional fields of
// [RequestInfo] and [Usage].
func Int64(v int64) *int64 { return &v }

// Float64 returns a pointer to v, for populating the optional fields of
// [RequestInfo].
func Float64(v float64) *float64 { return &v }
