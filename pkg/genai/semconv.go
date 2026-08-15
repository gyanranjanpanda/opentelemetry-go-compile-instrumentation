// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package genai

import (
	"go.opentelemetry.io/otel/attribute"
)

// GenAI attribute keys. This is the single copy that replaces the per-provider
// semconv/genai.go files, which were byte-identical between openai-go v1, v2
// and v3 and differed from anthropic-sdk-go's only by which optional keys each
// happened to need.
const (
	GenAISystemKey                        = attribute.Key("gen_ai.system")
	GenAIOperationNameKey                 = attribute.Key("gen_ai.operation.name")
	GenAIRequestModelKey                  = attribute.Key("gen_ai.request.model")
	GenAIResponseModelKey                 = attribute.Key("gen_ai.response.model")
	GenAIResponseIDKey                    = attribute.Key("gen_ai.response.id")
	GenAIResponseFinishReasonsKey         = attribute.Key("gen_ai.response.finish_reasons")
	GenAIUsageInputTokensKey              = attribute.Key("gen_ai.usage.input_tokens")
	GenAIUsageOutputTokensKey             = attribute.Key("gen_ai.usage.output_tokens")
	GenAIUsageTotalTokensKey              = attribute.Key("gen_ai.usage.total_tokens")
	GenAIUsageCacheReadInputTokensKey     = attribute.Key("gen_ai.usage.cache_read.input_tokens")
	GenAIUsageCacheCreationInputTokensKey = attribute.Key("gen_ai.usage.cache_creation.input_tokens")
	GenAIProviderNameKey                  = attribute.Key("gen_ai.provider.name")
	GenAIRequestMaxTokensKey              = attribute.Key("gen_ai.request.max_tokens")
	GenAIRequestTemperatureKey            = attribute.Key("gen_ai.request.temperature")
	GenAIRequestTopPKey                   = attribute.Key("gen_ai.request.top_p")
	GenAIRequestTopKKey                   = attribute.Key("gen_ai.request.top_k")
	GenAIRequestFrequencyPenaltyKey       = attribute.Key("gen_ai.request.frequency_penalty")
	GenAIRequestPresencePenaltyKey        = attribute.Key("gen_ai.request.presence_penalty")
	GenAIRequestIsStreamKey               = attribute.Key("gen_ai.request.is_stream")
	GenAIResponseTimeToFirstTokenKey      = attribute.Key("gen_ai.response.time_to_first_token")

	// GenAITokenTypeKey splits the gen_ai.client.token.usage histogram.
	GenAITokenTypeKey = attribute.Key("gen_ai.token.type")
)

// Token type values for GenAITokenTypeKey.
const (
	TokenTypeInput  = "input"
	TokenTypeOutput = "output"
)

func GenAISystem(val string) attribute.KeyValue { return GenAISystemKey.String(val) }

func GenAIOperationName(val string) attribute.KeyValue { return GenAIOperationNameKey.String(val) }

func GenAIRequestModel(val string) attribute.KeyValue { return GenAIRequestModelKey.String(val) }

func GenAIResponseModel(val string) attribute.KeyValue { return GenAIResponseModelKey.String(val) }

func GenAIResponseID(val string) attribute.KeyValue { return GenAIResponseIDKey.String(val) }

func GenAIResponseFinishReasons(val []string) attribute.KeyValue {
	return GenAIResponseFinishReasonsKey.StringSlice(val)
}

func GenAIUsageInputTokens(val int64) attribute.KeyValue {
	return GenAIUsageInputTokensKey.Int64(val)
}

func GenAIUsageOutputTokens(val int64) attribute.KeyValue {
	return GenAIUsageOutputTokensKey.Int64(val)
}

func GenAIUsageTotalTokens(val int64) attribute.KeyValue {
	return GenAIUsageTotalTokensKey.Int64(val)
}

func GenAIUsageCacheReadInputTokens(val int64) attribute.KeyValue {
	return GenAIUsageCacheReadInputTokensKey.Int64(val)
}

func GenAIUsageCacheCreationInputTokens(val int64) attribute.KeyValue {
	return GenAIUsageCacheCreationInputTokensKey.Int64(val)
}

func GenAIProviderName(val string) attribute.KeyValue { return GenAIProviderNameKey.String(val) }

func GenAIRequestMaxTokens(val int64) attribute.KeyValue {
	return GenAIRequestMaxTokensKey.Int64(val)
}

func GenAIRequestTemperature(val float64) attribute.KeyValue {
	return GenAIRequestTemperatureKey.Float64(val)
}

func GenAIRequestTopP(val float64) attribute.KeyValue { return GenAIRequestTopPKey.Float64(val) }

func GenAIRequestTopK(val int64) attribute.KeyValue { return GenAIRequestTopKKey.Int64(val) }

func GenAIRequestFrequencyPenalty(val float64) attribute.KeyValue {
	return GenAIRequestFrequencyPenaltyKey.Float64(val)
}

func GenAIRequestPresencePenalty(val float64) attribute.KeyValue {
	return GenAIRequestPresencePenaltyKey.Float64(val)
}

func GenAIRequestIsStream(val bool) attribute.KeyValue { return GenAIRequestIsStreamKey.Bool(val) }

func GenAIResponseTimeToFirstToken(microseconds int64) attribute.KeyValue {
	return GenAIResponseTimeToFirstTokenKey.Int64(microseconds)
}

// requestAttributes assembles the optional gen_ai.request.* attributes an
// adapter reported. Emission order is fixed so span attribute order stays
// stable across runs and matches the order the per-provider middleware
// produced.
func requestAttributes(info RequestInfo) []attribute.KeyValue {
	const maxOptionalRequestAttrs = 6
	attrs := make([]attribute.KeyValue, 0, maxOptionalRequestAttrs)
	if info.MaxTokens != nil {
		attrs = append(attrs, GenAIRequestMaxTokens(*info.MaxTokens))
	}
	if info.Temperature != nil {
		attrs = append(attrs, GenAIRequestTemperature(*info.Temperature))
	}
	if info.TopP != nil {
		attrs = append(attrs, GenAIRequestTopP(*info.TopP))
	}
	if info.TopK != nil {
		attrs = append(attrs, GenAIRequestTopK(*info.TopK))
	}
	if info.FrequencyPenalty != nil {
		attrs = append(attrs, GenAIRequestFrequencyPenalty(*info.FrequencyPenalty))
	}
	if info.PresencePenalty != nil {
		attrs = append(attrs, GenAIRequestPresencePenalty(*info.PresencePenalty))
	}
	// info.Messages is deliberately not emitted.
	// gated at emission; see proposal §4.5
	return attrs
}

// responseAttributes assembles the gen_ai.response.* and gen_ai.usage.*
// attributes an adapter reported, honouring the presence rules documented on
// [ResponseInfo].
func responseAttributes(info ResponseInfo) []attribute.KeyValue {
	const maxResponseAttrs = 8
	attrs := make([]attribute.KeyValue, 0, maxResponseAttrs)
	if info.ID != "" {
		attrs = append(attrs, GenAIResponseID(info.ID))
	}
	if info.Model != "" {
		attrs = append(attrs, GenAIResponseModel(info.Model))
	}
	if info.FinishReasons != nil {
		attrs = append(attrs, GenAIResponseFinishReasons(info.FinishReasons))
	}
	if info.Usage.InputTokens != nil {
		attrs = append(attrs, GenAIUsageInputTokens(*info.Usage.InputTokens))
	}
	if info.Usage.OutputTokens != nil {
		attrs = append(attrs, GenAIUsageOutputTokens(*info.Usage.OutputTokens))
	}
	if info.Usage.TotalTokens != nil {
		attrs = append(attrs, GenAIUsageTotalTokens(*info.Usage.TotalTokens))
	}
	return append(attrs, extensionAttributes(info.Ext)...)
}

// extensionAttributes emits the extensions the core knows. Any other
// implementation of [Extension] is ignored, so a newer adapter paired with an
// older core degrades to dropping the unknown facts rather than failing.
func extensionAttributes(ext Extension) []attribute.KeyValue {
	cache, ok := ext.(CacheUsage)
	if !ok {
		return nil
	}
	const maxCacheAttrs = 2
	attrs := make([]attribute.KeyValue, 0, maxCacheAttrs)
	if cache.ReadInputTokens > 0 {
		attrs = append(attrs, GenAIUsageCacheReadInputTokens(cache.ReadInputTokens))
	}
	if cache.CreationInputTokens > 0 {
		attrs = append(attrs, GenAIUsageCacheCreationInputTokens(cache.CreationInputTokens))
	}
	return attrs
}
