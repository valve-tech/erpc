package health

import (
	"encoding/json"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// Cordoning takes an upstream out of routing. The WRITE path (Cordon /
// Uncordon) already has metric tests; these cover the READ path that
// selection and the admin endpoints call. A read that answers "not cordoned"
// for an upstream an operator has cordoned sends traffic to a node they
// deliberately removed — and gives them no way to see why.

func TestIsCordoned_ReportsAMethodScopedCordon(t *testing.T) {
	tracker := newSeamTracker(t, "cordon-method")
	ups := common.NewFakeUpstream("up1")

	require.False(t, tracker.IsCordoned(ups, "eth_call"), "nothing is cordoned to begin with")

	tracker.Cordon(ups, "eth_call", "returns stale state")
	require.True(t, tracker.IsCordoned(ups, "eth_call"))
	require.False(t, tracker.IsCordoned(ups, "eth_getLogs"),
		"a method-scoped cordon must not remove the upstream from unrelated methods")
}

func TestIsCordoned_AWildcardCordonCoversEveryMethod(t *testing.T) {
	// "cordon this upstream entirely" is the operator's emergency lever. If
	// the wildcard were only consulted for method "*", the upstream would keep
	// serving every named method.
	tracker := newSeamTracker(t, "cordon-wildcard")
	ups := common.NewFakeUpstream("up1")

	tracker.Cordon(ups, "*", "provider incident")
	require.True(t, tracker.IsCordoned(ups, "eth_call"))
	require.True(t, tracker.IsCordoned(ups, "eth_getLogs"))
	require.True(t, tracker.IsCordoned(ups, "some_unknown_method"))
}

func TestIsCordoned_UncordonRestoresRouting(t *testing.T) {
	tracker := newSeamTracker(t, "cordon-restore")
	ups := common.NewFakeUpstream("up1")

	tracker.Cordon(ups, "eth_call", "stale")
	tracker.Uncordon(ups, "eth_call", "recovered")
	require.False(t, tracker.IsCordoned(ups, "eth_call"),
		"an uncordoned upstream must return to rotation")
}

func TestIsCordoned_IsScopedToTheUpstream(t *testing.T) {
	// Cordoning one provider must not take its peers out with it.
	tracker := newSeamTracker(t, "cordon-scope")
	bad := common.NewFakeUpstream("bad")
	good := common.NewFakeUpstream("good")

	tracker.Cordon(bad, "*", "provider incident")
	require.True(t, tracker.IsCordoned(bad, "eth_call"))
	require.False(t, tracker.IsCordoned(good, "eth_call"))
}

func TestCordonedReason_ReportsWhyAnUpstreamIsOut(t *testing.T) {
	// This is what an operator sees when they ask "why is this upstream out?".
	// An empty reason turns a diagnosable incident into a guessing game.
	tracker := newSeamTracker(t, "cordon-reason")
	ups := common.NewFakeUpstream("up1")

	reason, cordoned := tracker.CordonedReason(ups, "eth_call")
	require.False(t, cordoned)
	require.Equal(t, "", reason)

	tracker.Cordon(ups, "eth_call", "block head lag above 500")
	reason, cordoned = tracker.CordonedReason(ups, "eth_call")
	require.True(t, cordoned)
	require.Equal(t, "block head lag above 500", reason)
}

func TestCordonedReason_PrefersTheWildcardCordon(t *testing.T) {
	// A fleet-wide cordon is the more serious condition. Reporting the
	// method-scoped reason instead would tell the operator "stale eth_call"
	// while the real cause is that they cordoned the whole provider.
	tracker := newSeamTracker(t, "cordon-reason-wildcard")
	ups := common.NewFakeUpstream("up1")

	tracker.Cordon(ups, "eth_call", "method specific")
	tracker.Cordon(ups, "*", "provider incident")

	reason, cordoned := tracker.CordonedReason(ups, "eth_call")
	require.True(t, cordoned)
	require.Equal(t, "provider incident", reason)
}

func TestCordonedReason_ClearedOnUncordon(t *testing.T) {
	// A stale reason left behind after recovery would show a healthy upstream
	// as still out. The metrics-snapshot JSON publishes lastCordonedReason
	// independently of the cordoned flag, so an operator reading the snapshot
	// would see a reason for an upstream that is back in rotation.
	tracker := newSeamTracker(t, "cordon-reason-cleared")
	ups := common.NewFakeUpstream("up1")

	tracker.Cordon(ups, "eth_call", "stale")
	tracker.Uncordon(ups, "eth_call", "recovered")

	reason, cordoned := tracker.CordonedReason(ups, "eth_call")
	require.False(t, cordoned)
	require.Equal(t, "", reason)

	snapshot, err := tracker.GetUpstreamMethodMetrics(ups, "eth_call", common.DataFinalityStateAll).MarshalJSON()
	require.NoError(t, err)
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(snapshot, &decoded))
	require.Equal(t, false, decoded["cordoned"])
	require.Equal(t, "", decoded["lastCordonedReason"],
		"the snapshot must not keep publishing a reason after the upstream recovered")
}

func TestTrackedMetricsSnapshot_PublishesTheCordonReasonWhileCordoned(t *testing.T) {
	// The complement: while the upstream IS out, the snapshot has to say why.
	tracker := newSeamTracker(t, "cordon-snapshot")
	ups := common.NewFakeUpstream("up1")
	tracker.Cordon(ups, "eth_call", "block head lag above 500")

	snapshot, err := tracker.GetUpstreamMethodMetrics(ups, "eth_call", common.DataFinalityStateAll).MarshalJSON()
	require.NoError(t, err)
	var decoded map[string]interface{}
	require.NoError(t, json.Unmarshal(snapshot, &decoded))
	require.Equal(t, true, decoded["cordoned"])
	require.Equal(t, "block head lag above 500", decoded["lastCordonedReason"])
}

func TestEnableFinalityTracking_IsOffUntilAskedFor(t *testing.T) {
	// Finality tracking multiplies every upstream metric bucket by the number
	// of finality states. Turning it on by default would multiply the
	// /metrics page for every operator who never asked for it.
	tracker := newSeamTracker(t, "finality-flag")
	require.False(t, tracker.IsFinalityTracked(), "finality tracking must be opt-in")

	tracker.EnableFinalityTracking()
	require.True(t, tracker.IsFinalityTracked())
}
