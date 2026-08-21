package consensus

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// analyzeWithLeader builds the analysis exactly as production does, then names
// one upstream the leader. Production derives the leader from the network's EVM
// state poller, which needs a whole network object; naming it directly keeps
// the test on the decision logic being checked.
func analyzeWithLeader(cfg *config, leader common.Upstream, responses []*execResult) *consensusAnalysis {
	analysis := analyze(cfg, responses)
	analysis.leaderUpstream = leader
	return analysis
}

// emptyResultFrom builds a result whose JSON-RPC payload is emptyish, which the
// classifier files under ResponseTypeEmpty.
func emptyResultFrom(t *testing.T, ups common.Upstream, index int) *execResult {
	t.Helper()
	jrpc, err := common.NewJsonRpcResponse(1, nil, nil)
	require.NoError(t, err)
	return &execResult{
		Result:   common.NewNormalizedResponse().WithJsonRpcResponse(jrpc),
		Upstream: ups,
		Index:    index,
	}
}

func revertError() error {
	return common.NewErrEndpointExecutionException(
		common.NewErrJsonRpcExceptionInternal(3, 3, "execution reverted", nil, nil),
	)
}

func infraError() error {
	return common.NewErrEndpointServerSideException(nil, nil, 500)
}

// TestGetLeaderGroupNonError_FindsTheLeadersSuccessfulAnswer covers the
// leader-preferring behaviours. The leader is the upstream with the highest
// known block, so when erpc is told to prefer it, it must actually find the
// leader's answer and not fall back to whichever group happens to be biggest.
func TestGetLeaderGroupNonError_FindsTheLeadersSuccessfulAnswer(t *testing.T) {
	cfg := &config{maxParticipants: 3, agreementThreshold: 2}
	leader := taggedUpstream("leader")
	other := taggedUpstream("other-1")
	third := taggedUpstream("other-2")

	t.Run("the leader's minority answer still wins the lookup", func(t *testing.T) {
		// Two upstreams agree on 0xbb and only the leader says 0xaa. The
		// leader lookup must return 0xaa; returning the majority group would
		// defeat the whole point of preferring the leader.
		analysis := analyzeWithLeader(cfg, leader, []*execResult{
			resultFrom(t, leader, "0xaa", 0),
			resultFrom(t, other, "0xbb", 1),
			resultFrom(t, third, "0xbb", 2),
		})

		group := analysis.getLeaderGroupNonError()
		require.NotNil(t, group)
		assert.Equal(t, 1, group.Count)
		assert.Equal(t, ResponseTypeNonEmpty, group.ResponseType)
		assert.Same(t, leader, group.Results[0].Upstream)
	})

	t.Run("an emptyish leader answer counts as non-error", func(t *testing.T) {
		analysis := analyzeWithLeader(cfg, leader, []*execResult{
			emptyResultFrom(t, leader, 0),
			resultFrom(t, other, "0xbb", 1),
		})

		group := analysis.getLeaderGroupNonError()
		require.NotNil(t, group, "an empty result is still a successful response")
		assert.Equal(t, ResponseTypeEmpty, group.ResponseType)
	})

	t.Run("a leader that only errored is not found", func(t *testing.T) {
		analysis := analyzeWithLeader(cfg, leader, []*execResult{
			errorFrom(leader, revertError(), 0),
			resultFrom(t, other, "0xbb", 1),
		})

		assert.Nil(t, analysis.getLeaderGroupNonError(),
			"a reverting leader has no successful answer to prefer")
	})

	t.Run("a leader that hit an infrastructure error is not found", func(t *testing.T) {
		analysis := analyzeWithLeader(cfg, leader, []*execResult{
			errorFrom(leader, infraError(), 0),
			resultFrom(t, other, "0xbb", 1),
		})

		assert.Nil(t, analysis.getLeaderGroupNonError())
	})

	t.Run("a leader that did not answer at all is not found", func(t *testing.T) {
		analysis := analyzeWithLeader(cfg, leader, []*execResult{
			resultFrom(t, other, "0xbb", 0),
			resultFrom(t, third, "0xbb", 1),
		})

		assert.Nil(t, analysis.getLeaderGroupNonError())
	})

	t.Run("no leader configured yields no group", func(t *testing.T) {
		analysis := analyzeWithLeader(cfg, nil, []*execResult{
			resultFrom(t, other, "0xbb", 0),
		})

		assert.Nil(t, analysis.getLeaderGroupNonError(),
			"without a known leader the lookup must decline rather than pick arbitrarily")
	})
}

// TestGetLeaderGroupAny_IncludesTheLeadersConsensusError separates the two
// leader lookups. getLeaderGroupAny accepts an agreed-upon error such as an EVM
// revert, because a revert IS the correct answer; it must still refuse an
// infrastructure error, which means the node failed to answer at all.
func TestGetLeaderGroupAny_IncludesTheLeadersConsensusError(t *testing.T) {
	cfg := &config{maxParticipants: 3, agreementThreshold: 2}
	leader := taggedUpstream("leader")
	other := taggedUpstream("other-1")

	t.Run("a leader revert is returned", func(t *testing.T) {
		analysis := analyzeWithLeader(cfg, leader, []*execResult{
			errorFrom(leader, revertError(), 0),
			resultFrom(t, other, "0xbb", 1),
		})

		group := analysis.getLeaderGroupAny()
		require.NotNil(t, group, "a revert is a consensus-valid answer the leader gave")
		assert.Equal(t, ResponseTypeConsensusError, group.ResponseType)

		// getLeaderGroupNonError must disagree; that difference is what makes
		// the two lookups worth having.
		assert.Nil(t, analysis.getLeaderGroupNonError())
	})

	t.Run("a leader infrastructure error is skipped", func(t *testing.T) {
		analysis := analyzeWithLeader(cfg, leader, []*execResult{
			errorFrom(leader, infraError(), 0),
			resultFrom(t, other, "0xbb", 1),
		})

		assert.Nil(t, analysis.getLeaderGroupAny(),
			"a 500 from the leader is not an answer, so it must not be preferred over a real result")
	})

	t.Run("a leader success is returned", func(t *testing.T) {
		analysis := analyzeWithLeader(cfg, leader, []*execResult{
			resultFrom(t, leader, "0xaa", 0),
			resultFrom(t, other, "0xbb", 1),
		})

		group := analysis.getLeaderGroupAny()
		require.NotNil(t, group)
		assert.Same(t, leader, group.Results[0].Upstream)
	})

	t.Run("no leader configured yields no group", func(t *testing.T) {
		analysis := analyzeWithLeader(cfg, nil, []*execResult{
			resultFrom(t, other, "0xbb", 0),
		})
		assert.Nil(t, analysis.getLeaderGroupAny())
	})
}

// TestGetLeaderFirstErrorIncludingInfra returns the leader's failure verbatim.
// An operator reading a leader-preferring dispute needs the leader's own error,
// not a generic dispute, or they cannot tell which node broke.
func TestGetLeaderFirstErrorIncludingInfra(t *testing.T) {
	cfg := &config{maxParticipants: 3, agreementThreshold: 2}
	leader := taggedUpstream("leader")
	other := taggedUpstream("other-1")

	t.Run("an infrastructure error from the leader is returned", func(t *testing.T) {
		leaderErr := infraError()
		analysis := analyzeWithLeader(cfg, leader, []*execResult{
			errorFrom(leader, leaderErr, 0),
			resultFrom(t, other, "0xbb", 1),
		})

		got := analysis.getLeaderFirstErrorIncludingInfra()
		require.Error(t, got)
		assert.Same(t, leaderErr, got,
			"the leader's own error object must come back, not a rebuilt one")
	})

	t.Run("a revert from the leader is returned", func(t *testing.T) {
		leaderErr := revertError()
		analysis := analyzeWithLeader(cfg, leader, []*execResult{
			errorFrom(leader, leaderErr, 0),
			resultFrom(t, other, "0xbb", 1),
		})

		assert.Same(t, leaderErr, analysis.getLeaderFirstErrorIncludingInfra())
	})

	t.Run("another upstream's error is never mistaken for the leader's", func(t *testing.T) {
		// The failure this catches: reporting some other node's 500 as the
		// leader's, which sends the operator to the wrong node.
		analysis := analyzeWithLeader(cfg, leader, []*execResult{
			resultFrom(t, leader, "0xaa", 0),
			errorFrom(other, infraError(), 1),
		})

		assert.NoError(t, analysis.getLeaderFirstErrorIncludingInfra(),
			"a healthy leader must report no error even when a peer failed")
	})

	t.Run("no leader configured yields no error", func(t *testing.T) {
		analysis := analyzeWithLeader(cfg, nil, []*execResult{
			errorFrom(other, infraError(), 0),
		})
		assert.NoError(t, analysis.getLeaderFirstErrorIncludingInfra())
	})
}

// TestResponseType_String pins the label values. They land in metrics and log
// lines, so a renamed or missing label silently breaks an operator's dashboard.
func TestResponseType_String(t *testing.T) {
	assert.Equal(t, "non_empty", ResponseTypeNonEmpty.String())
	assert.Equal(t, "empty", ResponseTypeEmpty.String())
	assert.Equal(t, "consensus_error", ResponseTypeConsensusError.String())
	assert.Equal(t, "infrastructure_error", ResponseTypeInfrastructureError.String())
	assert.Equal(t, "unknown", ResponseType(99).String())
}
