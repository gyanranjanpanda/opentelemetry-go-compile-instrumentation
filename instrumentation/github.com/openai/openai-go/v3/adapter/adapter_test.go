// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package adapter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/otelc/pkg/genai"
)

// The classification and parsing cases below are carried over from the
// middleware_test.go this package replaces, retargeted at the adapter API.
// The provider-table cases moved with the table itself, into pkg/genai.

func TestClassify(t *testing.T) {
	tests := []struct {
		path     string
		expected genai.Operation
	}{
		{"/v1/chat/completions", genai.OperationChat},
		{"/openai/deployments/gpt-4/chat/completions", genai.OperationChat},
		{"/v1/completions", genai.OperationTextCompletion},
		{"/v1/embeddings", genai.OperationEmbeddings},
		{"/v1/models", genai.OperationUnknown},
		{"/v1/files", genai.OperationUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.expected, New().Classify(genai.Call{Path: tt.path}))
		})
	}
}

// TestClassifyNeedsNoBody pins a contract detail: the core calls Classify
// before it reads a single byte of the request body, so classification has to
// be decidable from the request line alone.
func TestClassifyNeedsNoBody(t *testing.T) {
	call := genai.Call{Phase: genai.PhaseRequest, Path: "/v1/chat/completions"}
	require.Nil(t, call.Body)
	assert.Equal(t, genai.OperationChat, New().Classify(call))
}

func TestParseRequest_Chat(t *testing.T) {
	body := []byte(`{"model":"gpt-4","max_tokens":100,"temperature":0.7,"top_p":0.9,` +
		`"frequency_penalty":0.5,"presence_penalty":0.3}`)

	info, err := New().ParseRequest(genai.Call{Op: genai.OperationChat, Body: body})
	require.NoError(t, err)

	assert.Equal(t, "gpt-4", info.Model)
	assert.Equal(t, int64(100), *info.MaxTokens)
	assert.InDelta(t, 0.7, *info.Temperature, 0.001)
	assert.InDelta(t, 0.9, *info.TopP, 0.001)
	assert.InDelta(t, 0.5, *info.FrequencyPenalty, 0.001)
	assert.InDelta(t, 0.3, *info.PresencePenalty, 0.001)
	assert.Nil(t, info.TopK, "OpenAI has no top_k; the field must stay unset rather than zero")
}

func TestParseRequest_ChatInvalidJSON(t *testing.T) {
	info, err := New().ParseRequest(genai.Call{Op: genai.OperationChat, Body: []byte("invalid json")})
	require.Error(t, err)
	assert.Empty(t, info.Model)
}

// TestParseRequest_ChatMissingModel covers the other pass-through trigger:
// valid JSON with no model is not an error, but the empty Model tells the core
// not to trace.
func TestParseRequest_ChatMissingModel(t *testing.T) {
	info, err := New().ParseRequest(genai.Call{Op: genai.OperationChat, Body: []byte(`{"max_tokens":10}`)})
	require.NoError(t, err)
	assert.Empty(t, info.Model)
}

func TestParseRequest_Messages(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []genai.Message
	}{
		{
			name: "plain string content",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`,
			want: []genai.Message{{Role: "user", Content: `"hello"`}},
		},
		{
			// A multimodal request must not become a parse failure, which the
			// core would read as "do not trace".
			name: "multimodal content parts",
			body: `{"model":"gpt-4","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			want: []genai.Message{{Role: "user", Content: `[{"type":"text","text":"hi"}]`}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := New().ParseRequest(genai.Call{Op: genai.OperationChat, Body: []byte(tt.body)})
			require.NoError(t, err)
			assert.Equal(t, "gpt-4", info.Model)
			assert.Equal(t, tt.want, info.Messages)
		})
	}
}

func TestParseRequest_Completion(t *testing.T) {
	body := []byte(`{"model":"gpt-3.5-turbo-instruct","max_tokens":50,"prompt":"once upon a time"}`)

	info, err := New().ParseRequest(genai.Call{Op: genai.OperationTextCompletion, Body: body})
	require.NoError(t, err)

	assert.Equal(t, "gpt-3.5-turbo-instruct", info.Model)
	assert.Equal(t, int64(50), *info.MaxTokens)
	assert.Equal(t, []genai.Message{{Role: "user", Content: "once upon a time"}}, info.Messages)
}

func TestParseRequest_Embedding(t *testing.T) {
	body := []byte(`{"model":"text-embedding-ada-002","input":"hello"}`)

	info, err := New().ParseRequest(genai.Call{Op: genai.OperationEmbeddings, Body: body})
	require.NoError(t, err)
	assert.Equal(t, "text-embedding-ada-002", info.Model)
}

func TestParseRequest_Stream(t *testing.T) {
	info, err := New().ParseRequest(genai.Call{
		Op:   genai.OperationChat,
		Body: []byte(`{"model":"gpt-4","stream":true}`),
	})
	require.NoError(t, err)
	assert.True(t, info.Stream)
}

func TestParseResponse_Chat(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-123","model":"gpt-4","choices":[{"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`)

	info, err := New().ParseResponse(genai.Call{Op: genai.OperationChat, Body: body})
	require.NoError(t, err)

	assert.Equal(t, "chatcmpl-123", info.ID)
	assert.Equal(t, "gpt-4", info.Model)
	assert.Equal(t, []string{"stop"}, info.FinishReasons)
	assert.Equal(t, int64(10), *info.Usage.InputTokens)
	assert.Equal(t, int64(20), *info.Usage.OutputTokens)
	assert.Equal(t, int64(30), *info.Usage.TotalTokens)
}

// TestParseResponse_ChatReportsEmptyFinishReasons is the presence rule that
// keeps a response with no choices emitting an empty finish-reasons attribute
// rather than none, as the previous middleware did.
func TestParseResponse_ChatReportsEmptyFinishReasons(t *testing.T) {
	info, err := New().ParseResponse(genai.Call{
		Op:   genai.OperationChat,
		Body: []byte(`{"id":"x","model":"gpt-4","choices":[]}`),
	})
	require.NoError(t, err)

	require.NotNil(t, info.FinishReasons, "chat always reports finish reasons as a concept")
	assert.Empty(t, info.FinishReasons)
	assert.Equal(t, int64(0), *info.Usage.InputTokens, "zero counts are still reported")
}

// TestParseResponse_EmbeddingsOmitsWhatItHasNo covers the opposite half:
// embeddings has no response id, no finish reasons and no output tokens, so
// those stay unset and the core emits nothing for them.
func TestParseResponse_EmbeddingsOmitsWhatItHasNo(t *testing.T) {
	body := []byte(`{"model":"text-embedding-ada-002","usage":{"prompt_tokens":2,"total_tokens":2}}`)

	info, err := New().ParseResponse(genai.Call{Op: genai.OperationEmbeddings, Body: body})
	require.NoError(t, err)

	assert.Equal(t, "text-embedding-ada-002", info.Model)
	assert.Equal(t, int64(2), *info.Usage.InputTokens)
	assert.Equal(t, int64(2), *info.Usage.TotalTokens)
	assert.Empty(t, info.ID)
	assert.Nil(t, info.FinishReasons)
	assert.Nil(t, info.Usage.OutputTokens)
}

func TestParseResponse_InvalidJSON(t *testing.T) {
	_, err := New().ParseResponse(genai.Call{Op: genai.OperationChat, Body: []byte("not json")})
	require.Error(t, err)
}

func TestParseStreamChunk(t *testing.T) {
	var acc genai.ResponseInfo
	a := New()

	a.ParseStreamChunk([]byte(`{"id":"chatcmpl-stream","model":"gpt-4",`+
		`"choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}`), &acc)
	assert.Equal(t, "chatcmpl-stream", acc.ID)
	assert.Nil(t, acc.Usage.TotalTokens, "no usage in the first chunk")

	a.ParseStreamChunk([]byte(`{"id":"chatcmpl-stream","model":"gpt-4",`+
		`"choices":[{"delta":{"content":" world"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`), &acc)

	assert.Equal(t, "gpt-4", acc.Model)
	assert.Equal(t, []string{"stop"}, acc.FinishReasons)
	assert.Equal(t, int64(5), *acc.Usage.InputTokens)
	assert.Equal(t, int64(2), *acc.Usage.OutputTokens)
	assert.Equal(t, int64(7), *acc.Usage.TotalTokens)
}

// TestParseStreamChunk_LaterChunksDoNotZeroUsage pins the zero guards: usage
// arrives once and every subsequent chunk decodes to zero, so an unguarded
// assignment would wipe the totals back out.
func TestParseStreamChunk_LaterChunksDoNotZeroUsage(t *testing.T) {
	var acc genai.ResponseInfo
	a := New()

	a.ParseStreamChunk([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`), &acc)
	a.ParseStreamChunk([]byte(`{"id":"chatcmpl-stream","choices":[]}`), &acc)

	assert.Equal(t, int64(7), *acc.Usage.TotalTokens)
	assert.Equal(t, "chatcmpl-stream", acc.ID)
}

func TestParseStreamChunk_InvalidJSONIsIgnored(t *testing.T) {
	acc := genai.ResponseInfo{ID: "kept"}
	New().ParseStreamChunk([]byte("not json"), &acc)
	assert.Equal(t, "kept", acc.ID)
}

func TestProvider(t *testing.T) {
	assert.Equal(t, genai.ProviderID("openai"), New().Provider())
}
