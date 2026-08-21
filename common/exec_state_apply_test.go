package common

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// ExecState.Apply is the only thing that puts the per-attempt upstream log into
// a trace. An operator reading a trace asks "which upstreams ran, why, what
// happened, and which one answered". These tests pin that the six parallel
// slices stay aligned and carry the recorded values.

// recordSpanAttrs runs fn against a real recorded span and returns the
// attributes the span ended up with, keyed by attribute name.
func recordSpanAttrs(t *testing.T, fn func(span trace.Span)) map[string]attribute.Value {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, span := tp.Tracer("test").Start(context.Background(), "unit")
	fn(span)
	span.End()

	ended := recorder.Ended()
	require.Len(t, ended, 1)

	attrs := map[string]attribute.Value{}
	for _, kv := range ended[0].Attributes() {
		attrs[string(kv.Key)] = kv.Value
	}
	return attrs
}

func TestExecStateApply_EmitsTheCounterTriplet(t *testing.T) {
	st := &ExecState{StartedAt: time.Now()}
	st.UpstreamAttempts.Add(3)
	st.UpstreamRetries.Add(2)
	st.UpstreamHedges.Add(1)
	st.NetworkAttempts.Add(5)
	st.NetworkRetries.Add(4)
	st.NetworkHedges.Add(6)
	st.CacheAttempts.Add(7)
	st.CacheRetries.Add(8)
	st.CacheHedges.Add(9)

	attrs := recordSpanAttrs(t, st.Apply)

	// Totals follow the documented model: attempts count physical calls only
	// (network rotations are not summed), retries and hedges sum every scope.
	require.Equal(t, int64(3+7), attrs["execution.attempts"].AsInt64())
	require.Equal(t, int64(2+4+8), attrs["execution.retries"].AsInt64())
	require.Equal(t, int64(1+6+9), attrs["execution.hedges"].AsInt64())

	require.Equal(t, int64(3), attrs["execution.upstream_attempts"].AsInt64())
	require.Equal(t, int64(2), attrs["execution.upstream_retries"].AsInt64())
	require.Equal(t, int64(1), attrs["execution.upstream_hedges"].AsInt64())
	require.Equal(t, int64(5), attrs["execution.network_attempts"].AsInt64())
	require.Equal(t, int64(4), attrs["execution.network_retries"].AsInt64())
	require.Equal(t, int64(6), attrs["execution.network_hedges"].AsInt64())
	require.Equal(t, int64(7), attrs["execution.cache_attempts"].AsInt64())
	require.Equal(t, int64(8), attrs["execution.cache_retries"].AsInt64())
	require.Equal(t, int64(9), attrs["execution.cache_hedges"].AsInt64())
}

func TestExecStateApply_EmitsTheUpstreamAttemptLogAsAlignedSlices(t *testing.T) {
	st := &ExecState{StartedAt: time.Now()}
	st.RecordUpstreamAttempt(UpstreamAttempt{
		UpstreamId: "alchemy-1",
		Outcome:    UpstreamOutcomeServerError,
		Reason:     SelectionReasonPrimary,
		Duration:   120 * time.Millisecond,
		Won:        false,
	})
	st.RecordUpstreamAttempt(UpstreamAttempt{
		UpstreamId: "quicknode-2",
		Outcome:    UpstreamOutcomeSuccess,
		Reason:     SelectionReasonRetry,
		Duration:   45 * time.Millisecond,
		Won:        true,
	})

	attrs := recordSpanAttrs(t, st.Apply)

	require.Equal(t, int64(2), attrs["upstreams.attempts"].AsInt64())
	require.Equal(t, []string{"alchemy-1", "quicknode-2"}, attrs["upstreams.tried"].AsStringSlice())
	require.Equal(t, []string{"server_error", "success"}, attrs["upstreams.outcomes"].AsStringSlice())
	require.Equal(t, []string{"primary", "retry"}, attrs["upstreams.reasons"].AsStringSlice())
	require.Equal(t, []int64{120, 45}, attrs["upstreams.durations_ms"].AsInt64Slice())
	require.Equal(t, []bool{false, true}, attrs["upstreams.won"].AsBoolSlice())
}

// TestExecStateApply_OmitsTheLogWhenNoAttemptRan pins the early return: a
// request served entirely from cache emits counters but no empty parallel
// slices, so an operator filtering on upstreams.tried sees nothing rather than
// an empty list.
func TestExecStateApply_OmitsTheLogWhenNoAttemptRan(t *testing.T) {
	st := &ExecState{StartedAt: time.Now()}
	st.CacheAttempts.Add(1)

	attrs := recordSpanAttrs(t, st.Apply)

	require.Contains(t, attrs, "execution.attempts")
	require.NotContains(t, attrs, "upstreams.attempts")
	require.NotContains(t, attrs, "upstreams.tried")
}

// TestExecStateApply_ToleratesNilReceiverAndNilSpan pins the two guards. A
// request that never touched an executor has no ExecState, and a caller
// outside a trace has no span; neither may panic in the response path.
func TestExecStateApply_ToleratesNilReceiverAndNilSpan(t *testing.T) {
	var nilState *ExecState
	require.NotPanics(t, func() { nilState.Apply(nil) })

	st := &ExecState{StartedAt: time.Now()}
	st.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "u1"})
	require.NotPanics(t, func() { st.Apply(nil) })

	require.NotPanics(t, func() {
		recordSpanAttrs(t, func(span trace.Span) { nilState.Apply(span) })
	})
}

// TestExecStateApply_ReflectsAWonMarkSetAfterTheAttempt proves Apply reads the
// log at emit time, not at record time: the executor flags the winner after
// every attempt has already been recorded.
func TestExecStateApply_ReflectsAWonMarkSetAfterTheAttempt(t *testing.T) {
	st := &ExecState{StartedAt: time.Now()}
	st.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "u1", Outcome: UpstreamOutcomeSuccess})
	st.RecordUpstreamAttempt(UpstreamAttempt{UpstreamId: "u2", Outcome: UpstreamOutcomeSuccess})

	st.MarkUpstreamAttemptWon("u2")

	attrs := recordSpanAttrs(t, st.Apply)
	require.Equal(t, []bool{false, true}, attrs["upstreams.won"].AsBoolSlice())
}
