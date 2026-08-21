package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An upstream can end a request with no response and no error. Upstream.Forward
// logs that pair by name — "upstream request ended with nil response and nil
// error" — and returns (nil, nil) to its caller. NormalizedResponse.
// JsonRpcResponse answers (nil, nil) for a nil receiver, so the state poller
// reads a nil *JsonRpcResponse.
//
// The two poll helpers below used to dereference that nil inside the very guard
// that tested for it. The panic happened in the Poll fan-out goroutine, which
// has no recover, so the process died. These tests drive the real helpers with
// a real (nil, nil) answer and pin what each one returns instead.
//
// Logged as upstream bug 66.

// nilAnswerUpstream answers one method with (nil, nil) and every other method
// with the double's normal unsupported error.
func nilAnswerUpstream(method string) *forwardingUpstream {
	up := newForwardingUpstream(123)
	up.on(method, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return nil, nil
	})
	return up
}

// TestFetchBlock_ANilAnswerReportsNoBlockInsteadOfPanicking pins the nil-response
// path of fetchBlock.
//
// The poller must report "no block number" — the same pair a null result
// produces — because the caller already counts that as a failed poll and
// latches skipLatestBlockCheck after ten of them.
func TestFetchBlock_ANilAnswerReportsNoBlockInsteadOfPanicking(t *testing.T) {
	up := nilAnswerUpstream("eth_getBlockByNumber")
	p := newGateTestPoller(t, up)

	num, ts, err := p.fetchBlock(context.Background(), "latest")

	require.NoError(t, err, "an upstream that answered nothing is not a JSON-RPC error")
	assert.Equal(t, int64(0), num, "no answer means no block number")
	assert.Equal(t, int64(0), ts)
	require.Equal(t, 1, up.callCount("eth_getBlockByNumber"), "the poller must really have asked")
}

// TestFetchSyncingState_ANilAnswerIsAnErrorNotANotSyncingClaim pins the
// nil-response path of fetchSyncingState.
//
// This helper has no neutral return value: `false` claims the node is fully
// synced. The poller learned nothing, so it must report an error and let the
// caller count the failure.
func TestFetchSyncingState_ANilAnswerIsAnErrorNotANotSyncingClaim(t *testing.T) {
	up := nilAnswerUpstream("eth_syncing")
	p := newGateTestPoller(t, up)

	syncing, err := p.fetchSyncingState(context.Background())

	require.Error(t, err, "an empty answer must not read as not-syncing")
	assert.False(t, syncing)
	var base *common.BaseError
	require.ErrorAs(t, err, &base)
	assert.Equal(t, common.ErrorCode("ErrEvmStatePoller"), base.Code)
	require.Equal(t, 1, up.callCount("eth_syncing"), "the poller must really have asked")
}
