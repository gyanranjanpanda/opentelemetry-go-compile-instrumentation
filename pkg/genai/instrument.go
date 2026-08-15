// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package genai

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"go.opentelemetry.io/otelc/pkg/runtime"
)

const (
	// MetricOperationDuration is the GenAI client operation duration histogram.
	MetricOperationDuration = "gen_ai.client.operation.duration"
	// MetricTokenUsage is the GenAI client token usage histogram.
	MetricTokenUsage = "gen_ai.client.token.usage" //nolint:gosec // a metric name, not a credential
)

// Option configures the instrumentation a middleware is built on.
type Option interface{ apply(*config) }

type config struct {
	tracer       trace.Tracer
	meter        metric.Meter
	scopeName    string
	scopeVersion string
}

type optionFunc func(*config)

func (f optionFunc) apply(c *config) { f(c) }

// WithScope sets the instrumentation scope used when the tracer and meter are
// resolved from the global providers. Each provider module passes its own
// name so scope-level attribution survives the move to a shared core.
func WithScope(name, version string) Option {
	return optionFunc(func(c *config) {
		c.scopeName = name
		c.scopeVersion = version
	})
}

// WithTracer supplies a tracer directly. A nil tracer is ignored, so callers
// can pass a package-level variable that may not be initialised yet.
func WithTracer(t trace.Tracer) Option {
	return optionFunc(func(c *config) {
		if t != nil {
			c.tracer = t
		}
	})
}

// WithMeter supplies a meter directly. A nil meter is ignored.
func WithMeter(m metric.Meter) Option {
	return optionFunc(func(c *config) {
		if m != nil {
			c.meter = m
		}
	})
}

// instrumentation holds the OpenTelemetry handles a middleware needs. It is
// built once per middleware construction, not once per request.
type instrumentation struct {
	tracer            trace.Tracer
	operationDuration metric.Float64Histogram
	tokenUsage        metric.Int64Histogram
}

func newInstrumentation(opts ...Option) *instrumentation {
	cfg := config{
		scopeName:    "go.opentelemetry.io/otelc/pkg/genai",
		scopeVersion: runtime.ModuleVersion(),
	}
	for _, opt := range opts {
		opt.apply(&cfg)
	}

	if cfg.tracer == nil {
		cfg.tracer = otel.GetTracerProvider().Tracer(
			cfg.scopeName,
			trace.WithInstrumentationVersion(cfg.scopeVersion),
		)
	}
	if cfg.meter == nil {
		cfg.meter = otel.GetMeterProvider().Meter(
			cfg.scopeName,
			metric.WithInstrumentationVersion(cfg.scopeVersion),
		)
	}

	inst := &instrumentation{tracer: cfg.tracer}
	logger := runtime.Logger()

	var err error
	inst.operationDuration, err = cfg.meter.Float64Histogram(
		MetricOperationDuration,
		metric.WithDescription("Duration of GenAI client operations."),
		metric.WithUnit("s"),
	)
	if err != nil {
		logger.Error("failed to create "+MetricOperationDuration+" histogram", "error", err)
		inst.operationDuration = nil
	}

	inst.tokenUsage, err = cfg.meter.Int64Histogram(
		MetricTokenUsage,
		metric.WithDescription("Number of input and output tokens used by GenAI client operations."),
		metric.WithUnit("{token}"),
	)
	if err != nil {
		logger.Error("failed to create "+MetricTokenUsage+" histogram", "error", err)
		inst.tokenUsage = nil
	}

	return inst
}

// recordDuration records one observation of the operation duration histogram.
// It is called from each site that ends a span, rather than from a deferred
// call at span start, so a streamed call is measured to the end of the stream
// rather than to the middleware's return.
func (i *instrumentation) recordDuration(
	ctx context.Context,
	start time.Time,
	attrs []attribute.KeyValue,
) {
	if i.operationDuration == nil {
		return
	}
	i.operationDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
}

// recordTokens records the input and output token counts an operation
// reported, split by gen_ai.token.type. Counts the adapter left unset are not
// recorded, so an embeddings call contributes no output-token observation
// rather than a zero.
func (i *instrumentation) recordTokens(
	ctx context.Context,
	usage Usage,
	attrs []attribute.KeyValue,
) {
	if i.tokenUsage == nil {
		return
	}
	record := func(tokenType string, count *int64) {
		if count == nil {
			return
		}
		withType := make([]attribute.KeyValue, 0, len(attrs)+1)
		withType = append(withType, attrs...)
		withType = append(withType, GenAITokenTypeKey.String(tokenType))
		i.tokenUsage.Record(ctx, *count, metric.WithAttributes(withType...))
	}
	record(TokenTypeInput, usage.InputTokens)
	record(TokenTypeOutput, usage.OutputTokens)
}
