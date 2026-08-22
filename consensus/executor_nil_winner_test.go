package consensus

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A participant can hand consensus a response that carries no readable
// JSON-RPC payload and no error. classifyAndHashResponse has a branch for it
// (analysis.go, "Successful response" -> jr == nil): it files the response
// under ResponseTypeInfrastructureError with the hash "error:generic". The
// group that response lands in holds no FirstError, because nothing failed.
//
// getBestError ranked such a group alongside the groups that DO hold an error.
// When the error-free group won the ranking, the low-participants +
// accept-most-common rule returned a winner with a nil Error and a nil Result,
// and (*executor).Run handed the network layer (nil, nil). The client saw an
// empty body and no explanation.
//
// Logged as upstream bug 69. This test drives the whole executor, because
// (nil, nil) at the Run boundary is the fault an operator meets.

// unreadableResponse returns a NormalizedResponse that carries no readable
// payload. Release() frees the parsed payload and clears the cached pointer,
// so a later read has nothing to hand back. The consensus executor releases
// responses itself, so this is a shape the analysis really can meet.
//
// This fixture used to assert that the read after the release returned
// (nil, nil). That assertion pinned bug 76: a released response answered
// exactly like an absent one. The read now reports ErrResponseReleased. The
// bug-69 shape survives, because resultToJsonRpcResponse (analysis.go) drops
// the error and passes the nil payload straight to classifyAndHashResponse,
// which files it under ResponseTypeInfrastructureError with no FirstError.
func unreadableResponse(t *testing.T) *common.NormalizedResponse {
	t.Helper()
	r := common.NewNormalizedResponse().WithBody(
		io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":["0x1"]}`)),
	)
	_, err := r.JsonRpcResponse(context.Background())
	require.NoError(t, err)
	r.Release()

	jrr, err := r.JsonRpcResponse(context.Background())
	require.ErrorIs(t, err, common.ErrResponseReleased,
		"a released response must name the release")
	require.Nil(t, jrr, "the fixture must really produce a payload-free response")
	return r
}

// TestConsensus_PayloadFreeParticipantsDoNotProduceANilNilAnswer builds the
// exact round the bug needs.
//
// Two upstreams answer with a response nothing can read, so they share one
// error-free infrastructure group of count 2. Two more revert with different
// codes, so they form two consensus-valid groups of count 1 each and no group
// leads uniquely. getBestError used to pick the count-2 group and read its nil
// FirstError.
//
// Run must answer with an error. An error the operator can read beats a silent
// nil, and this round really did see reverts, so a revert is the honest answer.
func TestConsensus_PayloadFreeParticipantsDoNotProduceANilNilAnswer(t *testing.T) {
	logger := zerolog.New(zerolog.NewTestWriter(t))

	pol := newBuilder().
		WithLogger(&logger).
		WithMaxParticipants(4).
		WithAgreementThreshold(3).
		WithLowParticipantsBehavior(common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult).
		Build()

	req := newTestRequest()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, common.RequestContextKey, req)

	var slot atomic.Int32
	resp, err := pol.Run(ctx, req, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		switch slot.Add(1) {
		case 1, 2:
			return unreadableResponse(t), nil
		case 3:
			return nil, codedRevert(-32000)
		default:
			return nil, codedRevert(-32001)
		}
	})

	require.Equal(t, int32(4), slot.Load(), "every participant slot must have run")
	assert.Nil(t, resp, "no participant produced a readable payload")
	require.Error(t, err, "bug 69: the round must not answer with neither a response nor an error")
	assert.Contains(t, err.Error(), "execution reverted",
		"the round saw real reverts, so the caller must get one of them")
}
