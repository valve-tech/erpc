package common

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// tracingHarness installs an in-memory span recorder as the process tracer, so
// a test can read back exactly what an operator would receive: the span names,
// the attributes, the status, and whether the span ended at all.
//
// Every tracing global the harness touches is captured and restored on
// cleanup. Tests that use it must NOT call t.Parallel(), because the globals
// are process-wide.
type tracingHarness struct {
	t        *testing.T
	recorder *tracetest.SpanRecorder
	provider *sdktrace.TracerProvider
}

// newTracingHarness records every span. Use it when the test is about span
// content rather than about the sampling decision.
func newTracingHarness(t *testing.T, detailed bool) *tracingHarness {
	t.Helper()
	return newTracingHarnessWithSampler(t, detailed, sdktrace.AlwaysSample())
}

// newTracingHarnessWithSampler lets a test drive the real sampler, which is the
// only way to prove that force-tracing actually bypasses a NeverSample config.
func newTracingHarnessWithSampler(t *testing.T, detailed bool, sampler sdktrace.Sampler) *tracingHarness {
	t.Helper()

	saveTracingGlobals(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(sr),
		sdktrace.WithSampler(sampler),
	)

	tracer = tp.Tracer(instrumentationName)
	tracerProvider = tp
	IsTracingEnabled = true
	IsTracingDetailed = detailed
	tracingDetailed.Store(detailed)

	// t.Context() is already cancelled by the time cleanup runs, so shut the
	// provider down on a fresh context and let it flush.
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	return &tracingHarness{t: t, recorder: sr, provider: tp}
}

// saveTracingGlobals captures the tracing globals and restores them on cleanup.
// Call it from any test that writes one of them, harness or not.
func saveTracingGlobals(t *testing.T) {
	t.Helper()

	prevTracer := tracer
	prevProvider := tracerProvider
	prevEnabled := IsTracingEnabled
	prevDetailed := IsTracingDetailed
	prevAtomicDetailed := tracingDetailed.Load()
	prevMatchers := forceTraceMatchers

	t.Cleanup(func() {
		tracer = prevTracer
		tracerProvider = prevProvider
		IsTracingEnabled = prevEnabled
		IsTracingDetailed = prevDetailed
		tracingDetailed.Store(prevAtomicDetailed)
		forceTraceMatchers = prevMatchers
	})
}

// resetTracingInitOnce reopens the sync.Once that guards InitializeTracing, so
// a test can drive the initializer body. On cleanup it consumes the Once again,
// which leaves the process in the state SetTracerProviderForTest expects.
func resetTracingInitOnce(t *testing.T) {
	t.Helper()

	initOnce = sync.Once{}
	t.Cleanup(func() {
		initOnce = sync.Once{}
		initOnce.Do(func() {})
	})
}

// ended returns the spans the recorder saw finish.
func (h *tracingHarness) ended() []sdktrace.ReadOnlySpan {
	return h.recorder.Ended()
}

// endedNamed returns the one ended span with the given name, and fails the test
// if the count is not exactly one.
func (h *tracingHarness) endedNamed(name string) sdktrace.ReadOnlySpan {
	h.t.Helper()

	var found []sdktrace.ReadOnlySpan
	for _, s := range h.recorder.Ended() {
		if s.Name() == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		h.t.Fatalf("want exactly 1 ended span named %q, got %d", name, len(found))
	}
	return found[0]
}

// startedButNotEnded reports the names of spans the recorder started and never
// saw end. An unended span is invisible to the operator, so tests assert it is
// empty on every path.
func (h *tracingHarness) startedButNotEnded() []string {
	endedIDs := map[string]bool{}
	for _, s := range h.recorder.Ended() {
		endedIDs[s.SpanContext().SpanID().String()] = true
	}
	var names []string
	for _, s := range h.recorder.Started() {
		if !endedIDs[s.SpanContext().SpanID().String()] {
			names = append(names, s.Name())
		}
	}
	return names
}

// spanAttrs flattens a span's attributes into a lookup map.
func spanAttrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	out := make(map[attribute.Key]attribute.Value, len(s.Attributes()))
	for _, a := range s.Attributes() {
		out[a.Key] = a.Value
	}
	return out
}
