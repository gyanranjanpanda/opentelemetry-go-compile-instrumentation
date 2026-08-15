// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package genai

import (
	"encoding/json"
	"strings"
)

// stubAdapter is a minimal Adapter used to exercise the core without pulling
// in a real provider SDK. It speaks a made-up wire format that is deliberately
// not OpenAI's, so a test passing here says something about the core rather
// than about one provider's JSON.
type stubAdapter struct {
	provider  ProviderID
	classify  func(Call) Operation
	parseReq  func(Call) (RequestInfo, error)
	parseResp func(Call) (ResponseInfo, error)
}

func (s stubAdapter) Provider() ProviderID { return s.provider }

func (s stubAdapter) Classify(call Call) Operation {
	if s.classify != nil {
		return s.classify(call)
	}
	switch {
	case strings.HasSuffix(call.Path, "/chat"):
		return OperationChat
	case strings.HasSuffix(call.Path, "/embed"):
		return OperationEmbeddings
	default:
		return OperationUnknown
	}
}

type stubRequestBody struct {
	Model       string   `json:"model"`
	Stream      bool     `json:"stream"`
	MaxTokens   *int64   `json:"max_tokens"`
	Temperature *float64 `json:"temperature"`
	TopK        *int64   `json:"top_k"`
}

func (s stubAdapter) ParseRequest(call Call) (RequestInfo, error) {
	if s.parseReq != nil {
		return s.parseReq(call)
	}
	var body stubRequestBody
	if err := json.Unmarshal(call.Body, &body); err != nil {
		return RequestInfo{}, err
	}
	return RequestInfo{
		Model:       body.Model,
		Stream:      body.Stream,
		MaxTokens:   body.MaxTokens,
		Temperature: body.Temperature,
		TopK:        body.TopK,
		Messages:    []Message{{Role: "user", Content: "secret prompt"}},
	}, nil
}

type stubResponseBody struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Reason string `json:"reason"`
	In     *int64 `json:"in"`
	Out    *int64 `json:"out"`
	Total  *int64 `json:"total"`
}

func (s stubAdapter) ParseResponse(call Call) (ResponseInfo, error) {
	if s.parseResp != nil {
		return s.parseResp(call)
	}
	var body stubResponseBody
	if err := json.Unmarshal(call.Body, &body); err != nil {
		return ResponseInfo{}, err
	}
	info := ResponseInfo{
		Usage: Usage{InputTokens: body.In, OutputTokens: body.Out, TotalTokens: body.Total},
	}
	if body.ID != "" {
		info.ID = String(body.ID)
	}
	if body.Model != "" {
		info.Model = String(body.Model)
	}
	// Embeddings has no finish-reason concept, so the slice stays nil and the
	// core emits no attribute for it. Chat always reports the field, even
	// when the list is empty.
	if call.Op != OperationEmbeddings {
		info.FinishReasons = []string{}
		if body.Reason != "" {
			info.FinishReasons = append(info.FinishReasons, body.Reason)
		}
	}
	return info, nil
}

// stubStreamAdapter adds SSE chunk handling to stubAdapter.
type stubStreamAdapter struct {
	stubAdapter
	parseChunk func([]byte, *ResponseInfo)
}

func (s stubStreamAdapter) ParseStreamChunk(payload []byte, acc *ResponseInfo) {
	if s.parseChunk != nil {
		s.parseChunk(payload, acc)
		return
	}
	var body stubResponseBody
	if err := json.Unmarshal(payload, &body); err != nil {
		return
	}
	if body.ID != "" {
		acc.ID = String(body.ID)
	}
	if body.Model != "" {
		acc.Model = String(body.Model)
	}
	if body.In != nil {
		acc.Usage.InputTokens = body.In
	}
	if body.Out != nil {
		acc.Usage.OutputTokens = body.Out
	}
	if body.Total != nil {
		acc.Usage.TotalTokens = body.Total
	}
	if body.Reason != "" {
		acc.FinishReasons = append(acc.FinishReasons, body.Reason)
	}
}

// unknownExtension is an Extension the core has never heard of.
type unknownExtension struct{ Note string }

func (unknownExtension) GenAIExtension() {}
