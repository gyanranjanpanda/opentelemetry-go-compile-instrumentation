// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// Package adapter implements the genai.Adapter contract for the OpenAI API
// shape. It is pure parsing: it decodes request and response bodies and
// classifies request paths, and it imports neither the openai-go SDK nor any
// OpenTelemetry package.
//
// Every function here is lifted from the middleware.go this package replaces —
// Classify from classifyOperation, the request and response decoding from
// parseChatRequest and friends, and ParseStreamChunk from the shared
// internal/streaming package's per-operation chunk handlers.
package adapter

import (
	"encoding/json"
	"strings"

	"go.opentelemetry.io/otelc/pkg/genai"
)

// Provider is the gen_ai.system value and the fallback for
// gen_ai.provider.name when a request host matches no entry in the core's
// host table.
const Provider = genai.ProviderID("openai")

// Adapter parses the OpenAI HTTP API.
type Adapter struct{}

// New returns an adapter for the OpenAI API shape.
func New() Adapter { return Adapter{} }

var (
	_ genai.Adapter       = Adapter{}
	_ genai.StreamAdapter = Adapter{}
)

// Provider implements [genai.Adapter].
func (Adapter) Provider() genai.ProviderID { return Provider }

// Classify implements [genai.Adapter]. The suffix ordering matters:
// "chat/completions" has to be tested before "completions", which is a suffix
// of it. Azure serves chat completions under /openai/deployments/<name>/, so
// the match is on the suffix rather than the whole path.
func (Adapter) Classify(call genai.Call) genai.Operation {
	if strings.HasSuffix(call.Path, "chat/completions") {
		return genai.OperationChat
	}
	if strings.HasSuffix(call.Path, "completions") {
		return genai.OperationTextCompletion
	}
	if strings.HasSuffix(call.Path, "embeddings") {
		return genai.OperationEmbeddings
	}
	return genai.OperationUnknown
}

// message mirrors one entry of the request's "messages" array. Content is left
// as raw JSON because the API accepts both a plain string and an array of
// content parts, and a stricter type would turn a multimodal request into a
// parse failure — which the core reads as "do not trace this call".
type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type chatRequest struct {
	Model            string    `json:"model"`
	Stream           bool      `json:"stream,omitempty"`
	MaxTokens        *int64    `json:"max_tokens,omitempty"`
	Temperature      *float64  `json:"temperature,omitempty"`
	TopP             *float64  `json:"top_p,omitempty"`
	FrequencyPenalty *float64  `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64  `json:"presence_penalty,omitempty"`
	Messages         []message `json:"messages,omitempty"`
}

type completionRequest struct {
	Model       string   `json:"model"`
	Stream      bool     `json:"stream,omitempty"`
	MaxTokens   *int64   `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	Prompt      string   `json:"prompt,omitempty"`
}

type embeddingRequest struct {
	Model string `json:"model"`
}

// ParseRequest implements [genai.Adapter].
func (Adapter) ParseRequest(call genai.Call) (genai.RequestInfo, error) {
	switch call.Op {
	case genai.OperationChat:
		return parseChatRequest(call.Body)
	case genai.OperationTextCompletion:
		return parseCompletionRequest(call.Body)
	case genai.OperationEmbeddings:
		return parseEmbeddingRequest(call.Body)
	case genai.OperationUnknown:
	}
	return genai.RequestInfo{}, nil
}

func parseChatRequest(body []byte) (genai.RequestInfo, error) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return genai.RequestInfo{}, err
	}

	messages := make([]genai.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		messages = append(messages, genai.Message{Role: m.Role, Content: string(m.Content)})
	}

	return genai.RequestInfo{
		Model:            req.Model,
		Stream:           req.Stream,
		MaxTokens:        req.MaxTokens,
		Temperature:      req.Temperature,
		TopP:             req.TopP,
		FrequencyPenalty: req.FrequencyPenalty,
		PresencePenalty:  req.PresencePenalty,
		// gated at emission; see proposal §4.5
		Messages: messages,
	}, nil
}

func parseCompletionRequest(body []byte) (genai.RequestInfo, error) {
	var req completionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return genai.RequestInfo{}, err
	}

	var messages []genai.Message
	if req.Prompt != "" {
		// The legacy completions API has a single prompt rather than a
		// message list; represent it as one user turn.
		messages = []genai.Message{{Role: "user", Content: req.Prompt}}
	}

	return genai.RequestInfo{
		Model:       req.Model,
		Stream:      req.Stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		// gated at emission; see proposal §4.5
		Messages: messages,
	}, nil
}

func parseEmbeddingRequest(body []byte) (genai.RequestInfo, error) {
	var req embeddingRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return genai.RequestInfo{}, err
	}
	return genai.RequestInfo{Model: req.Model}, nil
}

// completionResponse covers both the chat and the legacy completion response
// bodies, which carry the same fields for everything the instrumentation
// reads.
type completionResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

type embeddingResponse struct {
	Model string `json:"model"`
	Usage struct {
		PromptTokens int64 `json:"prompt_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
}

// ParseResponse implements [genai.Adapter].
func (Adapter) ParseResponse(call genai.Call) (genai.ResponseInfo, error) {
	switch call.Op {
	case genai.OperationChat, genai.OperationTextCompletion:
		return parseCompletionResponse(call.Body)
	case genai.OperationEmbeddings:
		return parseEmbeddingResponse(call.Body)
	case genai.OperationUnknown:
	}
	return genai.ResponseInfo{}, nil
}

func parseCompletionResponse(body []byte) (genai.ResponseInfo, error) {
	var resp completionResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return genai.ResponseInfo{}, err
	}

	// Non-nil even when empty: chat and text completion always report finish
	// reasons as a concept, so the attribute is always emitted, matching what
	// the middleware this replaces did.
	reasons := []string{}
	for _, c := range resp.Choices {
		if c.FinishReason != "" {
			reasons = append(reasons, c.FinishReason)
		}
	}

	return genai.ResponseInfo{
		// Always set, empty string included: chat and text completion report
		// an id and a model as concepts, so the attributes are always emitted
		// even when the provider left the fields blank. genai.String("") and a
		// nil pointer are deliberately different things.
		ID:            genai.String(resp.ID),
		Model:         genai.String(resp.Model),
		FinishReasons: reasons,
		// All three counts are always reported for these operations, zero
		// included, so all three pointers are always set.
		Usage: genai.Usage{
			InputTokens:  genai.Int64(resp.Usage.PromptTokens),
			OutputTokens: genai.Int64(resp.Usage.CompletionTokens),
			TotalTokens:  genai.Int64(resp.Usage.TotalTokens),
		},
	}, nil
}

func parseEmbeddingResponse(body []byte) (genai.ResponseInfo, error) {
	var resp embeddingResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return genai.ResponseInfo{}, err
	}

	// Embeddings has no response id, no finish reasons and no output tokens.
	// Leaving them unset is what stops the core emitting empty or zero-valued
	// attributes the previous middleware never produced.
	return genai.ResponseInfo{
		Model: genai.String(resp.Model),
		Usage: genai.Usage{
			InputTokens: genai.Int64(resp.Usage.PromptTokens),
			TotalTokens: genai.Int64(resp.Usage.TotalTokens),
		},
	}, nil
}

// streamChunk is the shape of one SSE payload. Chat and text-completion chunks
// differ only in a "delta" field the instrumentation never read, so one decode
// serves both — which is why ParseStreamChunk not being told the operation
// costs nothing here.
type streamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// ParseStreamChunk implements [genai.StreamAdapter]. The zero guards are
// deliberate: usage arrives in a late chunk and every earlier one decodes to
// zero, so an unguarded assignment would keep overwriting the totals back to
// zero.
func (Adapter) ParseStreamChunk(payload []byte, acc *genai.ResponseInfo) {
	var chunk streamChunk
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return
	}

	if chunk.ID != "" {
		acc.ID = genai.String(chunk.ID)
	}
	if chunk.Model != "" {
		acc.Model = genai.String(chunk.Model)
	}
	if chunk.Usage.PromptTokens > 0 {
		acc.Usage.InputTokens = genai.Int64(chunk.Usage.PromptTokens)
	}
	if chunk.Usage.CompletionTokens > 0 {
		acc.Usage.OutputTokens = genai.Int64(chunk.Usage.CompletionTokens)
	}
	if chunk.Usage.TotalTokens > 0 {
		acc.Usage.TotalTokens = genai.Int64(chunk.Usage.TotalTokens)
	}
	for _, c := range chunk.Choices {
		if c.FinishReason != "" {
			acc.FinishReasons = append(acc.FinishReasons, c.FinishReason)
		}
	}
}
