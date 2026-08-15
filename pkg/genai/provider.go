// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package genai

import "strings"

// providerMapping is checked in order: the first entry whose keyword is a
// substring of the request host wins. It is a slice, not a map, because Go
// map iteration order is randomized on every range - a map here would make
// ProviderName non-deterministic whenever a host matched more than one
// keyword (see Issue #824).
//
// Seeded verbatim from the openai-go instrumentation's table, which was the
// larger of the two per-provider copies. anthropic-sdk-go's three entries
// (anthropic.com, localhost, 127.0.0.1) are all already present here; what
// differed between the two was only the fallback, which is now the adapter's
// own ProviderID rather than a hard-coded string.
var providerMapping = []struct { //nolint:gochecknoglobals // private lookup table
	keyword  string
	provider string
}{
	{"openai.com", "openai"},
	{"azure.com", "azure"},
	{"anthropic.com", "anthropic"},
	{"dashscope.aliyuncs", "qwen"},
	{"volces.com", "ark"},
	{"ark.cn", "ark"},
	{"hunyuan", "tencent"},
	{"tencentcloudapi", "tencent"},
	{"googleapis.com", "google"},
	{"generativelanguage", "google"},
	{"deepseek.com", "deepseek"},
	{"moonshot", "moonshot"},
	{"zhipuai.cn", "zhipu"},
	{"bigmodel.cn", "zhipu"},
	{"baidu.com", "baidu"},
	{"minimax", "minimax"},
	{"siliconflow", "siliconflow"},
	{"together", "together"},
	{"mistral", "mistral"},
	{"groq.com", "groq"},
	{"ollama", "ollama"},
	{"localhost", "local"},
	{"127.0.0.1", "local"},
}

// ProviderName resolves the gen_ai.provider.name value for a request host,
// falling back to the calling adapter's own ProviderID when no keyword
// matches. The fallback is what made the two per-provider tables differ:
// openai-go defaulted to "openai" and anthropic-sdk-go to "anthropic".
func ProviderName(host string, fallback ProviderID) string {
	for _, entry := range providerMapping {
		if strings.Contains(host, entry.keyword) {
			return entry.provider
		}
	}
	return string(fallback)
}
