// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package v3

import (
	"net/http"

	"go.opentelemetry.io/otelc/instrumentation/github.com/openai/openai-go/v3/adapter"
	"go.opentelemetry.io/otelc/pkg/genai"
	"go.opentelemetry.io/otelc/pkg/runtime"
)

// OtelMiddleware returns an HTTP middleware that creates spans for OpenAI API
// calls following GenAI semantic conventions.
//
// Everything this used to do itself — the host-to-provider table, the span
// lifecycle, the bounded body reads, the SSE reassembly and the semconv
// helpers — now lives in pkg/genai and is shared with every other provider.
// What remains here is the OpenAI-specific parsing, in the adapter package,
// and the scope this module reports under.
//
// The tracer is passed in rather than resolved from the global provider so
// the package-level variable the hook initialises stays the single place a
// test or the runtime can redirect it. The meter has no such variable, so the
// core resolves it from the global provider under the scope named below.
func OtelMiddleware() func(*http.Request, func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	return genai.HTTPMiddleware(adapter.New(),
		genai.WithTracer(tracer),
		genai.WithScope(instrumentationName, runtime.ModuleVersion()),
	)
}
