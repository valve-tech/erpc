package evm

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// SuggestFinalizedBlock used to route EVERY suggestion through one goroutine
// guarded by TryLock, and it published the new value BEFORE it released that
// lock. Two things followed.
//
// A caller that reacted to the value it had just observed found the lock still
// held, so its next suggestion was discarded with nothing to re-issue it. That
// window needs no concurrency: ordinary sequential code walks into it, which is
// why the race detector turned TestSuggestFinalizedBlock_MajorJumpMatchingApplies
// from flaky into a reliable failure.
//
// A small keep-fresh advance was also discarded whenever a major jump was still
// verifying its chain identity, and that check makes a live eth_chainId call.
//
// SuggestLatestBlock never behaved this way: it applies a small advance inline
// and hands only a MAJOR jump to a background verification. The finalized
// counter now uses the same shape.
//
// Logged as upstream bug 62.

// blockingChainIdUpstream holds eth_chainId open until the test opens the gate,
// so a test can keep the major-jump verification in flight and observe what
// happens to a suggestion that arrives meanwhile.
type blockingChainIdUpstream struct {
	*suggestGateUpstream
	gate    chan struct{}
	entered chan struct{}
}

func newBlockingChainIdUpstream(configuredChainId int64, detected string) *blockingChainIdUpstream {
	return &blockingChainIdUpstream{
		suggestGateUpstream: newSuggestGateUpstream(configuredChainId, detected, nil),
		gate:                make(chan struct{}),
		entered:             make(chan struct{}, 1),
	}
}

func (u *blockingChainIdUpstream) EvmGetChainId(ctx context.Context) (string, error) {
	select {
	case u.entered <- struct{}{}:
	default:
	}
	select {
	case <-u.gate:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return u.suggestGateUpstream.EvmGetChainId(ctx)
}

// TestSuggestFinalizedBlock_SmallAdvanceAppliesInline is the finalized twin of
// TestSuggestLatestBlock_SmallAdvanceAppliesInline.
//
// A suggestion that is not a major jump must land in the shared counter before
// the call returns. An asynchronous apply is what let a caller see the value
// while the lock was still held.
func TestSuggestFinalizedBlock_SmallAdvanceAppliesInline(t *testing.T) {
	up := newSuggestGateUpstream(123, "123", nil)
	p := newGateTestPoller(t, up)

	p.SuggestFinalizedBlock(1000)
	require.Equal(t, int64(1000), p.FinalizedBlock(),
		"a cold-start suggestion must apply before the call returns")

	p.SuggestFinalizedBlock(1001)
	require.Equal(t, int64(1001), p.FinalizedBlock(),
		"a small advance must apply before the call returns")
	require.False(t, up.isCordoned())
}

// TestSuggestFinalizedBlock_SmallAdvanceSurvivesAMajorJumpVerification pins the
// drop itself.
//
// The major jump parks inside eth_chainId and holds the verification lock. A
// small advance arriving meanwhile must still reach the counter: it needs no
// verification, so nothing justifies discarding it.
func TestSuggestFinalizedBlock_SmallAdvanceSurvivesAMajorJumpVerification(t *testing.T) {
	up := newBlockingChainIdUpstream(123, "123")
	p := newGateTestPoller(t, up)

	p.SuggestFinalizedBlock(1000)
	require.Eventually(t, func() bool { return p.FinalizedBlock() == 1000 },
		2*time.Second, 10*time.Millisecond)

	major := int64(1000 + gateTolerance + 1000)
	p.SuggestFinalizedBlock(major)
	select {
	case <-up.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the major jump never reached the chain-identity check")
	}

	p.SuggestFinalizedBlock(1001)
	require.Equal(t, int64(1001), p.FinalizedBlock(),
		"a small advance must not be dropped while a major jump verifies its chain identity")

	close(up.gate)
	require.Eventually(t, func() bool { return p.FinalizedBlock() == major },
		2*time.Second, 10*time.Millisecond, "the verified major jump must still apply")
	require.False(t, up.isCordoned())
}
