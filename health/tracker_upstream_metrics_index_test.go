package health

import (
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// GetUpstreamMetrics is what the selection policy reads once per upstream per
// tick. It normally answers from the per-network index that request traffic
// builds. Before the first request there is no index, and the poller has
// already written block-head lag into the wildcard bucket. The answer has to
// be the same either way, or an upstream's first eval sees an empty record.

func TestGetUpstreamMetrics_ReturnsPollerDataBeforeAnyRequestTraffic(t *testing.T) {
	// SetLatestBlockNumber writes {*, All} WITHOUT registering an index
	// entry. If the read gave up when the index is empty, a lagging upstream
	// would score as healthy for its whole first eval window.
	tracker := newSeamTracker(t, "index-cold")
	fast := common.NewFakeUpstream("fast")
	slow := common.NewFakeUpstream("slow")

	tracker.SetLatestBlockNumber(fast, 1000, 0)
	tracker.SetLatestBlockNumber(slow, 940, 0)

	out := tracker.GetUpstreamMetrics(slow)
	require.Contains(t, out, "*", "the poller-written wildcard bucket must be visible to the eval")
	require.EqualValues(t, 60, out["*"].BlockHeadLag.Load())
}

func TestGetUpstreamMetrics_IsScopedToTheUpstreamAsked(t *testing.T) {
	// Both the indexed read and the cold fallback filter on upstream id.
	// Returning a peer's buckets would score one node with another's history.
	tracker := newSeamTracker(t, "index-scope")
	a := common.NewFakeUpstream("a")
	b := common.NewFakeUpstream("b")

	tracker.RecordUpstreamFailure(a, "eth_call", common.DataFinalityStateAll, errors.New("boom"))
	tracker.RecordUpstreamFailure(b, "eth_getLogs", common.DataFinalityStateAll, errors.New("boom"))

	out := tracker.GetUpstreamMetrics(a)
	require.Contains(t, out, "eth_call")
	require.NotContains(t, out, "eth_getLogs", "a peer's method must not appear in this upstream's metrics")
}

func TestGetUpstreamMetrics_ReturnsTheAllFinalitiesBucketPerMethod(t *testing.T) {
	// In 4-key mode each method owns several buckets. The eval must see one
	// deterministic record per method — the rollup — not whichever finality
	// the index happened to store.
	tracker := newSeamTracker(t, "index-finality")
	tracker.EnableFinalityTracking()
	ups := common.NewFakeUpstream("up1")

	tracker.RecordUpstreamFailure(ups, "eth_call", common.DataFinalityStateFinalized, errors.New("boom"))
	tracker.RecordUpstreamFailure(ups, "eth_call", common.DataFinalityStateUnfinalized, errors.New("boom"))

	out := tracker.GetUpstreamMetrics(ups)
	require.Contains(t, out, "eth_call")
	require.EqualValues(t, 2, out["eth_call"].ErrorsTotal.Load(),
		"the per-method entry must be the cross-finality rollup, not one finality's slice")
}

func TestGetUpstreamMetrics_ReturnsAnEmptyMapForAnUnknownUpstream(t *testing.T) {
	// The policy engine calls this for every upstream it knows, including one
	// that has never served a request. An empty map is the honest answer; a
	// nil map would make the caller's range panic-free but its length checks
	// wrong, and any invented entry would be fabricated history.
	tracker := newSeamTracker(t, "index-unknown")
	ups := common.NewFakeUpstream("never-used")

	out := tracker.GetUpstreamMetrics(ups)
	require.NotNil(t, out)
	require.Empty(t, out)
}
