package health

import (
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// A project whose selection policy scopes on finality flips the tracker into
// 4-key mode. Every Record* then has to fan out to the per-finality bucket AND
// the rollups the rest of the eval reads. A missing rollup starves the policy;
// an extra bucket double-counts the same request. Both change routing.

// countUpsKeys reports how many (upstream, method, finality) entries the
// tracker holds. The key count IS the contract getUpsKeys documents.
func countUpsKeys(t *Tracker) int {
	n := 0
	t.upsMetrics.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

func TestGetUpsKeys_FinalityOffWritesOnlyTheAllFinalitiesRollups(t *testing.T) {
	// Default mode. A request carrying a specific finality must still land in
	// exactly two buckets — the per-method and the any-method aggregate.
	// A third bucket here is per-finality cardinality nobody asked for.
	tracker := newSeamTracker(t, "finality-off")
	ups := common.NewFakeUpstream("up1")

	tracker.RecordUpstreamFailure(ups, "eth_getLogs", common.DataFinalityStateFinalized, errors.New("boom"))

	require.Equal(t, 2, countUpsKeys(tracker),
		"finality tracking is off, so only {method, All} and {*, All} may exist")
	require.EqualValues(t, 1, tracker.GetUpstreamMethodMetrics(ups, "eth_getLogs", common.DataFinalityStateAll).ErrorsTotal.Load())
	require.EqualValues(t, 1, tracker.GetUpstreamMethodMetrics(ups, "*", common.DataFinalityStateAll).ErrorsTotal.Load())
}

func TestGetUpsKeys_FinalityOnWritesTheFourBuckets(t *testing.T) {
	tracker := newSeamTracker(t, "finality-on-four")
	tracker.EnableFinalityTracking()
	ups := common.NewFakeUpstream("up1")

	tracker.RecordUpstreamFailure(ups, "eth_getLogs", common.DataFinalityStateFinalized, errors.New("boom"))

	require.Equal(t, 4, countUpsKeys(tracker),
		"a specific finality must populate {m,f}, {m,All}, {*,f} and {*,All}")
	for _, tc := range []struct {
		method   string
		finality common.DataFinalityState
	}{
		{"eth_getLogs", common.DataFinalityStateFinalized},
		{"eth_getLogs", common.DataFinalityStateAll},
		{"*", common.DataFinalityStateFinalized},
		{"*", common.DataFinalityStateAll},
	} {
		require.EqualValues(t, 1,
			tracker.GetUpstreamMethodMetrics(ups, tc.method, tc.finality).ErrorsTotal.Load(),
			"bucket {%s, %s} missed the failure", tc.method, tc.finality)
	}
}

func TestGetUpsKeys_TheRollupSumsAcrossFinalities(t *testing.T) {
	// This is what the 4-key fan-out buys: the all-finalities rollup stays a
	// complete picture while each finality keeps its own count. If the rollup
	// key were dropped from the fan-out, a finality-scoped project would read
	// a partial error rate on every method.
	tracker := newSeamTracker(t, "finality-rollup")
	tracker.EnableFinalityTracking()
	ups := common.NewFakeUpstream("up1")

	tracker.RecordUpstreamFailure(ups, "eth_getLogs", common.DataFinalityStateFinalized, errors.New("boom"))
	tracker.RecordUpstreamFailure(ups, "eth_getLogs", common.DataFinalityStateFinalized, errors.New("boom"))
	tracker.RecordUpstreamFailure(ups, "eth_getLogs", common.DataFinalityStateUnfinalized, errors.New("boom"))

	require.EqualValues(t, 2, tracker.GetUpstreamMethodMetrics(ups, "eth_getLogs", common.DataFinalityStateFinalized).ErrorsTotal.Load())
	require.EqualValues(t, 1, tracker.GetUpstreamMethodMetrics(ups, "eth_getLogs", common.DataFinalityStateUnfinalized).ErrorsTotal.Load())
	require.EqualValues(t, 3, tracker.GetUpstreamMethodMetrics(ups, "eth_getLogs", common.DataFinalityStateAll).ErrorsTotal.Load())
	require.EqualValues(t, 3, tracker.GetUpstreamMethodMetrics(ups, "*", common.DataFinalityStateAll).ErrorsTotal.Load())
}

func TestGetUpsKeys_FinalityOnStillWritesTwoKeysWhenTheCallerPassesAll(t *testing.T) {
	// `All` from the caller means "no specific finality known". Expanding it
	// to four keys would write the same aggregate twice and count the request
	// twice in the {*, All} bucket the network-scope policy reads.
	tracker := newSeamTracker(t, "finality-on-all")
	tracker.EnableFinalityTracking()
	ups := common.NewFakeUpstream("up1")

	tracker.RecordUpstreamFailure(ups, "eth_getLogs", common.DataFinalityStateAll, errors.New("boom"))

	require.Equal(t, 2, countUpsKeys(tracker))
	require.EqualValues(t, 1, tracker.GetUpstreamMethodMetrics(ups, "*", common.DataFinalityStateAll).ErrorsTotal.Load(),
		"an All-finality request must be counted once, not twice")
}

func TestGetUpstreamMethodMetrics_FallsBackToTheRollupForAnUnseenFinality(t *testing.T) {
	// The policy engine evaluates every (method, finality) its scope names,
	// including combinations no request has produced yet. Handing it a fresh
	// empty bucket would read as "this upstream has a perfect record", and it
	// would win the selection on no evidence.
	tracker := newSeamTracker(t, "finality-fallback")
	tracker.EnableFinalityTracking()
	ups := common.NewFakeUpstream("up1")

	tracker.RecordUpstreamFailure(ups, "eth_getLogs", common.DataFinalityStateFinalized, errors.New("boom"))

	unseen := tracker.GetUpstreamMethodMetrics(ups, "eth_getLogs", common.DataFinalityStateRealtime)
	require.EqualValues(t, 1, unseen.ErrorsTotal.Load(),
		"an unseen finality must read the all-finalities rollup, not an empty bucket")
	require.Equal(t, 4, countUpsKeys(tracker),
		"the read must not lazily create a bucket for the unseen finality")
}

func TestGetUpstreamMethodMetrics_ReadingAnEntryKeepsItOutOfTheIdleSweep(t *testing.T) {
	// A method whose writes all go to the wildcard bucket is still read every
	// tick by the policy engine. Without the touch on the read path, the sweep
	// would evict the entry from under the reader and reset its history.
	tracker := newSeamTracker(t, "finality-touch")
	tracker.SetIdleEvictionAfter(time.Hour)
	ups := common.NewFakeUpstream("up1")

	tracker.RecordUpstreamFailure(ups, "eth_getLogs", common.DataFinalityStateAll, errors.New("boom"))
	tm := tracker.GetUpstreamMethodMetrics(ups, "eth_getLogs", common.DataFinalityStateAll)
	before := tm.LastAccessedAtMs.Load()
	require.Positive(t, before)

	// Age the entry past any plausible cutoff, then read it again.
	tm.LastAccessedAtMs.Store(1)
	tracker.GetUpstreamMethodMetrics(ups, "eth_getLogs", common.DataFinalityStateAll)
	require.Greater(t, tm.LastAccessedAtMs.Load(), int64(1),
		"a read must refresh the idle timestamp")
}
