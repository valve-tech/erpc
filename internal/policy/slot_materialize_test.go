package policy

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// materializeOrder is the seam where the JS eval's answer becomes the real
// routing order. Every request on the slot is served in exactly this order,
// so an order change here is a traffic change — and any upstream the eval
// omitted must land in the excluded set, or it disappears from the
// diagnostics without ever being reported as dropped.

type orderUpstream struct {
	common.Upstream
	id string
}

func (u *orderUpstream) Id() string { return u.id }
func (u *orderUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: u.id}
}
func (u *orderUpstream) Logger() *zerolog.Logger { l := zerolog.Nop(); return &l }

func upsList(ids ...string) []common.Upstream {
	out := make([]common.Upstream, len(ids))
	for i, id := range ids {
		out[i] = &orderUpstream{id: id}
	}
	return out
}

func idsOf(ups []common.Upstream) []string {
	out := make([]string, len(ups))
	for i, u := range ups {
		out[i] = u.Id()
	}
	return out
}

func TestMaterializeOrder_PreservesTheEvalsOrderNotTheInputOrder(t *testing.T) {
	// The JS side ranked these. If materializeOrder re-sorted by input
	// order, every sortByScore in every policy would be silently discarded.
	ups := upsList("a", "b", "c")
	ordered, excluded := materializeOrder(ups, []string{"c", "a", "b"})
	require.Equal(t, []string{"c", "a", "b"}, idsOf(ordered))
	require.Empty(t, excluded)
}

func TestMaterializeOrder_ReportsOmittedUpstreamsAsExcludedInInputOrder(t *testing.T) {
	// An upstream the eval dropped must appear in the excluded set with a
	// reason. Without it the operator sees an upstream vanish from routing
	// with nothing in the decision record explaining why.
	ups := upsList("a", "b", "c", "d")
	ordered, excluded := materializeOrder(ups, []string{"c", "a"})

	require.Equal(t, []string{"c", "a"}, idsOf(ordered))
	require.Equal(t, []string{"b", "d"}, excludedIDs(excluded),
		"excluded upstreams are reported in input order, so the record is stable across ticks")
	for _, e := range excluded {
		require.Equal(t, "not in eval result", e.Reason)
	}
}

func TestMaterializeOrder_DropsIdsTheInputSetDoesNotContain(t *testing.T) {
	// A policy that invents an ID (a typo in forceInclude, a stale sticky
	// primary) must not produce a nil entry that panics the request path.
	ups := upsList("a", "b")
	ordered, excluded := materializeOrder(ups, []string{"a", "ghost", "b"})
	require.Equal(t, []string{"a", "b"}, idsOf(ordered))
	require.Empty(t, excluded)
}

func TestMaterializeOrder_DeduplicatesRepeatedIdsKeepingTheFirstPosition(t *testing.T) {
	// A `union` bug or a double `forceInclude` can repeat an ID. Serving
	// the same upstream twice in one request wastes a failover attempt on
	// an upstream already known to have failed.
	ups := upsList("a", "b", "c")
	ordered, excluded := materializeOrder(ups, []string{"b", "a", "b", "c", "a"})
	require.Equal(t, []string{"b", "a", "c"}, idsOf(ordered))
	require.Empty(t, excluded)
}

func TestMaterializeOrder_EmptyEvalResultExcludesEverything(t *testing.T) {
	// A policy that filters everything out is a real outage. It must be
	// visible as a full exclusion list, not as an empty decision record.
	ups := upsList("a", "b")
	ordered, excluded := materializeOrder(ups, nil)
	require.Empty(t, ordered)
	require.Equal(t, []string{"a", "b"}, excludedIDs(excluded))
}

func TestMaterializeOrder_EmptyInputSetYieldsNothing(t *testing.T) {
	ordered, excluded := materializeOrder(nil, []string{"a"})
	require.Empty(t, ordered)
	require.Empty(t, excluded)
}

func TestSplitStepFromLeafReasons_TakesTheFirstStepAndDropsLaterOnes(t *testing.T) {
	// The step name is a metric label. A second sentinel leaking through
	// would publish "@step:..." as an exclusion reason and blow up the
	// cardinality of selection_exclusion_total.
	step, leaves := splitStepFromLeafReasons([]string{"@step:byTag", "@step:removeCordoned", "lag"})
	require.Equal(t, "byTag", step, "first step wins, matching the JS side")
	require.Equal(t, []string{"lag"}, leaves)
}

func TestSplitStepFromLeafReasons_KeepsLeafOrder(t *testing.T) {
	// Each leaf becomes one metric increment. Reordering them would not
	// change the counts, but it would change what an operator reads as the
	// primary cause in the decision record.
	step, leaves := splitStepFromLeafReasons([]string{"errorRate", "@step:excludeIf", "latency"})
	require.Equal(t, "excludeIf", step)
	require.Equal(t, []string{"errorRate", "latency"}, leaves)
}

func TestSplitStepFromLeafReasons_NoSentinelLeavesTheSlugsAlone(t *testing.T) {
	step, leaves := splitStepFromLeafReasons([]string{"lag", "throttling"})
	require.Equal(t, "", step)
	require.Equal(t, []string{"lag", "throttling"}, leaves)
}

func TestSplitStepFromLeafReasons_OnlyASentinelYieldsNilLeaves(t *testing.T) {
	// nil, not an empty slice: `len(LeafReasons) > 0` is the branch that
	// decides whether Reason comes from a leaf slug or from the step name.
	step, leaves := splitStepFromLeafReasons([]string{"@step:take"})
	require.Equal(t, "take", step)
	require.Nil(t, leaves)
}

func TestSplitStepFromLeafReasons_EmptyInputIsSafe(t *testing.T) {
	step, leaves := splitStepFromLeafReasons(nil)
	require.Equal(t, "", step)
	require.Nil(t, leaves)
}

func TestSplitStepFromLeafReasons_DoesNotMistakeAShortSlugForASentinel(t *testing.T) {
	// A slug shorter than the "@step:" prefix must not be sliced — that
	// would panic the tick and stall selection for the whole network.
	step, leaves := splitStepFromLeafReasons([]string{"lag", "@", "@step", "@step:byTag"})
	require.Equal(t, "byTag", step)
	require.Equal(t, []string{"lag", "@", "@step"}, leaves)
}

func TestSplitStepFromLeafReasons_EmptyStepNameStillCountsAsASentinel(t *testing.T) {
	// "@step:" with nothing after it must be consumed, not published as a
	// leaf reason — an empty metric label is worse than none.
	step, leaves := splitStepFromLeafReasons([]string{"@step:", "lag"})
	require.Equal(t, "", step)
	require.Equal(t, []string{"lag"}, leaves)
}

func TestEnrichExcluded_PrefersTheLeafSlugOverTheStepName(t *testing.T) {
	// The leaf slug names the signal ("errorRate"); the step names the
	// primitive ("excludeIf"). An operator needs the signal first.
	ex := []ExcludedUpstream{{ID: "a", Step: "excludeIf", LeafReasons: []string{"errorRate", "latency"}}}
	enrichExcluded(ex)
	require.Equal(t, "errorRate", ex[0].Reason)
}

func TestEnrichExcluded_FallsBackToTheStepName(t *testing.T) {
	ex := []ExcludedUpstream{{ID: "a", Step: "removeCordoned"}}
	enrichExcluded(ex)
	require.Equal(t, "removeCordoned", ex[0].Reason)
}

func TestEnrichExcluded_LeavesTheMaterializeDefaultInPlace(t *testing.T) {
	// A raw Array.filter fall-through has neither a step nor a leaf. The
	// default reason set by materializeOrder must survive.
	ex := []ExcludedUpstream{{ID: "a", Reason: "not in eval result"}}
	enrichExcluded(ex)
	require.Equal(t, "not in eval result", ex[0].Reason)
}

func TestEnrichExcluded_HandlesAnEmptySlice(t *testing.T) {
	enrichExcluded(nil)
	enrichExcluded([]ExcludedUpstream{})
}
