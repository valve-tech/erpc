package health

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// SetLatestBlockNumber is the chain-agnostic seam every selection policy reads
// through. It writes three things an operator's routing depends on:
//
//   - the NETWORK head, which is the reference every lag is measured against;
//   - the per-upstream head;
//   - BlockHeadLag on every metrics bucket a policy can evaluate, including
//     the {*, All} wildcard bucket that evalScope:network policies read.
//
// A miss on any of them makes blockNumberLagAbove silently never fire, which
// means a lagging upstream keeps serving stale reads with no visible symptom.

func newSeamTracker(t *testing.T, project string) *Tracker {
	t.Helper()
	tracker := NewTracker(&log.Logger, project, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	tracker.Bootstrap(ctx)
	return tracker
}

// wildcardLag reads the {ups, "*", All} bucket — the one a network-scope
// selection policy evaluates.
func wildcardLag(tracker *Tracker, ups common.Upstream) int64 {
	return tracker.GetUpstreamMethodMetrics(ups, "*", common.DataFinalityStateAll).BlockHeadLag.Load()
}

func TestSetLatestBlockNumber_WritesTheWildcardBucketWithoutAnyRequestTraffic(t *testing.T) {
	// The dedup index is built from REQUEST traffic. A poller that fires before
	// any client request must still populate {*, All}; otherwise a brand-new
	// upstream reads lag=0 no matter how far behind it is, and the selection
	// policy happily routes to it.
	tracker := newSeamTracker(t, "seam-no-traffic")
	fast := common.NewFakeUpstream("fast")
	slow := common.NewFakeUpstream("slow")

	tracker.SetLatestBlockNumber(fast, 1000, 0)
	tracker.SetLatestBlockNumber(slow, 940, 0)

	require.EqualValues(t, 0, wildcardLag(tracker, fast), "the upstream at the tip has no lag")
	require.EqualValues(t, 60, wildcardLag(tracker, slow),
		"a lagging upstream's lag must reach {*, All} even with zero request traffic")
}

func TestSetLatestBlockNumber_NetworkHeadIsTheMaxAcrossUpstreams(t *testing.T) {
	// Lag is measured against the network head. If a lagging upstream could
	// lower it, every peer's lag would read as 0 and the whole fleet would look
	// caught up while serving stale data.
	tracker := newSeamTracker(t, "seam-network-head")
	net := common.NewFakeUpstream("a").NetworkId()

	tracker.SetLatestBlockNumber(common.NewFakeUpstream("a"), 1000, 0)
	tracker.SetLatestBlockNumber(common.NewFakeUpstream("b"), 800, 0)

	require.EqualValues(t, 1000, tracker.getMetadata(metadataKey{nil, net}).evmLatestBlockNumber.Load(),
		"a lower sample must not drag the network head down")
}

func TestSetLatestBlockNumber_LagUpdatesForEveryPeerWhenTheTipAdvances(t *testing.T) {
	// Only ONE upstream's poller fires per call, but the tip moving changes
	// EVERY peer's lag. If peers were not recomputed, an upstream whose poller
	// is stale or absent would keep reporting the lag it had when it last
	// polled — which is 0 right after it starts.
	tracker := newSeamTracker(t, "seam-peers")
	a := common.NewFakeUpstream("a")
	b := common.NewFakeUpstream("b")
	c := common.NewFakeUpstream("c")

	tracker.SetLatestBlockNumber(a, 1000, 0)
	tracker.SetLatestBlockNumber(b, 1000, 0)
	tracker.SetLatestBlockNumber(c, 1000, 0)
	require.EqualValues(t, 0, wildcardLag(tracker, b))

	// Only a's poller fires and moves the tip forward by 50.
	tracker.SetLatestBlockNumber(a, 1050, 0)

	require.EqualValues(t, 0, wildcardLag(tracker, a))
	require.EqualValues(t, 50, wildcardLag(tracker, b),
		"a peer that did not poll must still show the new lag")
	require.EqualValues(t, 50, wildcardLag(tracker, c))
}

func TestSetLatestBlockNumber_LagReachesThePerMethodBucketsToo(t *testing.T) {
	// Policies evaluate at several grains: network ({*, All}) and per-method
	// ({method, All}). A lag written to only one grain makes the same policy
	// behave differently depending on its evalScope — an inconsistency an
	// operator has almost no way to diagnose.
	tracker := newSeamTracker(t, "seam-grains")
	a := common.NewFakeUpstream("a")
	b := common.NewFakeUpstream("b")

	// Give both upstreams request traffic so their per-method buckets exist.
	tracker.RecordUpstreamRequest(a, "eth_getBalance", common.DataFinalityStateUnfinalized)
	tracker.RecordUpstreamRequest(b, "eth_getBalance", common.DataFinalityStateUnfinalized)

	tracker.SetLatestBlockNumber(b, 900, 0)
	tracker.SetLatestBlockNumber(a, 1000, 0)

	perMethod := tracker.GetUpstreamMethodMetrics(b, "eth_getBalance", common.DataFinalityStateAll)
	require.EqualValues(t, 100, perMethod.BlockHeadLag.Load(),
		"the per-method {method, All} rollup must carry the same lag as the wildcard bucket")
	require.EqualValues(t, 100, wildcardLag(tracker, b))
}

func TestSetLatestBlockNumber_NonPositiveSamplesAreRejected(t *testing.T) {
	// A poller that fails to parse a response can hand us 0 or -1. Storing it
	// would make the network head zero, every lag negative, and every
	// lag-based predicate meaningless.
	tracker := newSeamTracker(t, "seam-nonpositive")
	a := common.NewFakeUpstream("a")
	net := a.NetworkId()

	// The head is far above the rollback tolerance, so a 0 or -1 sample would
	// otherwise be accepted as a "large correction" and stored.
	const head = 100_000
	tracker.SetLatestBlockNumber(a, head, 0)
	tracker.SetLatestBlockNumber(a, 0, 0)
	tracker.SetLatestBlockNumber(a, -5, 0)

	require.EqualValues(t, head, tracker.getMetadata(metadataKey{nil, net}).evmLatestBlockNumber.Load(),
		"a non-positive sample must not touch the network head")
	require.EqualValues(t, head, tracker.getMetadata(metadataKey{a, net}).evmLatestBlockNumber.Load(),
		"a non-positive sample must not touch the upstream head")
}

func TestSetLatestBlockNumber_SmallDecreasesAreIgnoredAsNoise(t *testing.T) {
	// Providers behind a load balancer routinely answer from a node that is a
	// block or two behind. Adopting every dip would make the recorded head
	// oscillate and produce phantom lag alerts.
	tracker := newSeamTracker(t, "seam-noise")
	a := common.NewFakeUpstream("a")
	net := a.NetworkId()

	tracker.SetLatestBlockNumber(a, 1000, 0)
	tracker.SetLatestBlockNumber(a, 999, 0)

	require.EqualValues(t, 1000, tracker.getMetadata(metadataKey{a, net}).evmLatestBlockNumber.Load(),
		"a one-block dip is noise and must be ignored")
}

func TestSetLatestBlockNumber_ALargeRollbackRederivesTheNetworkHead(t *testing.T) {
	// A provider briefly reporting another chain's height pins the network head
	// forever under max-only storage, and every lag derived from it stays
	// wrong until the process restarts. The correction must be accepted AND
	// the network head re-derived from the remaining upstreams — never adopted
	// from the single correcting sample, which may itself be behind.
	tracker := newSeamTracker(t, "seam-rollback")
	bogus := common.NewFakeUpstream("bogus")
	healthy := common.NewFakeUpstream("healthy")
	net := bogus.NetworkId()

	tracker.SetLatestBlockNumber(healthy, 1000, 0)
	tracker.SetLatestBlockNumber(bogus, 50_000_000, 0)
	require.EqualValues(t, 50_000_000, tracker.getMetadata(metadataKey{nil, net}).evmLatestBlockNumber.Load(),
		"precondition: the bogus sample became the network head")

	// The provider corrects itself — and lands BELOW the healthy peer, which is
	// the case that separates "re-derive from all upstreams" from "adopt this
	// one sample". Adopting 900 here would understate the tip by 100 blocks
	// and hide every peer's real lag.
	tracker.SetLatestBlockNumber(bogus, 900, 0)

	require.EqualValues(t, 900, tracker.getMetadata(metadataKey{bogus, net}).evmLatestBlockNumber.Load(),
		"a large decrease must be accepted as a correction")
	require.EqualValues(t, 1000, tracker.getMetadata(metadataKey{nil, net}).evmLatestBlockNumber.Load(),
		"the network head must be re-derived as the max over ALL upstreams, not adopted from the correcting one")
	require.EqualValues(t, 0, wildcardLag(tracker, healthy),
		"the healthy upstream is the tip and must read as caught up once the bogus head is gone")
	require.EqualValues(t, 100, wildcardLag(tracker, bogus),
		"the corrected upstream must now show its real lag against the re-derived head")
}

func TestSetLatestBlockNumber_FeedsTheBlockTimeEstimateThatConvertsLagToSeconds(t *testing.T) {
	// Selection policies read blockHeadLagSeconds, which is
	// blockHeadLag x GetNetworkBlockTime. If the EMA never warms up, that
	// product is zero and a seconds-based threshold never fires no matter how
	// far behind an upstream is.
	tracker := newSeamTracker(t, "seam-blocktime")
	a := common.NewFakeUpstream("a")

	require.EqualValues(t, 0, tracker.GetNetworkBlockTime(a.NetworkId()),
		"no estimate before any timestamped sample")

	// Twelve-second blocks, on-chain timestamps.
	base := time.Now().Unix() - 600
	for i := int64(0); i < 12; i++ {
		tracker.SetLatestBlockNumber(a, 1000+i, base+i*12)
	}

	bt := tracker.GetNetworkBlockTime(a.NetworkId())
	require.NotZero(t, bt, "the block-time EMA must warm up from on-chain timestamps")
	require.InDelta(t, 12.0, bt.Seconds(), 1.0,
		"the estimate must track the real 12s cadence, got %s", bt)
}

func TestSetLatestBlockNumber_BlockTimeIgnoresRepeatedSamplesOfTheSameHead(t *testing.T) {
	// A poller that keeps reporting the same head must not be read as
	// "zero-second blocks". That would collapse the lag-to-seconds conversion
	// and disable every seconds-based threshold on the network.
	tracker := newSeamTracker(t, "seam-blocktime-flat")
	a := common.NewFakeUpstream("a")

	ts := time.Now().Unix() - 600
	for i := 0; i < 20; i++ {
		tracker.SetLatestBlockNumber(a, 1000, ts+int64(i))
	}
	require.EqualValues(t, 0, tracker.GetNetworkBlockTime(a.NetworkId()),
		"a stuck head must produce no block-time estimate rather than a zero one")
}

func TestSetLatestBlockNumberForNetwork_SetsTheHeadDirectly(t *testing.T) {
	// The shared-state path writes the network head without an upstream
	// attached. It must land in the same place SetLatestBlockNumber reads, or
	// two eRPC instances sharing state will disagree about the tip.
	tracker := newSeamTracker(t, "seam-network-direct")
	a := common.NewFakeUpstream("a")
	net := a.NetworkId()

	tracker.SetLatestBlockNumberForNetwork(net, 2000)
	require.EqualValues(t, 2000, tracker.getMetadata(metadataKey{nil, net}).evmLatestBlockNumber.Load())

	// An upstream at 1900 must now read as 100 behind.
	tracker.SetLatestBlockNumber(a, 1900, 0)
	require.EqualValues(t, 100, wildcardLag(tracker, a),
		"lag must be measured against the externally-set network head")
}
