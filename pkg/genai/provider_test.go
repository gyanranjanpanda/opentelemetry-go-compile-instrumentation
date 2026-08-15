// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package genai

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProviderName(t *testing.T) {
	tests := []struct {
		host     string
		fallback ProviderID
		expected string
	}{
		{"api.openai.com", "openai", "openai"},
		{"myendpoint.azure.com", "openai", "azure"},
		{"api.deepseek.com", "openai", "deepseek"},
		{"dashscope.aliyuncs.com", "openai", "qwen"},
		{"api.groq.com", "openai", "groq"},
		{"localhost:11434", "openai", "local"},
		{"127.0.0.1:8080", "openai", "local"},
		{"api.anthropic.com", "anthropic", "anthropic"},
		// The fallback is what used to differ between the two per-provider
		// tables; it now comes from the adapter's own ProviderID.
		{"custom-api.example.com", "openai", "openai"},
		{"custom-api.example.com", "anthropic", "anthropic"},
	}

	for _, tt := range tests {
		t.Run(tt.host+"/"+string(tt.fallback), func(t *testing.T) {
			assert.Equal(t, tt.expected, ProviderName(tt.host, tt.fallback))
		})
	}
}

// TestProviderName_AmbiguousHostIsDeterministic guards against a regression to
// a map-based provider table: when a host matches more than one keyword, the
// result must always be the earliest match in declaration order, not whichever
// keyword a randomized map iteration happens to hit first. Carried over from
// the openai-go instrumentation's own regression test for Issue #824.
func TestProviderName_AmbiguousHostIsDeterministic(t *testing.T) {
	host := "litellm-gateway.mistral-together-proxy.internal"

	for range 50 {
		assert.Equal(t, "together", ProviderName(host, "openai"))
	}
}
