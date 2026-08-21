package health

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Everything below is read by the selection policy or by an operator's
// dashboard. The sliding window is what makes a degradation visible within a
// fraction of the window instead of at a tumble cliff, and the cordon flag is
// the one signal that a tick of the clock must never clear.

func TestRollingCounter_WipeClearsTheWholeWindowAndTheHead(t *testing.T) {
	c := NewRollingCounter()
	// Spread the samples over every bucket, so a Wipe that clears only some of
	// them still leaves a non-zero window.
	for i := 0; i < rollingBuckets; i++ {
		c.Add(int64(i + 1))
		c.RotateOldest()
	}
	require.Positive(t, c.Load(), "sanity: the window holds samples")

	c.Wipe()
	assert.EqualValues(t, 0, c.Load(), "Wipe must clear EVERY bucket, not just the newest")

	// The counter stays usable afterwards.
	c.Add(7)
	assert.EqualValues(t, 7, c.Load())
}

func TestRollingCounter_StoreReplacesTheWindowWithOneValue(t *testing.T) {
	c := NewRollingCounter()
	c.Add(3)
	c.RotateOldest()
	c.Add(4)
	require.EqualValues(t, 7, c.Load())

	c.Store(42)
	assert.EqualValues(t, 42, c.Load(), "Store must replace the window, not add to it")

	// The value must land in the NEWEST bucket so it survives the next
	// rotation; a value in the oldest bucket would disappear on the next tick.
	c.RotateOldest()
	assert.EqualValues(t, 42, c.Load())

	c.Add(1)
	assert.EqualValues(t, 43, c.Load())
}

func TestTrackedMetrics_ResetClearsTheCordonButRotateDoesNot(t *testing.T) {
	lg := zerolog.Nop()
	m := newTrackedMetrics(&lg)
	m.RequestsTotal.Add(10)
	m.ErrorsTotal.Add(4)
	m.RemoteRateLimitedTotal.Add(2)
	m.MisbehaviorsTotal.Add(1)
	m.ResponseQuantiles.Add(0.5)
	m.Cordoned.Store(true)
	m.LastCordonedReason.Store("probe says it is behind")

	// Rotate is the request path. Cordoning is the strongest "do not use"
	// signal there is; clearing it on a timer would silently put a bad node
	// back into rotation.
	m.Rotate()
	assert.True(t, m.Cordoned.Load(), "a rotation must never lift a cordon")
	assert.Equal(t, "probe says it is behind", m.LastCordonedReason.Load())

	// Reset is the admin path and clears everything.
	m.Reset()
	assert.EqualValues(t, 0, m.RequestsTotal.Load())
	assert.EqualValues(t, 0, m.ErrorsTotal.Load())
	assert.EqualValues(t, 0, m.RemoteRateLimitedTotal.Load())
	assert.EqualValues(t, 0, m.MisbehaviorsTotal.Load())
	assert.False(t, m.Cordoned.Load())
	assert.Equal(t, "", m.LastCordonedReason.Load())
	assert.EqualValues(t, 0, m.ErrorRate(), "no requests means no error rate, not a divide by zero")
	assert.EqualValues(t, 0, m.ThrottledRate())
	assert.EqualValues(t, 0, m.MisbehaviorRate())
	assert.Zero(t, m.ResponseQuantiles.GetQuantile(0.5), "Reset must empty the latency sketch too")
}

func TestTrackedMetrics_GetResponseQuantilesExposesTheLiveSketch(t *testing.T) {
	lg := zerolog.Nop()
	m := newTrackedMetrics(&lg)
	q := m.GetResponseQuantiles()
	require.NotNil(t, q)

	// The policy engine scores on this handle. A copy would score an empty
	// sketch and rank every upstream identically.
	m.ResponseQuantiles.Add(0.250)
	assert.InDelta(t, 250*time.Millisecond, q.GetQuantile(0.5), float64(20*time.Millisecond))
}

func TestTracker_GetNetworkMethodMetricsIsStableForOneKey(t *testing.T) {
	tracker := newSeamTracker(t, "ntw-metrics")

	m := tracker.GetNetworkMethodMetrics("evm:123", "eth_call")
	require.NotNil(t, m, "a network/method pair must always resolve to a bucket")
	m.RequestsTotal.Add(3)

	// The same key must return the SAME bucket, or every read starts a new
	// window and the network never accumulates a rate.
	again := tracker.GetNetworkMethodMetrics("evm:123", "eth_call")
	assert.Same(t, m, again)
	assert.EqualValues(t, 3, again.RequestsTotal.Load())

	other := tracker.GetNetworkMethodMetrics("evm:123", "eth_getLogs")
	assert.NotSame(t, m, other, "two methods must not share one bucket")
	assert.EqualValues(t, 0, other.RequestsTotal.Load())
}

func TestTracker_TimerFeedsTheLatencySketchOnlyOnSuccess(t *testing.T) {
	// A failing upstream answers fast. Letting its failures into the quantile
	// would crown it the fastest in the pool and route everything to it.
	tracker := newSeamTracker(t, "timer")
	ups := common.NewFakeUpstream("u1")

	timer := tracker.RecordUpstreamDurationStart(ups, "eth_call", "", common.DataFinalityStateUnknown, "")
	require.NotNil(t, timer)
	time.Sleep(5 * time.Millisecond)
	timer.ObserveDuration(false)

	q := tracker.GetUpstreamMethodMetrics(ups, "eth_call", common.DataFinalityStateUnknown).ResponseQuantiles
	require.Zero(t, q.GetQuantile(0.5), "a failed call must not enter the latency sketch")

	timer = tracker.RecordUpstreamDurationStart(ups, "eth_call", "", common.DataFinalityStateUnknown, "")
	time.Sleep(5 * time.Millisecond)
	timer.ObserveDuration(true)

	assert.Positive(t, q.GetQuantile(0.5), "a successful call must be measured")
}

func TestTracker_RecordBlockHeadLargeRollbackKeepsTheAxesApart(t *testing.T) {
	// Two pollers report rollbacks for one upstream — the latest head and the
	// finalized head. Sharing one gauge would let each overwrite the other, and
	// a reorg on one axis would read as a reorg on both.
	tracker := newSeamTracker(t, "rollback")
	ups := common.NewFakeUpstream("u1")

	gaugeFor := func(finality string) float64 {
		return testutil.ToFloat64(telemetry.MetricUpstreamBlockHeadLargeRollback.WithLabelValues(
			"rollback", ups.VendorName(), ups.NetworkLabel(), ups.Id(), finality))
	}

	tracker.RecordBlockHeadLargeRollback(ups, "latest", 1000, 940)
	tracker.RecordBlockHeadLargeRollback(ups, "finalized", 900, 899)

	assert.EqualValues(t, 60, gaugeFor("latest"), "the latest-head rollback depth must be reported")
	assert.EqualValues(t, 1, gaugeFor("finalized"), "the finalized-head axis has its own gauge")

	// The gauge is cached per key; a later rollback must reuse it and report
	// the NEW depth rather than accumulating.
	tracker.RecordBlockHeadLargeRollback(ups, "latest", 2000, 1995)
	assert.EqualValues(t, 5, gaugeFor("latest"))
	assert.EqualValues(t, 1, gaugeFor("finalized"), "one axis's rollback must not disturb the other")
}
