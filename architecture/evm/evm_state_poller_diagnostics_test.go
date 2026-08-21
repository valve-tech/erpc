package evm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GetDiagnostics is the operator's only window into why an upstream stopped
// reporting a head. It has to distinguish "the node does not support this
// method, we gave up" from "we simply have not polled yet" — those two look
// identical from the outside and lead to opposite actions.

// setPollState writes the poller's detection bookkeeping directly.
//
// The give-up counters can only be driven through the poll loop, and the
// shared counter debounces at millisecond granularity, so ten consecutive
// polls would need ten milliseconds of real time and a sleep. These tests are
// about what GetDiagnostics REPORTS for a given state, so they set the state
// and read the report — no clock involved.
func setPollState(p *EvmStatePoller, mutate func(*EvmStatePoller)) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	mutate(p)
}

func TestGetDiagnostics(t *testing.T) {
	ctx := context.Background()

	t.Run("NilPollerReportsNothing", func(t *testing.T) {
		var p *EvmStatePoller
		assert.Nil(t, p.GetDiagnostics())
	})

	t.Run("ColdPollerReportsNoIssues", func(t *testing.T) {
		// Nothing has failed yet, so no issue text may appear. A blank slate
		// that reports "gave up" would send an operator chasing a healthy node.
		up := newForwardingUpstream(123)
		diag := newGateTestPoller(t, up).GetDiagnostics()

		require.NotNil(t, diag)
		assert.False(t, diag.Enabled)
		assert.Zero(t, diag.LatestBlock)
		assert.Zero(t, diag.FinalizedBlock)
		assert.False(t, diag.SkipLatestBlockCheck)
		assert.Empty(t, diag.LatestBlockDetectionIssue)
		assert.Empty(t, diag.FinalizedBlockDetectionIssue)
		assert.Empty(t, diag.SyncingCheckError)
		assert.Nil(t, diag.EarliestByProbe)
	})

	t.Run("ReportsTheHeadsItHolds", func(t *testing.T) {
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)
		p.SuggestLatestBlock(500)
		p.SuggestFinalizedBlock(400)
		require.Eventually(t, func() bool { return p.FinalizedBlock() == 400 },
			2*time.Second, 10*time.Millisecond)

		diag := p.GetDiagnostics()

		assert.Equal(t, int64(500), diag.LatestBlock)
		assert.Equal(t, int64(400), diag.FinalizedBlock)
	})

	// UPSTREAM-CANDIDATE BUG, pinned as-is: an operator who sets
	// skipSyncingCheck on purpose is told the check "was disabled after
	// consecutive failures (method may not be supported)". The renderer folds
	// the configured skip into the same flag it uses for the give-up verdict.
	// See the report.
	t.Run("OperatorConfiguredSkipIsReportedAsAFailure", func(t *testing.T) {
		up := newForwardingUpstream(123)
		skip := true
		up.cfg.Evm.SkipSyncingCheck = &skip

		diag := newGateTestPoller(t, up).GetDiagnostics()

		assert.True(t, diag.SkipSyncingCheck)
		assert.Contains(t, diag.SyncingCheckError, "disabled after consecutive failures",
			"current behaviour: a deliberate opt-out is reported as a detection failure")
	})

	t.Run("GivingUpOnLatestIsReportedWithAReason", func(t *testing.T) {
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)
		setPollState(p, func(p *EvmStatePoller) {
			p.skipLatestBlockCheck = true
			p.latestBlockFailureCount = 10
		})

		diag := p.GetDiagnostics()

		assert.True(t, diag.SkipLatestBlockCheck)
		assert.Equal(t, 10, diag.LatestBlockFailureCount)
		assert.Contains(t, diag.LatestBlockDetectionIssue, "disabled after consecutive failures")
		// Discriminating: the finalized side must stay silent. One shared
		// message for both would send the operator after the wrong check.
		assert.Empty(t, diag.FinalizedBlockDetectionIssue)
	})

	t.Run("GivingUpAfterASuccessIsNotAnIssue", func(t *testing.T) {
		// The check worked at least once, so it IS supported — the skip is a
		// later, transient decision. Saying "method may not be supported" here
		// would be wrong.
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)
		setPollState(p, func(p *EvmStatePoller) {
			p.skipLatestBlockCheck = true
			p.latestBlockSuccessfulOnce = true
			p.skipFinalizedCheck = true
			p.finalizedBlockSuccessfulOnce = true
		})

		diag := p.GetDiagnostics()

		assert.True(t, diag.SkipLatestBlockCheck)
		assert.Empty(t, diag.LatestBlockDetectionIssue)
		assert.Empty(t, diag.FinalizedBlockDetectionIssue)
	})

	t.Run("GivingUpOnFinalizedIsReportedWithAReason", func(t *testing.T) {
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)
		setPollState(p, func(p *EvmStatePoller) {
			p.skipFinalizedCheck = true
			p.finalizedBlockFailureCount = 10
		})

		diag := p.GetDiagnostics()

		assert.True(t, diag.SkipFinalizedCheck)
		assert.Equal(t, 10, diag.FinalizedBlockFailureCount)
		assert.Contains(t, diag.FinalizedBlockDetectionIssue, "disabled after consecutive failures")
		// Discriminating: the latest-block side must be untouched.
		assert.False(t, diag.SkipLatestBlockCheck)
		assert.Empty(t, diag.LatestBlockDetectionIssue)
	})

	t.Run("ReportsASuccessfulLatestPoll", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, blockHeader(777))
		})
		p := newGateTestPoller(t, up)

		got, err := p.PollLatestBlockNumber(ctx)
		require.NoError(t, err)
		require.Equal(t, int64(777), got)

		diag := p.GetDiagnostics()
		assert.True(t, diag.LatestBlockSuccessfulOnce)
		assert.Zero(t, diag.LatestBlockFailureCount)
		assert.Equal(t, int64(777), diag.LatestBlock)
		assert.Empty(t, diag.LatestBlockDetectionIssue)
	})

	t.Run("AnUnsupportedMethodCountsAsAFailureNotAnError", func(t *testing.T) {
		// A node without eth_getBlockByNumber must not make the poll return an
		// error — the poller counts it and moves on.
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)

		got, err := p.PollLatestBlockNumber(ctx)

		require.NoError(t, err)
		assert.Zero(t, got)
		diag := p.GetDiagnostics()
		assert.Equal(t, 1, diag.LatestBlockFailureCount)
		assert.False(t, diag.SkipLatestBlockCheck, "one failure is not a verdict")
		assert.False(t, diag.LatestBlockSuccessfulOnce)
	})

	t.Run("ReportsTheEarliestBoundPerProbe", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(b int64) bool { return b >= 42 }))
		p := newGateTestPoller(t, up)
		p.SuggestLatestBlock(1_000)

		_, err := p.PollEarliestBlockNumber(ctx, common.EvmProbeBlockHeader, time.Millisecond)
		require.NoError(t, err)

		diag := p.GetDiagnostics()
		require.NotNil(t, diag.EarliestByProbe)
		info := diag.EarliestByProbe[common.EvmProbeBlockHeader]
		require.NotNil(t, info)
		assert.Equal(t, common.EvmProbeBlockHeader, info.ProbeType)
		assert.Equal(t, int64(42), info.EarliestBlock)
		// Discriminating: no periodic scheduler was configured, so reporting
		// one as running would hide a bound that never refreshes.
		assert.False(t, info.SchedulerRunning)
		assert.Len(t, diag.EarliestByProbe, 1, "only the polled probe may appear")
	})
}

func TestOnLatestBlock(t *testing.T) {
	t.Run("FiresOnEveryForwardAdvance", func(t *testing.T) {
		// The served-tip tracker registers here. A missed callback leaves it
		// pinned to an old head and routing keeps sending traffic to laggards.
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)

		seen := make(chan int64, 8)
		p.OnLatestBlock(func(v int64) { seen <- v })

		p.SuggestLatestBlock(100)
		p.SuggestLatestBlock(200)
		p.SuggestLatestBlock(150) // backwards — must be dropped, not delivered

		require.Equal(t, int64(100), <-seen)
		require.Equal(t, int64(200), <-seen)
		select {
		case v := <-seen:
			t.Fatalf("a non-advancing suggestion must not fire the callback, got %d", v)
		default:
		}
	})
}
