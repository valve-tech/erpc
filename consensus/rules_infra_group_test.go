package consensus

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A participant can produce a response that carries neither a result nor an
// error: `inner` returned (nil, nil). classifyAndHashResponse files it under
// ResponseTypeInfrastructureError with the hash "error:generic", but the group
// it lands in has no FirstError, because nothing failed. Two consensus rules
// read FirstError off such a group and both are reached from real
// configurations, so this file pins what each one serves.
//
// Every other consensus rule reads FirstError only after its condition proved
// a real error exists.

// emptyResult builds the response an inner function produces when it returns
// (nil, nil): no payload and no error.
func emptyResult(ups common.Upstream, index int) *execResult {
	return &execResult{Upstream: ups, Index: index}
}

// TestRule_AllParticipantsAnsweredWithNothingReportsLowParticipants covers the
// case where every upstream produced a result-less, error-less response.
//
// The "identical infrastructure errors" rule matches, because those responses
// all hash the same and none of them counts as a valid participant. Its action
// then has no error to return. It must fall back to low-participants rather
// than serve an error-free, result-free winner: low-participants is exactly
// what happened — nobody answered.
func TestRule_AllParticipantsAnsweredWithNothingReportsLowParticipants(t *testing.T) {
	cfg := &config{maxParticipants: 2, agreementThreshold: 2}
	u1, u2 := taggedUpstream("u1"), taggedUpstream("u2")

	a := analyze(cfg, []*execResult{
		emptyResult(u1, 0),
		emptyResult(u2, 1),
	})

	require.Zero(t, a.validParticipants, "a result-less response is not a valid participant")
	requireRule(t, a, "all participants have identical infrastructure errors meeting threshold -> return error", 0)

	sr := winnerOf(cfg, a)
	requireLowParticipants(t, sr)
	assert.Nil(t, sr.Result, "no upstream produced a payload to serve")
}

// TestRule_LowParticipantsAcceptMostCommonServesTheRealError pins the fix for
// upstream bug 69.
//
// getBestError used to rank the result-less group alongside the groups that
// hold an error. Here the result-less group is the larger one, so it won the
// ranking and the rule returned &slotResult{Error: nil} with no Result.
// (*executor).Run then handed the network layer (nil, nil).
//
// getBestError now skips a group that holds no error, so the rule serves one
// of the two reverts instead. The client gets an error it can read.
func TestRule_LowParticipantsAcceptMostCommonServesTheRealError(t *testing.T) {
	cfg := &config{
		maxParticipants:         4,
		agreementThreshold:      3,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
	}
	u1, u2, u3, u4 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3"), taggedUpstream("u4")

	// Two upstreams answered with nothing at all (one group, count 2).
	// Two more reverted with different codes (two groups, count 1 each), so no
	// consensus-valid group has a unique lead and the earlier unique-leader
	// rule cannot claim the round.
	a := analyze(cfg, []*execResult{
		emptyResult(u1, 0),
		emptyResult(u2, 1),
		errorFrom(u3, codedRevert(-32000), 2),
		errorFrom(u4, codedRevert(-32001), 3),
	})

	require.Equal(t, 2, a.validParticipants)
	require.Len(t, a.getValidGroups(), 2, "the two reverts are the only valid groups")
	requireRule(t, a, "low participants + accept-most-common: return best valid by priority and consider non-empty ties as dispute", 0)

	sr := winnerOf(cfg, a)
	require.NotNil(t, sr)
	assert.Nil(t, sr.Result, "no upstream produced a payload")
	require.Error(t, sr.Error, "the winner must carry an error, or Run returns (nil, nil)")
	assert.Contains(t, sr.Error.Error(), "execution reverted",
		"the round saw real reverts, so one of them is the honest answer")
}
