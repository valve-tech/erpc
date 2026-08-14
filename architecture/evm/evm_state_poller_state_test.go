package evm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the poller's read-side answers: whether a block counts as
// final, and what happens to a suggestion that is not strictly newer. Both feed
// routing and cache decisions, and both are silent when they go wrong.

// pollerCtx bounds every direct shared-counter write in this file.
func pollerCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestIsBlockFinalized_RefusesToAnswerBeforeAnyHeadIsKnown(t *testing.T) {
	t.Parallel()

	// Cold start: eRPC knows neither head. Answering "false" would be a guess
	// that reads as "not final yet", so callers would keep re-fetching a block
	// that has been final for hours. The typed error lets the caller fall back
	// instead of trusting a made-up answer.
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)

	final, err := p.IsBlockFinalized(100)
	require.Error(t, err, "with no head known at all, the poller must say it cannot tell")
	assert.False(t, final)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeFinalizedBlockUnavailable),
		"the caller distinguishes 'unknown' from 'not final' by this code, got %T: %v", err, err)
}

func TestIsBlockFinalized_UsesTheRealFinalizedHeadWhenItHasOne(t *testing.T) {
	t.Parallel()

	// A real finalized head from the chain beats any inference. The boundary
	// matters: the finalized block itself IS final, the next one is not.
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)
	p.finalizedBlockShared.TryUpdate(pollerCtx(t), 1000)
	p.latestBlockShared.TryUpdate(pollerCtx(t), 5000)
	require.Equal(t, int64(1000), p.FinalizedBlock())

	for _, tc := range []struct {
		block int64
		want  bool
	}{
		{block: 999, want: true},
		{block: 1000, want: true},
		{block: 1001, want: false},
		{block: 5000, want: false},
	} {
		final, err := p.IsBlockFinalized(tc.block)
		require.NoError(t, err)
		assert.Equalf(t, tc.want, final,
			"block %d against a real finalized head of 1000", tc.block)
	}
}

func TestIsBlockFinalized_InfersAFinalizedHeadFromTheLatestWhenTheChainOffersNone(t *testing.T) {
	t.Parallel()

	// Many chains never answer eth_getBlockByNumber("finalized"). Without an
	// inference, every cache entry on those chains would stay unfinalized
	// forever and never become permanently cacheable. The inference sits a
	// fallback depth below the head.
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)
	depth := p.fallbackFinalityDepth()
	latest := depth + 500
	p.latestBlockShared.TryUpdate(pollerCtx(t), latest)
	require.Zero(t, p.FinalizedBlock(), "precondition: no real finalized head")

	final, err := p.IsBlockFinalized(500)
	require.NoError(t, err)
	assert.True(t, final, "a block exactly at latest-depth must count as final")

	final, err = p.IsBlockFinalized(501)
	require.NoError(t, err)
	assert.False(t, final, "one block above latest-depth is inside the reorg window")

	final, err = p.IsBlockFinalized(latest)
	require.NoError(t, err)
	assert.False(t, final, "the head itself is never final by inference")
}

func TestIsBlockFinalized_TreatsAShallowChainAsHavingNothingFinal(t *testing.T) {
	t.Parallel()

	// A chain younger than the fallback depth has no block far enough below the
	// head to be safe. Subtracting anyway would underflow to a negative height
	// and mark genesis-adjacent blocks final on a chain that can still reorg
	// them. The code guards this with `if latestBlock > depth`; pin the guard.
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)
	depth := p.fallbackFinalityDepth()
	p.latestBlockShared.TryUpdate(pollerCtx(t), depth-1)

	for _, block := range []int64{1, 2, depth - 2} {
		final, err := p.IsBlockFinalized(block)
		require.NoError(t, err)
		assert.Falsef(t, final, "block %d on a chain shorter than the fallback depth must not be final", block)
	}

	// Genesis is the one exception the guard leaves through: the inferred head
	// pins at 0, so block 0 reads as final. Genesis never reorgs, so this is
	// correct — recorded here so nobody reads it as the underflow the guard
	// prevents.
	final, err := p.IsBlockFinalized(0)
	require.NoError(t, err)
	assert.True(t, final, "genesis is final on any chain length")
}

func TestIsBlockFinalized_AnswersFromTheFinalizedHeadEvenWithNoLatestHead(t *testing.T) {
	t.Parallel()

	// The two counters advance independently and either can be the first to
	// arrive. A finalized head alone is enough to answer, and must not be
	// discarded because the latest head has not landed yet.
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)
	p.finalizedBlockShared.TryUpdate(pollerCtx(t), 700)
	require.Zero(t, p.LatestBlock(), "precondition: no latest head")

	final, err := p.IsBlockFinalized(700)
	require.NoError(t, err)
	assert.True(t, final)

	final, err = p.IsBlockFinalized(701)
	require.NoError(t, err)
	assert.False(t, final)
}

func TestSuggestLatestBlock_SilentlyDropsAnythingNotStrictlyNewer(t *testing.T) {
	t.Parallel()

	// The head counter only ever moves forward. A stale or equal sample from a
	// lagging upstream is dropped WITHOUT an error and without a metric — the
	// caller cannot tell the difference between "applied" and "ignored". That
	// asymmetry is deliberate (the counter must never roll back), but it makes
	// any test that suggests a value and then waits for it inherently racy.
	// Pinning it here stops someone "fixing" the silence into a rollback.
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)

	p.SuggestLatestBlock(2000)
	require.Equal(t, int64(2000), p.LatestBlock())

	p.SuggestLatestBlock(2000) // equal
	assert.Equal(t, int64(2000), p.LatestBlock(), "an equal sample must not disturb the head")

	p.SuggestLatestBlock(1) // far behind
	assert.Equal(t, int64(2000), p.LatestBlock(), "a stale sample must never roll the head back")

	p.SuggestLatestBlock(0) // the zero value a failed probe produces
	assert.Equal(t, int64(2000), p.LatestBlock(), "a zero from a failed probe must never reach the head")

	p.SuggestLatestBlock(2001) // strictly newer: applies
	assert.Equal(t, int64(2001), p.LatestBlock())
}

func TestSuggestFinalizedBlock_SilentlyDropsAnythingNotStrictlyNewer(t *testing.T) {
	t.Parallel()

	// Same rule on the finalized counter, but this one applies in a goroutine,
	// so the drop is doubly invisible. A finalized head that rolled back would
	// be worse than a latest head that did: entries already written to the cache
	// as permanent would be re-fetched and could disagree.
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)

	p.SuggestFinalizedBlock(2000)
	require.Eventually(t, func() bool { return p.FinalizedBlock() == 2000 },
		2*time.Second, 10*time.Millisecond)

	for _, stale := range []int64{2000, 1, 0} {
		p.SuggestFinalizedBlock(stale)
	}
	require.Never(t, func() bool { return p.FinalizedBlock() != 2000 },
		300*time.Millisecond, 20*time.Millisecond,
		"no stale or equal sample may move the finalized head")
}

func TestSyncingState_RoundTripsWhatTheProbeReported(t *testing.T) {
	t.Parallel()

	// Routing skips an upstream that reports itself syncing. The setter and the
	// getter take different locks, so a mismatch between them would show up only
	// as an upstream that never leaves (or never enters) the syncing state.
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)

	assert.Equal(t, common.EvmSyncingStateUnknown, p.SyncingState(),
		"a poller that has not probed yet must report unknown, not 'not syncing'")

	for _, state := range []common.EvmSyncingState{
		common.EvmSyncingStateSyncing,
		common.EvmSyncingStateNotSyncing,
		common.EvmSyncingStateUnknown,
	} {
		p.SetSyncingState(state)
		assert.Equal(t, state, p.SyncingState())
	}
}

func TestIsObjectNull_TreatsAPollerWithoutAnUpstreamAsAbsent(t *testing.T) {
	t.Parallel()

	// Callers hold this as an interface, so a nil poller arrives as a non-nil
	// interface holding a nil pointer. Every guard in the routing path asks
	// IsObjectNull instead of comparing to nil, and it must also reject a
	// half-built poller whose upstream was never wired.
	var nilPoller *EvmStatePoller
	assert.True(t, nilPoller.IsObjectNull(), "a nil poller must report itself absent, not panic")
	assert.True(t, (&EvmStatePoller{}).IsObjectNull(), "a poller with no upstream is not usable")

	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)
	assert.False(t, p.IsObjectNull(), "a fully wired poller must report itself present")
}

func TestEarliestBlock_ReportsZeroForAProbeItHasNeverRun(t *testing.T) {
	t.Parallel()

	// Zero means "no lower bound known" and the availability check fails OPEN on
	// it. Returning anything else for an unprobed method would fence off history
	// the upstream actually holds.
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)

	for _, probe := range []common.EvmAvailabilityProbeType{
		common.EvmProbeBlockHeader,
		common.EvmProbeEventLogs,
		common.EvmProbeCallState,
		common.EvmProbeTraceData,
	} {
		assert.Zerof(t, p.EarliestBlock(probe),
			"the unprobed %q bound must read as unknown (0), so the check fails open", probe)
	}
}

func TestSetNetworkConfig_IgnoresAConfigWithNoEvmBlock(t *testing.T) {
	t.Parallel()

	// A non-EVM (or empty) network config reaching an EVM poller would otherwise
	// null out the finality settings the poller was built with, silently
	// changing which blocks count as final.
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)
	p.SetNetworkConfig(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{
		ChainId:               123,
		FallbackFinalityDepth: 64,
	}})
	require.Equal(t, int64(64), p.fallbackFinalityDepth(), "precondition: the real config applied")

	p.SetNetworkConfig(nil)
	assert.Equal(t, int64(64), p.fallbackFinalityDepth(), "a nil config must be ignored, not applied")

	p.SetNetworkConfig(&common.NetworkConfig{})
	assert.Equal(t, int64(64), p.fallbackFinalityDepth(), "a config with no evm block must be ignored")
}

func TestSetNetworkConfig_PrefersTheOperatorsAliasForTheNetworkLabel(t *testing.T) {
	t.Parallel()

	// The label lands on every metric this poller emits. An operator who set an
	// alias expects to find it on their dashboard rather than the raw chain id.
	up := newSuggestGateUpstream(123, "0x7b", nil)
	p := newGateTestPoller(t, up)

	p.SetNetworkConfig(&common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	})
	require.Equal(t, "evm:123", p.networkLabel, "with no alias, the network id is the label")

	p.SetNetworkConfig(&common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Alias:        "mainnet",
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
	})
	assert.Equal(t, "mainnet", p.networkLabel, "the operator's alias must win")
}
