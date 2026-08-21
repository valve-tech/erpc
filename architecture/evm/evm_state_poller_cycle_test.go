package evm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive one full Poll cycle, the periodic earliest-bound scheduler
// and the give-up counters. They all use forwardingUpstream, because every
// branch here reads a real response body.
//
// Nothing waits on a background goroutine with a margin. Where a goroutine is
// unavoidable the test synchronises on a channel the fixture closes, or on the
// mutex the goroutine itself releases.

// pollingUpstream answers eth_getBlockByNumber with a fixed header and routes
// eth_syncing to the handler the test supplies. Poll fans out over both, so a
// double that answers only one of them turns the other into a poll error.
func pollingUpstream(t *testing.T, head int64, syncing forwardHandler) *forwardingUpstream {
	t.Helper()
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResult(req, blockHeader(head))
	})
	up.on("eth_syncing", syncing)
	return up
}

// --- Poll: the eth_syncing arm ---

func TestPoll_ANodeIsCalledSyncedOnlyAfterEnoughConsecutiveAnswers(t *testing.T) {
	up := pollingUpstream(t, 500, syncingResult(`false`))
	p := newGateTestPoller(t, up)

	for i := 1; i < FullySyncedThreshold; i++ {
		require.NoError(t, p.Poll(context.Background()))
		// Discriminating: one "not syncing" answer is not proof. A poller that
		// trusted the first answer would already say NotSyncing here, and the
		// router would send traffic to a node still filling its state.
		assert.Equalf(t, common.EvmSyncingStateUnknown, p.SyncingState(),
			"after %d of %d confirmations the state must still be unknown", i, FullySyncedThreshold)
	}

	require.NoError(t, p.Poll(context.Background()))
	assert.Equal(t, common.EvmSyncingStateNotSyncing, p.SyncingState())
}

func TestPoll_ASyncingAnswerMarksTheNodeSyncingAndRestartsTheCount(t *testing.T) {
	var syncing bool
	var mu sync.Mutex
	up := pollingUpstream(t, 500, func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		if syncing {
			return jsonResult(req, `true`)
		}
		return jsonResult(req, `false`)
	})
	p := newGateTestPoller(t, up)

	// Get within one answer of "fully synced".
	for i := 1; i < FullySyncedThreshold; i++ {
		require.NoError(t, p.Poll(context.Background()))
	}
	require.Equal(t, common.EvmSyncingStateUnknown, p.SyncingState())

	mu.Lock()
	syncing = true
	mu.Unlock()
	require.NoError(t, p.Poll(context.Background()))
	assert.Equal(t, common.EvmSyncingStateSyncing, p.SyncingState(),
		"a node that reports it is syncing must be marked syncing straight away")

	// One "not syncing" answer after that must NOT restore the near-synced
	// count. Discriminating: without the reset the very next answer would flip
	// the node to fully synced, which is exactly the false-ready signal the
	// confirmation count exists to prevent.
	mu.Lock()
	syncing = false
	mu.Unlock()
	require.NoError(t, p.Poll(context.Background()))
	assert.Equal(t, common.EvmSyncingStateUnknown, p.SyncingState())
}

func TestPoll_AnUpstreamThatOptsOutIsMarkedNotSyncingWithoutAsking(t *testing.T) {
	up := pollingUpstream(t, 500, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		t.Fatal("the poller must not ask an upstream that opted out of the syncing check")
		return nil, nil
	})
	skip := true
	up.cfg.Evm.SkipSyncingCheck = &skip
	p := newGateTestPoller(t, up)

	require.NoError(t, p.Poll(context.Background()))

	assert.Equal(t, common.EvmSyncingStateNotSyncing, p.SyncingState())
	assert.Equal(t, 0, up.callCount("eth_syncing"))
}

// --- fetchSyncingState: the give-up latch ---

func TestFetchSyncingState_OnlyAnUnsupportedAnswerStopsTheProbe(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantSkip bool
	}{
		{"Unsupported", common.NewErrEndpointUnsupported(errors.New("eth_syncing is not available")), true},
		{"MethodIgnored", common.NewErrUpstreamMethodIgnored("eth_syncing", "fwd-ups"), true},
		// Discriminating: a transport failure says nothing about method
		// support. Latching on it would blind the poller to a node that comes
		// back a second later.
		{"TransportFailure", errors.New("dial tcp: connection refused"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := syncingPoller(t, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				return nil, tc.err
			})

			_, err := p.fetchSyncingState(context.Background())
			require.Error(t, err)

			p.stateMu.RLock()
			defer p.stateMu.RUnlock()
			assert.Equal(t, tc.wantSkip, p.skipSyncingCheck)
		})
	}
}

// --- PollLatestBlockNumber: the give-up counter ---

func TestPollLatestBlockNumber_GivesUpAfterTenUnsupportedAnswers(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return nil, common.NewErrEndpointUnsupported(errors.New("no eth_getBlockByNumber on this node"))
	})
	p := newGateTestPoller(t, up)
	p.SetNetworkConfig(&common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123, FallbackStatePollerDebounce: common.Duration(time.Nanosecond)},
	})

	for i := 0; i < 10; i++ {
		// The shared counter stamps freshness in whole milliseconds, so a tight
		// loop would be debounced away whatever the interval.
		time.Sleep(2 * time.Millisecond)
		got, err := p.PollLatestBlockNumber(context.Background())
		require.NoErrorf(t, err, "an unsupported method is not a poll failure (call %d)", i)
		assert.Equal(t, int64(0), got)
	}
	require.Equal(t, 10, up.callCount("eth_getBlockByNumber"))

	// Discriminating: the eleventh call must not reach the upstream at all. A
	// poller that only logged the failures would keep paying for the request on
	// every cycle, forever.
	time.Sleep(2 * time.Millisecond)
	_, err := p.PollLatestBlockNumber(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 10, up.callCount("eth_getBlockByNumber"),
		"after ten consecutive unsupported answers the poller must stop asking")
}

func TestPollLatestBlockNumber_AMajorJumpOnAMismatchedChainIsDropped(t *testing.T) {
	head := int64(1000)
	var mu sync.Mutex
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		return jsonResult(req, blockHeader(head))
	})
	p := newGateTestPoller(t, up)
	p.SetNetworkConfig(&common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123, FallbackStatePollerDebounce: common.Duration(time.Nanosecond)},
	})

	got, err := p.PollLatestBlockNumber(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1000), got, "a cold-start poll needs no identity check")

	// The endpoint now answers for a different chain, at a much higher height.
	mu.Lock()
	head = 1000 + common.DefaultToleratedBlockHeadRollback + 5000
	mu.Unlock()
	up.setChainId("999", nil)

	time.Sleep(2 * time.Millisecond)
	_, err = p.PollLatestBlockNumber(context.Background())

	require.NoError(t, err, "a rejected sample is not a poll failure")
	require.Equal(t, 2, up.callCount("eth_getBlockByNumber"), "the second poll must really have re-fetched")
	// Discriminating: the shared counter must NOT move. It is what every
	// lag-based routing decision reads, so a swallowed sample here would send
	// traffic to an upstream that is thousands of blocks behind the tip it
	// claims.
	assert.Equal(t, int64(1000), p.LatestBlock())
	reasons := up.cordonReasons()
	require.NotEmpty(t, reasons, "a proven cross-wired endpoint must be cordoned")
	assert.True(t, strings.Contains(reasons[0], "chain identity mismatch"), reasons[0])
}

// --- verifyThenSuggestLatestBlock ---

// countingChainIdUpstream records every identity lookup the poller makes.
type countingChainIdUpstream struct {
	*forwardingUpstream
	mu    sync.Mutex
	calls int
}

func (u *countingChainIdUpstream) EvmGetChainId(ctx context.Context) (string, error) {
	u.mu.Lock()
	u.calls++
	u.mu.Unlock()
	return u.forwardingUpstream.EvmGetChainId(ctx)
}

func (u *countingChainIdUpstream) chainIdCalls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

func TestVerifyThenSuggestLatestBlock_OnlyOneVerificationRunsAtATime(t *testing.T) {
	up := &countingChainIdUpstream{forwardingUpstream: newForwardingUpstream(123)}
	p := newGateTestPoller(t, up)
	p.SuggestLatestBlock(1_000)
	require.Equal(t, int64(1_000), p.LatestBlock())

	dropped := int64(1_000 + common.DefaultToleratedBlockHeadRollback + 9_000)
	kept := int64(1_000 + common.DefaultToleratedBlockHeadRollback + 5_000)

	// Hold the verification lock, exactly as an in-flight verification would.
	p.latestMajorVerifyInProgress.Lock()
	p.verifyThenSuggestLatestBlock(dropped)
	p.latestMajorVerifyInProgress.Unlock()

	// A second, uncontended suggestion. Taking the lock afterwards joins its
	// goroutine without a margin — it releases the lock on exit.
	p.verifyThenSuggestLatestBlock(kept)
	p.latestMajorVerifyInProgress.Lock()
	p.latestMajorVerifyInProgress.Unlock()

	// Discriminating: the contended suggestion names a HIGHER block than the
	// one that ran. A poller that queued it instead of dropping it would leave
	// the higher value in the counter, and would pay for a second identity
	// check to get there.
	assert.Equal(t, kept, p.LatestBlock())
	assert.Equal(t, 1, up.chainIdCalls(), "one verification at a time means one identity check")
}

func TestVerifyThenSuggestLatestBlock_ASuggestionOvertakenWhileQueuedIsDropped(t *testing.T) {
	up := &countingChainIdUpstream{forwardingUpstream: newForwardingUpstream(123)}
	p := newGateTestPoller(t, up)
	// Another path already advanced the head past the queued suggestion.
	p.SuggestLatestBlock(9_000)
	require.Equal(t, int64(9_000), p.LatestBlock())

	p.verifyThenSuggestLatestBlock(1_000)

	// Wait for the verification goroutine by taking the lock it releases on
	// exit — no margin, no polling.
	p.latestMajorVerifyInProgress.Lock()
	p.latestMajorVerifyInProgress.Unlock()

	assert.Equal(t, int64(9_000), p.LatestBlock(), "a stale suggestion must never move the head backwards")
	// Discriminating: the identity check costs a live eth_chainId call on every
	// major move, and 9000→1000 IS a major move. A suggestion already overtaken
	// must be re-read and dropped BEFORE paying for one.
	assert.Equal(t, 0, up.chainIdCalls())
}

// --- the earliest-bound scheduler ---

// prunedUpstream answers the "latest" tag with a header and every concrete
// height with null, so the binary search converges on nothing.
func prunedUpstream(t *testing.T, head int64) (*forwardingUpstream, chan struct{}) {
	t.Helper()
	probed := make(chan struct{}, 1)
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		if _, ok := requestedBlockNumber(t, req); !ok {
			return jsonResult(req, blockHeader(head))
		}
		select {
		case probed <- struct{}{}:
		default:
		}
		return jsonResult(req, `null`)
	})
	plus := int64(0)
	up.cfg.Evm.BlockAvailability = &common.EvmBlockAvailabilityConfig{
		Lower: &common.EvmAvailabilityBoundConfig{
			EarliestBlockPlus: &plus,
			Probe:             common.EvmProbeBlockHeader,
		},
	}
	return up, probed
}

func TestInitializeEarliestBlockDetection_ASearchThatFindsNothingLeavesTheBoundUnknown(t *testing.T) {
	up, _ := prunedUpstream(t, 9_000)
	p := newGateTestPoller(t, up)

	p.initializeEarliestBlockDetectionAndStartScheduler(context.Background())

	require.Greater(t, up.callCount("eth_getBlockByNumber"), 2, "the search must really have run")
	// Discriminating: the bound must stay 0 (unknown, fail open). Reporting the
	// converged value would name the TIP as the earliest retained block, which
	// is the most restrictive bound possible and would reject every historical
	// request the upstream can actually serve.
	assert.Equal(t, int64(0), p.EarliestBlock(common.EvmProbeBlockHeader))
	p.earliestMu.RLock()
	defer p.earliestMu.RUnlock()
	assert.False(t, p.earliestInitialDetectionDone[common.EvmProbeBlockHeader],
		"a failed search must leave the probe retryable on the next cycle")
}

func TestRunPeriodicEarliestBlockBoundUpdateLoop_KeepsProbingUntilTheAppContextEnds(t *testing.T) {
	up, probed := prunedUpstream(t, 9_000)
	p := newGateTestPoller(t, up)

	appCtx, cancel := context.WithCancel(context.Background())
	p.appCtx = appCtx

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		p.runPeriodicEarliestBlockBoundUpdateLoop(common.EvmProbeBlockHeader, time.Millisecond)
	}()

	// Wait for a real probe rather than for a duration.
	<-probed

	cancel()
	// Discriminating: the loop must exit on the app context. A loop that only
	// watched its ticker would leak one goroutine per upstream for the life of
	// the process, and this receive would block until the test times out.
	<-stopped
}

// --- the availability probes on a nil/nil answer ---

// nilAnsweringUpstream answers eth_getBlockByNumber with a real header (so the
// probes get past their block-hash lookup) and every other method with the
// (nil, nil) pair Upstream.Forward itself logs as "nil response and nil error".
func nilAnsweringUpstream(t *testing.T) *forwardingUpstream {
	t.Helper()
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResult(req, blockHeader(10))
	})
	up.onFallback(func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return nil, nil
	})
	return up
}

func TestCheckProbe_ANilAnswerReadsAsNotAvailableAndNotAsUnsupported(t *testing.T) {
	for _, probe := range []common.EvmAvailabilityProbeType{
		common.EvmProbeEventLogs,
		common.EvmProbeCallState,
		common.EvmProbeTraceData,
	} {
		t.Run(string(probe), func(t *testing.T) {
			up := nilAnsweringUpstream(t)
			p := newGateTestPoller(t, up)

			ok, unsupported, err := p.checkProbe(context.Background(), probe, 10)

			require.NoError(t, err)
			assert.False(t, ok, "no body means no evidence the height is retained")
			// Discriminating: "unsupported" ends the whole binary search for
			// this probe, at every height at once. A blank answer says nothing
			// about method support, so it must stay a per-height "no".
			assert.False(t, unsupported)
		})
	}
}

// --- verifyChainIdOnMajorHeadMove ---

func TestVerifyChainIdOnMajorHeadMove_ALargeBackwardMoveIsCheckedToo(t *testing.T) {
	up := newForwardingUpstream(123)
	up.setChainId("999", nil)
	p := newGateTestPoller(t, up)

	// A head that fell thousands of blocks: the same evidence of a cross-wired
	// endpoint as a jump forward, seen from the other side.
	ok := p.verifyChainIdOnMajorHeadMove(
		context.Background(), "latest",
		10_000+common.DefaultToleratedBlockHeadRollback+5_000, 10_000)

	// Discriminating: only an absolute distance catches this. A check written
	// as `polled-current > threshold` reads a rollback as a negative number,
	// passes it, and lets the wrong chain's height into the counter.
	assert.False(t, ok)
	require.NotEmpty(t, up.cordonReasons())
}

func TestVerifyChainIdOnMajorHeadMove_AnUpstreamWithNoChainIdIsNotSecondGuessed(t *testing.T) {
	up := newForwardingUpstream(123)
	up.setChainId("", errors.New("dial tcp: connection refused"))
	p := newGateTestPoller(t, up)

	ok := p.verifyChainIdOnMajorHeadMove(
		context.Background(), "latest",
		1_000, 1_000+common.DefaultToleratedBlockHeadRollback+5_000)

	// Discriminating: an unverifiable sample is DROPPED but the upstream is
	// not cordoned. Cordoning on a transient eth_chainId failure would take a
	// healthy upstream out of rotation until an admin put it back by hand.
	assert.False(t, ok)
	assert.Empty(t, up.cordonReasons())
}
