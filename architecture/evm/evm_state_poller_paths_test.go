package evm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the state-poller paths that need a REAL response body:
// the finalized ratchet, eth_syncing parsing, the block fetch decoder, the
// debounce resolver and the one-time earliest-bound bootstrap. They all use
// forwardingUpstream (see forwarding_upstream_test.go), because a double that
// answers (nil, nil) leaves every body-reading branch dark.
//
// Where a branch is reached by more than one route, the test asserts the
// discriminating property — the requests the poller actually sent, or the
// cause string it kept — rather than a return value a second route also
// produces.

// --- PollFinalizedBlockNumber ---

func TestPollFinalizedBlockNumber_StoresTheHeadAndAsksForTheFinalizedTag(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResult(req, blockHeader(500))
	})
	p := newGateTestPoller(t, up)

	got, err := p.PollFinalizedBlockNumber(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(500), got)
	assert.Equal(t, int64(500), p.FinalizedBlock(), "the poll must land in the shared counter")

	// Discriminating: the poll must ask for the "finalized" tag. A poller that
	// asked for "latest" would store the same 500 here.
	calls := up.methodCalls("eth_getBlockByNumber")
	require.Len(t, calls, 1)
	assert.Contains(t, calls[0], `"finalized"`)
}

func TestPollFinalizedBlockNumber_GivesUpAfterTenUnsupportedAnswers(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return nil, common.NewErrEndpointUnsupported(errors.New("no finalized tag on this node"))
	})
	p := newGateTestPoller(t, up)
	// Drop the debounce to a tick so each call really re-fetches.
	p.SetNetworkConfig(&common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123, FallbackStatePollerDebounce: common.Duration(time.Nanosecond)},
	})

	for i := 0; i < 10; i++ {
		// The shared counter stamps its freshness in whole milliseconds, so a
		// tight loop would be debounced away no matter how small the interval.
		time.Sleep(2 * time.Millisecond)
		got, err := p.PollFinalizedBlockNumber(context.Background())
		require.NoErrorf(t, err, "an unsupported finalized tag is not a poll failure (call %d)", i)
		assert.Equal(t, int64(0), got)
	}
	require.Equal(t, 10, up.callCount("eth_getBlockByNumber"))

	// Discriminating: the eleventh call must not reach the upstream at all.
	// A poller that only logged the failures would keep forwarding forever.
	_, err := p.PollFinalizedBlockNumber(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 10, up.callCount("eth_getBlockByNumber"),
		"after ten consecutive unsupported answers the poller must stop asking")
}

func TestPollFinalizedBlockNumber_TransportFailureSurfacesTheCause(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return nil, errors.New("dial tcp: connection refused")
	})
	p := newGateTestPoller(t, up)

	_, err := p.PollFinalizedBlockNumber(context.Background())
	require.Error(t, err, "a transport failure is a real poll failure, not an unsupported method")
	assert.Contains(t, err.Error(), "connection refused")
}

// --- fetchBlock ---

func TestFetchBlock_NullResultIsNotAnError(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResult(req, `null`)
	})
	p := newGateTestPoller(t, up)

	num, ts, err := p.fetchBlock(context.Background(), "finalized")
	require.NoError(t, err, "a node that has no finalized block yet answers null; that is not a failure")
	assert.Equal(t, int64(0), num)
	assert.Equal(t, int64(0), ts)
}

func TestFetchBlock_BlockWithoutANumberIsRejectedWithTheResultAttached(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResult(req, `{"hash":"0xdead","timestamp":"0x10"}`)
	})
	p := newGateTestPoller(t, up)

	_, _, err := p.fetchBlock(context.Background(), "latest")
	require.Error(t, err)
	// Discriminating: the offending body must travel with the error, otherwise
	// an operator cannot tell this apart from a null result.
	var base *common.BaseError
	require.True(t, errors.As(err, &base))
	assert.Equal(t, common.ErrorCode("ErrEvmStatePoller"), base.Code)
	assert.Contains(t, fmt.Sprintf("%v", base.Details["result"]), "0xdead")
}

func TestFetchBlock_NonHexBlockNumberIsAnError(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResult(req, `{"number":"not-a-number","hash":"0xabc"}`)
	})
	p := newGateTestPoller(t, up)

	_, _, err := p.fetchBlock(context.Background(), "latest")
	require.Error(t, err, "a block number that is not hex must not be read as zero")
}

func TestFetchBlock_UnparsableTimestampKeepsTheBlockNumber(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResult(req, `{"number":"0x2a","hash":"0xabc","timestamp":"zzz"}`)
	})
	p := newGateTestPoller(t, up)

	num, ts, err := p.fetchBlock(context.Background(), "latest")
	require.NoError(t, err, "a broken timestamp must not discard a good block number")
	assert.Equal(t, int64(42), num)
	assert.Equal(t, int64(0), ts)
}

// --- fetchSyncingState ---

// syncingPoller wires one eth_syncing answer to a fresh poller.
func syncingPoller(t *testing.T, h forwardHandler) *EvmStatePoller {
	t.Helper()
	up := newForwardingUpstream(123)
	up.on("eth_syncing", h)
	return newGateTestPoller(t, up)
}

func syncingResult(raw string) forwardHandler {
	return func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResult(req, raw)
	}
}

func TestFetchSyncingState_ReadsEveryShapeNodesActuallyReturn(t *testing.T) {
	cases := []struct {
		name    string
		result  string
		syncing bool
	}{
		{"BareTrue", `true`, true},
		{"BareFalse", `false`, false},
		{"GethObjectWithCurrentBlock", `{"currentBlock":"0x10","highestBlock":"0x20"}`, true},
		{"ArbitrumObjectWithMsgCount", `{"msgCount":42}`, true},
		{"NonStandardOkTrueMeansSynced", `{"Ok":true}`, false},
		{"NonStandardOkFalseMeansSyncing", `{"Ok":false}`, true},
		{"NonStandardLowercaseOk", `{"ok":false}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := syncingPoller(t, syncingResult(tc.result))
			got, err := p.fetchSyncingState(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.syncing, got)
		})
	}
}

func TestFetchSyncingState_AnObjectWithNoKnownKeyIsRejected(t *testing.T) {
	p := syncingPoller(t, syncingResult(`{"stage":"headers"}`))

	_, err := p.fetchSyncingState(context.Background())
	require.Error(t, err, "an unrecognised object must not be reported as 'not syncing'")
	var base *common.BaseError
	require.True(t, errors.As(err, &base))
	assert.Equal(t, common.ErrorCode("ErrEvmStatePoller"), base.Code)
}

func TestFetchSyncingState_UnparsableResultIsRejected(t *testing.T) {
	p := syncingPoller(t, syncingResult(`{"currentBlock":`))

	_, err := p.fetchSyncingState(context.Background())
	require.Error(t, err, "a truncated body must not be read as 'not syncing'")
	// Discriminating: the poller must translate the decoder's complaint into its
	// own typed error carrying the offending body. Passing the raw decoder error
	// through would satisfy require.Error but leave an operator with no body to
	// look at.
	var base *common.BaseError
	require.True(t, errors.As(err, &base))
	assert.Equal(t, common.ErrorCode("ErrEvmStatePoller"), base.Code)
	assert.Contains(t, fmt.Sprintf("%s", base.Details["result"]), "currentBlock")
}

func TestFetchSyncingState_ErrorMemberBesideAResultIsHonoured(t *testing.T) {
	// A 200-OK carrying BOTH a populated result and an error member. Only this
	// shape separates "the reader honoured the error member" from "the result
	// happened to be empty".
	p := syncingPoller(t, func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResultBesideError(req, `true`, -32000, "node is restarting")
	})

	got, err := p.fetchSyncingState(context.Background())
	require.Error(t, err, "the error member wins over the result member")
	assert.Contains(t, err.Error(), "node is restarting")
	assert.False(t, got)
}

// --- resolveDebounce ---

func TestResolveDebounce_PrefersTheMeasuredBlockTimeOverTheFallback(t *testing.T) {
	up := newForwardingUpstream(123)
	p := newGateTestPoller(t, up)

	// Feed the tracker four block observations two seconds apart, which is what
	// its EMA needs before it reports a block time at all.
	for i := int64(1); i <= 4; i++ {
		p.tracker.SetLatestBlockNumber(up, i, 1_700_000_000+i*2)
	}
	require.Equal(t, 2*time.Second, p.tracker.GetNetworkBlockTime(up.NetworkId()),
		"the fixture must warm the EMA up, otherwise this test cannot see the branch")

	got := p.resolveDebounce(&common.EvmNetworkConfig{FallbackStatePollerDebounce: common.Duration(9 * time.Second)})
	want := time.Duration(float64(2*time.Second) * common.DefaultDynamicBlockTimeDebounceMultiplier)
	assert.Equal(t, want, got, "a known block time must beat the configured fallback")
}

func TestResolveDebounce_HonoursTheConfiguredBlockTimeMultiplier(t *testing.T) {
	up := newForwardingUpstream(123)
	p := newGateTestPoller(t, up)
	for i := int64(1); i <= 4; i++ {
		p.tracker.SetLatestBlockNumber(up, i, 1_700_000_000+i*2)
	}
	require.Equal(t, 2*time.Second, p.tracker.GetNetworkBlockTime(up.NetworkId()))

	mult := 0.25
	got := p.resolveDebounce(&common.EvmNetworkConfig{DynamicBlockTimeDebounceMultiplier: &mult})
	assert.Equal(t, 500*time.Millisecond, got)
}

func TestResolveDebounce_FallsBackToConfigThenToOneSecond(t *testing.T) {
	p := newGateTestPoller(t, newForwardingUpstream(123))

	assert.Equal(t, 7*time.Second,
		p.resolveDebounce(&common.EvmNetworkConfig{FallbackStatePollerDebounce: common.Duration(7 * time.Second)}))
	assert.Equal(t, 1*time.Second, p.resolveDebounce(nil),
		"with no block time and no config the poller must still debounce")
}

// --- initializeEarliestBlockDetectionAndStartScheduler ---

// earliestBoundUpstream answers headers from `from` upwards and null below it,
// and declares a lower bound anchored on the earliest available block.
func earliestBoundUpstream(t *testing.T, from int64, updateRate time.Duration) *forwardingUpstream {
	t.Helper()
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		block, ok := requestedBlockNumber(t, req)
		if !ok {
			return jsonResult(req, blockHeader(9_000)) // the "latest" tag
		}
		if block < from {
			return jsonResult(req, `null`)
		}
		return jsonResult(req, blockHeader(block))
	})
	plus := int64(0)
	up.cfg.Evm.BlockAvailability = &common.EvmBlockAvailabilityConfig{
		Lower: &common.EvmAvailabilityBoundConfig{
			EarliestBlockPlus: &plus,
			Probe:             common.EvmProbeBlockHeader,
			UpdateRate:        common.Duration(updateRate),
		},
	}
	return up
}

func TestInitializeEarliestBlockDetection_BoundWithNoEarliestAnchorIsNotScheduled(t *testing.T) {
	up := newForwardingUpstream(123)
	minus := int64(128)
	up.cfg.Evm.BlockAvailability = &common.EvmBlockAvailabilityConfig{
		Upper: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: &minus},
	}
	p := newGateTestPoller(t, up)

	p.initializeEarliestBlockDetectionAndStartScheduler(context.Background())

	// Discriminating: a latestBlockMinus bound needs no earliest search, so the
	// poller must not spend a single request on one.
	assert.Empty(t, up.allCalls(), "only an earliestBlockPlus bound may trigger the binary search")
	assert.Equal(t, int64(0), p.EarliestBlock(common.EvmProbeBlockHeader))
}

func TestInitializeEarliestBlockDetection_DetectsTheBoundOnceAndThenStopsSearching(t *testing.T) {
	up := earliestBoundUpstream(t, 100, 0)
	p := newGateTestPoller(t, up)

	p.initializeEarliestBlockDetectionAndStartScheduler(context.Background())
	require.Equal(t, int64(100), p.EarliestBlock(common.EvmProbeBlockHeader))
	firstRound := up.callCount("eth_getBlockByNumber")
	require.Greater(t, firstRound, 1, "the binary search must have probed several blocks")

	// Wait past the one-millisecond staleness the detection call passes down.
	// Without this the shared counter would debounce the second search anyway,
	// and the test could not see whether the done-flag did its job.
	time.Sleep(5 * time.Millisecond)
	p.initializeEarliestBlockDetectionAndStartScheduler(context.Background())

	// Discriminating: the second call must be free. Counting the bound alone
	// would pass even if the whole search ran again.
	assert.Equal(t, firstRound, up.callCount("eth_getBlockByNumber"),
		"initial detection is once per instance, not once per Poll cycle")
}

func TestInitializeEarliestBlockDetection_ARunningSchedulerSkipsTheProbeEntirely(t *testing.T) {
	// A rate far beyond the test's lifetime: the loop must start, but its
	// ticker must never fire, so the assertion stays deterministic.
	up := earliestBoundUpstream(t, 100, time.Hour)
	p := newGateTestPoller(t, up)

	p.initializeEarliestBlockDetectionAndStartScheduler(context.Background())
	require.Equal(t, int64(100), p.EarliestBlock(common.EvmProbeBlockHeader))
	firstRound := up.callCount("eth_getBlockByNumber")

	time.Sleep(5 * time.Millisecond)
	p.initializeEarliestBlockDetectionAndStartScheduler(context.Background())

	// Discriminating: with detection done AND the scheduler running, the probe
	// must be skipped before any work — no second binary search.
	assert.Equal(t, firstRound, up.callCount("eth_getBlockByNumber"))
	p.earliestMu.RLock()
	defer p.earliestMu.RUnlock()
	assert.True(t, p.earliestInitialDetectionDone[common.EvmProbeBlockHeader])
}

func TestInitializeEarliestBlockDetection_OnlyAPositiveUpdateRateStartsAScheduler(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rate    time.Duration
		started bool
	}{
		{"NoRateMeansOneShotDetection", 0, false},
		{"PositiveRateStartsTheLoop", time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := earliestBoundUpstream(t, 100, tc.rate)
			p := newGateTestPoller(t, up)

			p.initializeEarliestBlockDetectionAndStartScheduler(context.Background())

			require.Equal(t, int64(100), p.EarliestBlock(common.EvmProbeBlockHeader))
			p.earliestMu.RLock()
			defer p.earliestMu.RUnlock()
			assert.Equal(t, tc.started, p.earliestSchedulerStarted[common.EvmProbeBlockHeader])
		})
	}
}

func TestInitializeEarliestBlockDetection_HeadFailureSkipsTheCycleBeforeAnySearch(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		if _, ok := requestedBlockNumber(t, req); !ok {
			return nil, errors.New("head unavailable")
		}
		return jsonResult(req, blockHeader(1))
	})
	plus := int64(0)
	up.cfg.Evm.BlockAvailability = &common.EvmBlockAvailabilityConfig{
		Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: &plus},
	}
	p := newGateTestPoller(t, up)

	p.initializeEarliestBlockDetectionAndStartScheduler(context.Background())

	// Discriminating: the search must not start, and the detection flag must
	// stay clear so the next Poll cycle retries.
	assert.Equal(t, 1, up.callCount("eth_getBlockByNumber"),
		"without a head there is nothing to bisect against")
	p.earliestMu.RLock()
	defer p.earliestMu.RUnlock()
	assert.False(t, p.earliestInitialDetectionDone[common.EvmProbeBlockHeader],
		"a failed head fetch must leave the probe retryable")
}

func TestInitializeEarliestBlockDetection_NullHeadSkipsTheCycle(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResult(req, `null`)
	})
	plus := int64(0)
	up.cfg.Evm.BlockAvailability = &common.EvmBlockAvailabilityConfig{
		Lower: &common.EvmAvailabilityBoundConfig{EarliestBlockPlus: &plus},
	}
	p := newGateTestPoller(t, up)

	p.initializeEarliestBlockDetectionAndStartScheduler(context.Background())

	assert.Equal(t, 1, up.callCount("eth_getBlockByNumber"))
	assert.Equal(t, int64(0), p.EarliestBlock(common.EvmProbeBlockHeader))
	p.earliestMu.RLock()
	defer p.earliestMu.RUnlock()
	assert.False(t, p.earliestInitialDetectionDone[common.EvmProbeBlockHeader],
		"a null head is a retryable cycle, not a completed detection")
}

func TestInitializeEarliestBlockDetection_AnUnnamedProbeSharesTheBlockHeaderSchedule(t *testing.T) {
	// Lower names no probe, Upper names blockHeader explicitly. An unnamed
	// probe defaults to blockHeader, so both bounds must collapse onto ONE
	// schedule — one scheduler and one binary search, not two.
	up := earliestBoundUpstream(t, 100, time.Hour)
	up.cfg.Evm.BlockAvailability.Lower.Probe = ""
	plus := int64(0)
	up.cfg.Evm.BlockAvailability.Upper = &common.EvmAvailabilityBoundConfig{
		EarliestBlockPlus: &plus,
		Probe:             common.EvmProbeBlockHeader,
		UpdateRate:        common.Duration(2 * time.Hour),
	}
	p := newGateTestPoller(t, up)

	p.initializeEarliestBlockDetectionAndStartScheduler(context.Background())

	assert.Equal(t, int64(100), p.EarliestBlock(common.EvmProbeBlockHeader))
	p.earliestMu.RLock()
	defer p.earliestMu.RUnlock()
	assert.Len(t, p.earliestSchedulerStarted, 1, "one effective probe means one scheduler")
	assert.Len(t, p.earliestInitialDetectionDone, 1, "and one detection, not one per bound")
}

// --- fetchBlockHashByNumber ---

func TestFetchBlockHashByNumber_TransportFailureReadsAsNotAvailable(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return nil, errors.New("dial tcp: connection refused")
	})
	p := newGateTestPoller(t, up)

	hash, ok, err := p.fetchBlockHashByNumber(context.Background(), 10)
	require.NoError(t, err, "a probe helper must never surface transport errors to the binary search")
	assert.False(t, ok)
	assert.Empty(t, hash)
}

func TestFetchBlockHashByNumber_ErrorMemberBesideAHashReadsAsNotAvailable(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResultBesideError(req, blockHeader(10), -32000, "pruned")
	})
	p := newGateTestPoller(t, up)

	hash, ok, _ := p.fetchBlockHashByNumber(context.Background(), 10)
	// Discriminating: the body DOES carry a usable hash. Only a reader that
	// honours the error member returns "not available" here.
	assert.False(t, ok, "an error member must veto the hash beside it")
	assert.Empty(t, hash)
	require.NotEmpty(t, up.methodCalls("eth_getBlockByNumber"))
	assert.True(t, strings.Contains(up.methodCalls("eth_getBlockByNumber")[0], `"0xa"`),
		"the probe must ask for the block it was given")
}
