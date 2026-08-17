package consensus

import (
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The consensus rule chain decides which upstream answer erpc serves. A rule
// that fires out of turn returns a confidently wrong answer, so these tests
// pin two things for every rule: WHICH rule matched, and WHAT it returned.
//
// Rules are identified by description plus occurrence, not by a bare index, so
// inserting an unrelated rule does not rewrite every expectation. Two rules
// share the description "dispute when there is a tie..."; the occurrence
// argument separates them.

// ruleIndexes returns the positions of every rule carrying the given
// description, in chain order.
func ruleIndexes(desc string) []int {
	var out []int
	for i := range consensusRules {
		if consensusRules[i].Description == desc {
			out = append(out, i)
		}
	}
	return out
}

// matchedRuleIndex returns the index of the first rule whose condition holds,
// or -1 when the chain matches nothing.
func matchedRuleIndex(a *consensusAnalysis) int {
	for i := range consensusRules {
		if consensusRules[i].Condition(a) {
			return i
		}
	}
	return -1
}

// requireRule asserts that the given rule is the one the chain selects.
// occurrence is 0 for the first rule with that description, 1 for the second.
func requireRule(t *testing.T, a *consensusAnalysis, desc string, occurrence int) {
	t.Helper()
	idxs := ruleIndexes(desc)
	require.Greater(t, len(idxs), occurrence, "no rule #%d with description %q", occurrence, desc)
	got := matchedRuleIndex(a)
	require.NotEqual(t, -1, got, "no rule matched at all")
	require.Equal(t, idxs[occurrence], got,
		"wrong rule matched: want %q (#%d), got %q (index %d)",
		desc, occurrence, consensusRules[got].Description, got)
}

// analyzeForMethod builds the analysis and names the RPC method. Production
// reads the method off the request in the context; naming it directly keeps
// the test on the decision logic.
func analyzeForMethod(cfg *config, method string, responses []*execResult) *consensusAnalysis {
	a := analyze(cfg, responses)
	a.method = method
	return a
}

// resultText returns the winner's JSON-RPC result so a test can prove WHICH
// answer was served, not merely that some answer was.
func resultText(t *testing.T, sr *slotResult) string {
	t.Helper()
	require.NotNil(t, sr, "no slot result")
	require.NoError(t, sr.Error, "expected a result, got an error")
	require.NotNil(t, sr.Result, "expected a result payload")
	jrr, err := sr.Result.JsonRpcResponse()
	require.NoError(t, err)
	return jrr.GetResultString()
}

func requireDispute(t *testing.T, sr *slotResult) {
	t.Helper()
	require.NotNil(t, sr)
	require.Error(t, sr.Error)
	assert.True(t, common.HasErrorCode(sr.Error, common.ErrCodeConsensusDispute),
		"want a dispute error, got: %v", sr.Error)
}

func requireLowParticipants(t *testing.T, sr *slotResult) {
	t.Helper()
	require.NotNil(t, sr)
	require.Error(t, sr.Error)
	assert.True(t, common.HasErrorCode(sr.Error, common.ErrCodeConsensusLowParticipants),
		"want a low-participants error, got: %v", sr.Error)
}

// codedRevert builds a consensus-valid execution exception with a chosen
// JSON-RPC code. Different codes hash into different groups, which is how a
// test builds two distinct error groups.
func codedRevert(code int) error {
	return common.NewErrEndpointExecutionException(
		common.NewErrJsonRpcExceptionInternal(code, common.JsonRpcErrorNumber(code), "execution reverted", nil, nil),
	)
}

// --- prefer-highest-value-for -------------------------------------------

const preferHighestDesc = "prefer-highest-value-for: return highest value where at least agreementThreshold upstreams agree"

// TestRule_PreferHighestValue serves the largest value that enough upstreams
// agree on. Serving a higher value that only one node reports would hand the
// client a block number the rest of the network has not reached.
func TestRule_PreferHighestValue(t *testing.T) {
	base := func(threshold int) *config {
		return &config{
			maxParticipants:       3,
			agreementThreshold:    threshold,
			preferHighestValueFor: map[string][]string{"eth_blockNumber": {"result"}},
		}
	}
	u1, u2, u3 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3")

	t.Run("a lone higher value loses to the agreed lower value", func(t *testing.T) {
		cfg := base(2)
		a := analyzeForMethod(cfg, "eth_blockNumber", []*execResult{
			resultFrom(t, u1, "0x10", 0),
			resultFrom(t, u2, "0x10", 1),
			resultFrom(t, u3, "0x20", 2), // higher, but only one node says so
		})

		requireRule(t, a, preferHighestDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0x10")
	})

	t.Run("the highest value that meets the threshold wins", func(t *testing.T) {
		cfg := base(2)
		a := analyzeForMethod(cfg, "eth_blockNumber", []*execResult{
			resultFrom(t, u1, "0x20", 0),
			resultFrom(t, u2, "0x20", 1),
			resultFrom(t, u3, "0x10", 2),
		})

		requireRule(t, a, preferHighestDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0x20")
	})

	t.Run("no value meeting the threshold is a dispute", func(t *testing.T) {
		cfg := base(2)
		a := analyzeForMethod(cfg, "eth_blockNumber", []*execResult{
			resultFrom(t, u1, "0x10", 0),
			resultFrom(t, u2, "0x20", 1),
			resultFrom(t, u3, "0x30", 2),
		})

		requireRule(t, a, preferHighestDesc, 0)
		requireDispute(t, winnerOf(cfg, a))
	})

	t.Run("a zero threshold lets a single vote carry the highest value", func(t *testing.T) {
		// The rule floors the threshold at 1. That floor is unobservable —
		// a group always holds at least one response, so `count < threshold`
		// is already false for every threshold at or below 1. This case pins
		// the reachable behaviour: nothing is filtered out, so the highest
		// value wins on one vote.
		cfg := base(0)
		a := analyzeForMethod(cfg, "eth_blockNumber", []*execResult{
			resultFrom(t, u1, "0x10", 0),
			resultFrom(t, u2, "0x30", 1),
		})

		requireRule(t, a, preferHighestDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0x30")
	})

	t.Run("errors and unparsable values are skipped, not counted", func(t *testing.T) {
		// The error and the word "latest" carry no number. If either were
		// counted as a value the highest-value comparison would compare
		// against nonsense.
		cfg := base(1)
		a := analyzeForMethod(cfg, "eth_blockNumber", []*execResult{
			errorFrom(u1, codedRevert(3), 0),
			resultFrom(t, u2, "latest", 1),
			resultFrom(t, u3, "0x11", 2),
		})

		requireRule(t, a, preferHighestDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0x11")
	})

	t.Run("no numeric value anywhere falls through to plain agreement", func(t *testing.T) {
		cfg := base(2)
		a := analyzeForMethod(cfg, "eth_blockNumber", []*execResult{
			resultFrom(t, u1, "latest", 0),
			resultFrom(t, u2, "latest", 1),
		})

		assert.NotEqual(t, ruleIndexes(preferHighestDesc)[0], matchedRuleIndex(a),
			"with nothing numeric to compare, the rule must decline")
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "latest")
	})

	t.Run("a method without the setting is untouched", func(t *testing.T) {
		cfg := base(2)
		a := analyzeForMethod(cfg, "eth_getBalance", []*execResult{
			resultFrom(t, u1, "0x10", 0),
			resultFrom(t, u2, "0x10", 1),
			resultFrom(t, u3, "0x20", 2),
		})

		assert.NotEqual(t, ruleIndexes(preferHighestDesc)[0], matchedRuleIndex(a))
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0x10")
	})
}

// --- only-block-head-leader ---------------------------------------------

const onlyLeaderDisputeDesc = "only-block-head-leader on dispute: prefer leader; non-error if available else leader error"

// TestRule_OnlyBlockHeadLeaderOnDispute pins the strictest leader behaviour:
// on a dispute the leader's answer is the answer, even its failure.
func TestRule_OnlyBlockHeadLeaderOnDispute(t *testing.T) {
	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 2,
		disputeBehavior:    common.ConsensusDisputeBehaviorOnlyBlockHeadLeader,
	}
	leader, other := taggedUpstream("leader"), taggedUpstream("other")

	t.Run("the leader's minority answer wins the dispute", func(t *testing.T) {
		a := analyzeWithLeader(cfg, leader, []*execResult{
			resultFrom(t, leader, "0xaa", 0),
			resultFrom(t, other, "0xbb", 1),
		})

		requireRule(t, a, onlyLeaderDisputeDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
	})

	t.Run("a real threshold winner keeps the rule out", func(t *testing.T) {
		a := analyzeWithLeader(cfg, leader, []*execResult{
			resultFrom(t, leader, "0xaa", 0),
			resultFrom(t, other, "0xaa", 1),
		})

		assert.NotEqual(t, ruleIndexes(onlyLeaderDisputeDesc)[0], matchedRuleIndex(a),
			"there is no dispute to resolve when the threshold is met")
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
	})

	t.Run("the leader's infrastructure error is returned verbatim", func(t *testing.T) {
		// A 500 from the leader is not a consensus answer, but this behaviour
		// says the leader decides. Returning a peer's healthy answer instead
		// would silently defeat the operator's choice.
		leaderErr := infraError()
		a := analyzeWithLeader(cfg, leader, []*execResult{
			errorFrom(leader, leaderErr, 0),
			resultFrom(t, other, "0xbb", 1),
		})

		requireRule(t, a, onlyLeaderDisputeDesc, 0)
		sr := winnerOf(cfg, a)
		require.Error(t, sr.Error)
		assert.Same(t, leaderErr, sr.Error)
	})

	t.Run("no known leader falls back to a dispute", func(t *testing.T) {
		a := analyzeWithLeader(cfg, nil, []*execResult{
			resultFrom(t, taggedUpstream("u1"), "0xaa", 0),
			resultFrom(t, other, "0xbb", 1),
		})

		requireRule(t, a, onlyLeaderDisputeDesc, 0)
		requireDispute(t, winnerOf(cfg, a))
	})
}

const onlyLeaderLowDesc = "only-block-head-leader on low participants: select leader's non-error result if available"

// TestRule_OnlyBlockHeadLeaderOnLowParticipants covers the same preference
// when too few upstreams answered at all.
func TestRule_OnlyBlockHeadLeaderOnLowParticipants(t *testing.T) {
	cfg := &config{
		maxParticipants:         3,
		agreementThreshold:      2,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorOnlyBlockHeadLeader,
	}
	leader, other := taggedUpstream("leader"), taggedUpstream("other")

	t.Run("the leader's answer is served despite too few participants", func(t *testing.T) {
		a := analyzeWithLeader(cfg, leader, []*execResult{
			resultFrom(t, leader, "0xaa", 0),
			errorFrom(other, infraError(), 1),
		})

		requireRule(t, a, onlyLeaderLowDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
	})

	t.Run("enough participants keeps the rule out", func(t *testing.T) {
		a := analyzeWithLeader(cfg, leader, []*execResult{
			resultFrom(t, leader, "0xaa", 0),
			resultFrom(t, other, "0xbb", 1),
		})

		assert.NotEqual(t, ruleIndexes(onlyLeaderLowDesc)[0], matchedRuleIndex(a))
	})

	t.Run("a leader that only reverted returns that revert", func(t *testing.T) {
		leaderErr := codedRevert(3)
		a := analyzeWithLeader(cfg, leader, []*execResult{
			errorFrom(leader, leaderErr, 0),
			errorFrom(other, infraError(), 1),
		})

		requireRule(t, a, onlyLeaderLowDesc, 0)
		sr := winnerOf(cfg, a)
		require.Error(t, sr.Error)
		assert.Same(t, leaderErr, sr.Error)
	})

	t.Run("no known leader reports low participants", func(t *testing.T) {
		a := analyzeWithLeader(cfg, nil, []*execResult{
			errorFrom(other, infraError(), 0),
			errorFrom(taggedUpstream("u2"), infraError(), 1),
		})

		requireRule(t, a, onlyLeaderLowDesc, 0)
		requireLowParticipants(t, winnerOf(cfg, a))
	})
}

const preferLeaderDesc = "prefer-block-head-leader: prefer leader's non-error group when no threshold winner"

// TestRule_PreferBlockHeadLeader is the softer leader behaviour: prefer the
// leader when it answered, otherwise let the later rules decide.
func TestRule_PreferBlockHeadLeader(t *testing.T) {
	leader, u2, u3 := taggedUpstream("leader"), taggedUpstream("u2"), taggedUpstream("u3")

	t.Run("on dispute the leader's group wins", func(t *testing.T) {
		cfg := &config{
			maxParticipants:    3,
			agreementThreshold: 2,
			disputeBehavior:    common.ConsensusDisputeBehaviorPreferBlockHeadLeader,
		}
		a := analyzeWithLeader(cfg, leader, []*execResult{
			resultFrom(t, leader, "0xaa", 0),
			resultFrom(t, u2, "0xbb", 1),
			resultFrom(t, u3, "0xcc", 2),
		})

		requireRule(t, a, preferLeaderDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
	})

	t.Run("on low participants the leader's group wins", func(t *testing.T) {
		cfg := &config{
			maxParticipants:         3,
			agreementThreshold:      3,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorPreferBlockHeadLeader,
		}
		a := analyzeWithLeader(cfg, leader, []*execResult{
			resultFrom(t, leader, "0xaa", 0),
			errorFrom(u2, infraError(), 1),
		})

		requireRule(t, a, preferLeaderDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
	})

	t.Run("a threshold winner keeps the rule out", func(t *testing.T) {
		cfg := &config{
			maxParticipants:    3,
			agreementThreshold: 2,
			disputeBehavior:    common.ConsensusDisputeBehaviorPreferBlockHeadLeader,
		}
		a := analyzeWithLeader(cfg, leader, []*execResult{
			resultFrom(t, leader, "0xaa", 0),
			resultFrom(t, u2, "0xbb", 1),
			resultFrom(t, u3, "0xbb", 2),
		})

		assert.NotEqual(t, ruleIndexes(preferLeaderDesc)[0], matchedRuleIndex(a))
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xbb",
			"the agreed answer wins; preferring the leader is only a tiebreak")
	})

	t.Run("a silent leader lets the later rules decide", func(t *testing.T) {
		cfg := &config{
			maxParticipants:    3,
			agreementThreshold: 2,
			disputeBehavior:    common.ConsensusDisputeBehaviorPreferBlockHeadLeader,
		}
		a := analyzeWithLeader(cfg, nil, []*execResult{
			resultFrom(t, u2, "0xbb", 0),
			resultFrom(t, u3, "0xcc", 1),
		})

		assert.NotEqual(t, ruleIndexes(preferLeaderDesc)[0], matchedRuleIndex(a))
		requireDispute(t, winnerOf(cfg, a))
	})
}

// --- prefer-larger ------------------------------------------------------

const largerBelowDesc = "prefer-larger + accept-most-common: choose largest non-empty below threshold"

// TestRule_PreferLargerBelowThreshold covers the "a truncated answer is a
// wrong answer" preference: below threshold, take the biggest payload.
func TestRule_PreferLargerBelowThreshold(t *testing.T) {
	cfg := &config{
		maxParticipants:       3,
		agreementThreshold:    3,
		preferLargerResponses: true,
		disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
	}
	u1, u2 := taggedUpstream("u1"), taggedUpstream("u2")

	t.Run("the largest payload wins when nothing meets the threshold", func(t *testing.T) {
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xbbbbbbbbbbbbbbbbbbbb", 1),
		})

		requireRule(t, a, largerBelowDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xbbbbbbbbbbbbbbbbbbbb")
	})

	t.Run("with nothing non-empty to size up it disputes", func(t *testing.T) {
		// An empty answer and a revert have no payload to compare. Picking
		// either one as "largest" would invent a preference the operator
		// never expressed.
		a := analyze(cfg, []*execResult{
			emptyResultFrom(t, u1, 0),
			errorFrom(u2, codedRevert(3), 1),
		})

		requireRule(t, a, largerBelowDesc, 0)
		requireDispute(t, winnerOf(cfg, a))
	})
}

const largerAboveDesc = "prefer-larger: when above threshold and multiple valid groups, choose largest non-empty"

func TestRule_PreferLargerAboveThreshold(t *testing.T) {
	cfg := &config{
		maxParticipants:       4,
		agreementThreshold:    2,
		preferLargerResponses: true,
		disputeBehavior:       common.ConsensusDisputeBehaviorReturnError,
	}
	u1, u2, u3, u4 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3"), taggedUpstream("u4")

	t.Run("two agreed groups resolve to the larger one", func(t *testing.T) {
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xaa", 1),
			resultFrom(t, u3, "0xbbbbbbbbbbbbbbbbbbbb", 2),
			resultFrom(t, u4, "0xbbbbbbbbbbbbbbbbbbbb", 3),
		})

		requireRule(t, a, largerAboveDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xbbbbbbbbbbbbbbbbbbbb")
	})

	t.Run("two agreed groups with no payload dispute", func(t *testing.T) {
		a := analyze(cfg, []*execResult{
			emptyResultFrom(t, u1, 0),
			emptyResultFrom(t, u2, 1),
			errorFrom(u3, codedRevert(3), 2),
			errorFrom(u4, codedRevert(3), 3),
		})

		requireRule(t, a, largerAboveDesc, 0)
		requireDispute(t, winnerOf(cfg, a))
	})
}

const largerOverThresholdDesc = "prefer-larger + accept-most-common: choose largest when smaller meets threshold and larger exists"

// TestRule_PreferLargerBeatsAgreedSmaller is the sharpest case for this
// preference: the majority agrees on a short answer, one node returns a longer
// one, and the operator asked for the longer one.
func TestRule_PreferLargerBeatsAgreedSmaller(t *testing.T) {
	cfg := &config{
		maxParticipants:       3,
		agreementThreshold:    2,
		preferLargerResponses: true,
		disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
	}
	u1, u2, u3 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3")

	a := analyze(cfg, []*execResult{
		resultFrom(t, u1, "0xaa", 0),
		resultFrom(t, u2, "0xaa", 1),
		resultFrom(t, u3, "0xbbbbbbbbbbbbbbbbbbbb", 2),
	})

	requireRule(t, a, largerOverThresholdDesc, 0)
	assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xbbbbbbbbbbbbbbbbbbbb")
}

const returnErrorLargerDesc = "return-error: smaller winner at threshold but larger non-empty exists with preference -> dispute"

// TestRule_ReturnErrorWhenLargerIsUnagreed is the same shape as above, but the
// operator chose ReturnError. Serving either answer would hide the conflict.
func TestRule_ReturnErrorWhenLargerIsUnagreed(t *testing.T) {
	cfg := &config{
		maxParticipants:       3,
		agreementThreshold:    2,
		preferLargerResponses: true,
		disputeBehavior:       common.ConsensusDisputeBehaviorReturnError,
	}
	u1, u2, u3 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3")

	a := analyze(cfg, []*execResult{
		resultFrom(t, u1, "0xaa", 0),
		resultFrom(t, u2, "0xaa", 1),
		resultFrom(t, u3, "0xbbbbbbbbbbbbbbbbbbbb", 2),
	})

	requireRule(t, a, returnErrorLargerDesc, 0)
	requireDispute(t, winnerOf(cfg, a))
}

// --- prefer-non-empty ---------------------------------------------------

const nonEmptyOverAgreedDesc = "accept-most-common + prefer-non-empty: choose non-empty even if empty or error meets threshold"

// TestRule_PreferNonEmptyOverAgreedNothing covers the most common real fault:
// two lagging nodes agree the data does not exist, one caught-up node has it.
func TestRule_PreferNonEmptyOverAgreedNothing(t *testing.T) {
	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 2,
		preferNonEmpty:     true,
		disputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
	}
	u1, u2, u3 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3")

	t.Run("an agreed empty answer loses to the one real answer", func(t *testing.T) {
		a := analyze(cfg, []*execResult{
			emptyResultFrom(t, u1, 0),
			emptyResultFrom(t, u2, 1),
			resultFrom(t, u3, "0xaa", 2),
		})

		requireRule(t, a, nonEmptyOverAgreedDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
	})

	t.Run("an agreed revert loses to the one real answer", func(t *testing.T) {
		a := analyze(cfg, []*execResult{
			errorFrom(u1, codedRevert(3), 0),
			errorFrom(u2, codedRevert(3), 1),
			resultFrom(t, u3, "0xaa", 2),
		})

		requireRule(t, a, nonEmptyOverAgreedDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
	})
}

const nonEmptyOverEmptyBelowDesc = "accept-most-common below threshold prefers non-empty over empty"

func TestRule_PreferNonEmptyOverEmptyBelowThreshold(t *testing.T) {
	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 3,
		preferNonEmpty:     true,
		disputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
	}
	u1, u2, u3 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3")

	t.Run("one real answer beats the empty majority", func(t *testing.T) {
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			emptyResultFrom(t, u2, 1),
			emptyResultFrom(t, u3, 2),
		})

		requireRule(t, a, nonEmptyOverEmptyBelowDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
	})

	t.Run("two disagreeing real answers make the rule stand down", func(t *testing.T) {
		// The rule only resolves an empty-versus-present conflict. Two
		// different present answers are a genuine disagreement, and picking
		// one of them here would skip the dispute handling below.
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xbb", 1),
			emptyResultFrom(t, u3, 2),
		})

		assert.NotEqual(t, ruleIndexes(nonEmptyOverEmptyBelowDesc)[0], matchedRuleIndex(a))
	})
}

const nonEmptyOverErrorBelowDesc = "accept-most-common below threshold prefers non-empty over consensus error"

func TestRule_PreferNonEmptyOverErrorBelowThreshold(t *testing.T) {
	cfg := &config{
		maxParticipants:    3,
		agreementThreshold: 3,
		preferNonEmpty:     true,
		disputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
	}
	u1, u2, u3 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3")

	a := analyze(cfg, []*execResult{
		errorFrom(u1, codedRevert(3), 0),
		errorFrom(u2, codedRevert(3), 1),
		resultFrom(t, u3, "0xaa", 2),
	})

	requireRule(t, a, nonEmptyOverErrorBelowDesc, 0)
	assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
}

const nonEmptyOverErrorAboveDesc = "accept-most-common + prefer-non-empty: above threshold choose non-empty over consensus error"

func TestRule_PreferNonEmptyOverAgreedErrorAboveThreshold(t *testing.T) {
	cfg := &config{
		maxParticipants:    5,
		agreementThreshold: 2,
		preferNonEmpty:     true,
		disputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
	}
	ups := []common.Upstream{
		taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3"),
		taggedUpstream("u4"), taggedUpstream("u5"),
	}

	// Three nodes have the data and two revert. Both groups clear the
	// threshold, so the tie must break toward the real answer.
	a := analyze(cfg, []*execResult{
		resultFrom(t, ups[0], "0xaa", 0),
		resultFrom(t, ups[1], "0xaa", 1),
		resultFrom(t, ups[2], "0xaa", 2),
		errorFrom(ups[3], codedRevert(3), 3),
		errorFrom(ups[4], codedRevert(3), 4),
	})

	requireRule(t, a, nonEmptyOverErrorAboveDesc, 0)
	assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
}

const returnErrorEmptyDesc = "return error when empty would win but non-empty exists (above threshold)"

// TestRule_ReturnErrorWhenEmptyWouldWin pins the difference the preference
// makes under ReturnError: with it, the conflict surfaces; without it, the
// agreed empty answer is served.
func TestRule_ReturnErrorWhenEmptyWouldWin(t *testing.T) {
	u1, u2, u3 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3")
	responses := func(t *testing.T) []*execResult {
		return []*execResult{
			emptyResultFrom(t, u1, 0),
			emptyResultFrom(t, u2, 1),
			resultFrom(t, u3, "0xaa", 2),
		}
	}

	t.Run("with prefer-non-empty the conflict is reported", func(t *testing.T) {
		cfg := &config{
			maxParticipants:    3,
			agreementThreshold: 2,
			preferNonEmpty:     true,
			disputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
		}
		a := analyze(cfg, responses(t))

		requireRule(t, a, returnErrorEmptyDesc, 0)
		requireDispute(t, winnerOf(cfg, a))
	})

	t.Run("without the preference the agreed empty answer is served", func(t *testing.T) {
		cfg := &config{
			maxParticipants:    3,
			agreementThreshold: 2,
			disputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
		}
		a := analyze(cfg, responses(t))

		assert.NotEqual(t, ruleIndexes(returnErrorEmptyDesc)[0], matchedRuleIndex(a))
		sr := winnerOf(cfg, a)
		require.NoError(t, sr.Error)
		require.NotNil(t, sr.Result)
	})
}

// --- ties ---------------------------------------------------------------

const tieDesc = "dispute when there is a tie at or above threshold without preference"

// TestRule_TieWithoutPreference covers both tie rules. A tie with no stated
// preference has no right answer, so erpc must say so rather than pick.
func TestRule_TieWithoutPreference(t *testing.T) {
	ups := []common.Upstream{
		taggedUpstream("u1"), taggedUpstream("u2"),
		taggedUpstream("u3"), taggedUpstream("u4"),
	}

	t.Run("two equally agreed results dispute", func(t *testing.T) {
		cfg := &config{maxParticipants: 4, agreementThreshold: 2}
		a := analyze(cfg, []*execResult{
			resultFrom(t, ups[0], "0xaa", 0),
			resultFrom(t, ups[1], "0xaa", 1),
			resultFrom(t, ups[2], "0xbb", 2),
			resultFrom(t, ups[3], "0xbb", 3),
		})

		requireRule(t, a, tieDesc, 0)
		requireDispute(t, winnerOf(cfg, a))
	})

	t.Run("a result tied with an agreed error reaches the later tie rule", func(t *testing.T) {
		// The first tie rule ignores error groups, so this tie falls to the
		// second one. Both must dispute: an agreed revert and an agreed
		// result carry equal weight and cannot both be right.
		cfg := &config{maxParticipants: 4, agreementThreshold: 2}
		a := analyze(cfg, []*execResult{
			resultFrom(t, ups[0], "0xaa", 0),
			resultFrom(t, ups[1], "0xaa", 1),
			errorFrom(ups[2], codedRevert(3), 2),
			errorFrom(ups[3], codedRevert(3), 3),
		})

		requireRule(t, a, tieDesc, 1)
		requireDispute(t, winnerOf(cfg, a))
	})

	t.Run("an agreed error with no tie is returned, not disputed", func(t *testing.T) {
		cfg := &config{maxParticipants: 5, agreementThreshold: 2}
		revert := codedRevert(3)
		a := analyze(cfg, []*execResult{
			errorFrom(ups[0], revert, 0),
			errorFrom(ups[1], codedRevert(3), 1),
			errorFrom(ups[2], codedRevert(3), 2),
			resultFrom(t, ups[3], "0xaa", 3),
			resultFrom(t, taggedUpstream("u5"), "0xaa", 4),
		})

		assert.NotEqual(t, ruleIndexes(tieDesc)[0], matchedRuleIndex(a))
		sr := winnerOf(cfg, a)
		require.Error(t, sr.Error)
		assert.Same(t, revert, sr.Error, "the agreed revert IS the answer")
	})
}

// --- accept-most-common -------------------------------------------------

const uniqueLeaderDesc = "accept-most-common valid group below threshold with a unique leader"

// TestRule_AcceptMostCommonUniqueLeader serves the plurality answer when no
// group reaches the threshold but one is clearly ahead.
func TestRule_AcceptMostCommonUniqueLeader(t *testing.T) {
	cfg := &config{
		maxParticipants:         3,
		agreementThreshold:      3,
		disputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
	}
	u1, u2, u3 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3")

	t.Run("the plurality result is served", func(t *testing.T) {
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xaa", 1),
			resultFrom(t, u3, "0xbb", 2),
		})

		requireRule(t, a, uniqueLeaderDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
	})

	t.Run("a plurality revert is returned as that revert", func(t *testing.T) {
		revert := codedRevert(3)
		a := analyze(cfg, []*execResult{
			errorFrom(u1, revert, 0),
			errorFrom(u2, codedRevert(3), 1),
			resultFrom(t, u3, "0xaa", 2),
		})

		requireRule(t, a, uniqueLeaderDesc, 0)
		sr := winnerOf(cfg, a)
		require.Error(t, sr.Error)
		assert.Same(t, revert, sr.Error)
	})

	t.Run("a third group does not disturb the leader", func(t *testing.T) {
		cfg4 := &config{
			maxParticipants:         6,
			agreementThreshold:      4,
			disputeBehavior:         common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorReturnError,
		}
		a := analyze(cfg4, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xaa", 1),
			resultFrom(t, u3, "0xaa", 2),
			resultFrom(t, taggedUpstream("u4"), "0xbb", 3),
			resultFrom(t, taggedUpstream("u5"), "0xbb", 4),
			resultFrom(t, taggedUpstream("u6"), "0xcc", 5),
		})

		requireRule(t, a, uniqueLeaderDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg4, a)), "0xaa")
	})
}

const lowAcceptMostCommonDesc = "low participants + accept-most-common: return best valid by priority and consider non-empty ties as dispute"

// TestRule_LowParticipantsAcceptMostCommon covers the last-resort ranking:
// a real answer, then an empty one, then an agreed error.
func TestRule_LowParticipantsAcceptMostCommon(t *testing.T) {
	newCfg := func(threshold, maxP int) *config {
		return &config{
			maxParticipants:         maxP,
			agreementThreshold:      threshold,
			lowParticipantsBehavior: common.ConsensusLowParticipantsBehaviorAcceptMostCommonValidResult,
		}
	}
	u := func(n string) common.Upstream { return taggedUpstream(n) }

	t.Run("two single disagreeing answers dispute", func(t *testing.T) {
		cfg := newCfg(3, 3)
		a := analyze(cfg, []*execResult{
			resultFrom(t, u("u1"), "0xaa", 0),
			resultFrom(t, u("u2"), "0xbb", 1),
			errorFrom(u("u3"), infraError(), 2),
		})

		requireRule(t, a, lowAcceptMostCommonDesc, 0)
		requireDispute(t, winnerOf(cfg, a))
	})

	t.Run("a clear non-empty group outranks an equally sized error group", func(t *testing.T) {
		cfg := newCfg(5, 5)
		a := analyze(cfg, []*execResult{
			resultFrom(t, u("u1"), "0xaa", 0),
			resultFrom(t, u("u2"), "0xaa", 1),
			errorFrom(u("u3"), codedRevert(3), 2),
			errorFrom(u("u4"), codedRevert(3), 3),
		})

		requireRule(t, a, lowAcceptMostCommonDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xaa")
	})

	t.Run("with nothing non-empty the empty answer is served", func(t *testing.T) {
		cfg := newCfg(5, 5)
		a := analyze(cfg, []*execResult{
			emptyResultFrom(t, u("u1"), 0),
			emptyResultFrom(t, u("u2"), 1),
			errorFrom(u("u3"), codedRevert(3), 2),
			errorFrom(u("u4"), codedRevert(3), 3),
		})

		requireRule(t, a, lowAcceptMostCommonDesc, 0)
		sr := winnerOf(cfg, a)
		require.NoError(t, sr.Error)
		require.NotNil(t, sr.Result, "an empty answer still beats an error")
	})

	t.Run("with only errors the agreed error is returned", func(t *testing.T) {
		cfg := newCfg(5, 5)
		revertA := codedRevert(3)
		a := analyze(cfg, []*execResult{
			errorFrom(u("u1"), revertA, 0),
			errorFrom(u("u2"), codedRevert(3), 1),
			errorFrom(u("u3"), codedRevert(-32000), 2),
			errorFrom(u("u4"), codedRevert(-32000), 3),
		})

		requireRule(t, a, lowAcceptMostCommonDesc, 0)
		sr := winnerOf(cfg, a)
		require.Error(t, sr.Error)
	})
}

// --- preference rules standing down -------------------------------------

const thresholdWinnerDesc = "consensus on result or error achieved if there is a valid group that meets the agreement threshold"
const noResponsesDesc = "consider low participants when no responses are available"

// TestRules_PreferencesStandDown pins what happens when a configured
// preference has nothing to act on. Each rule must decline and let plain
// agreement decide. A preference that fires anyway would rewrite a correct
// answer for no reason.
func TestRules_PreferencesStandDown(t *testing.T) {
	u1, u2, u3 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3")

	t.Run("prefer-larger leaves an already-largest agreed answer alone", func(t *testing.T) {
		cfg := &config{
			maxParticipants:       3,
			agreementThreshold:    2,
			preferLargerResponses: true,
			disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		}
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xbbbbbbbbbbbbbbbbbbbb", 0),
			resultFrom(t, u2, "0xbbbbbbbbbbbbbbbbbbbb", 1),
			resultFrom(t, u3, "0xaa", 2),
		})

		requireRule(t, a, thresholdWinnerDesc, 0)
		assert.Contains(t, resultText(t, winnerOf(cfg, a)), "0xbbbbbbbbbbbbbbbbbbbb")
	})

	t.Run("prefer-larger under return-error still disputes a real disagreement", func(t *testing.T) {
		cfg := &config{
			maxParticipants:       3,
			agreementThreshold:    3,
			preferLargerResponses: true,
			disputeBehavior:       common.ConsensusDisputeBehaviorReturnError,
		}
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xbbbbbbbbbbbbbbbbbbbb", 1),
		})

		requireDispute(t, winnerOf(cfg, a))
	})

	t.Run("prefer-non-empty with nothing non-empty serves the plurality revert", func(t *testing.T) {
		cfg := &config{
			maxParticipants:    3,
			agreementThreshold: 3,
			preferNonEmpty:     true,
			disputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		}
		revert := codedRevert(3)
		a := analyze(cfg, []*execResult{
			errorFrom(u1, revert, 0),
			errorFrom(u2, codedRevert(3), 1),
			emptyResultFrom(t, u3, 2),
		})

		requireRule(t, a, uniqueLeaderDesc, 0)
		sr := winnerOf(cfg, a)
		require.Error(t, sr.Error)
		assert.Same(t, revert, sr.Error)
	})

	t.Run("return-error with a unanimous empty answer serves it", func(t *testing.T) {
		// prefer-non-empty only reports a conflict when a real answer exists
		// somewhere. With every node agreeing on empty there is no conflict.
		cfg := &config{
			maxParticipants:    2,
			agreementThreshold: 2,
			preferNonEmpty:     true,
			disputeBehavior:    common.ConsensusDisputeBehaviorReturnError,
		}
		a := analyze(cfg, []*execResult{
			emptyResultFrom(t, u1, 0),
			emptyResultFrom(t, u2, 1),
		})

		requireRule(t, a, thresholdWinnerDesc, 0)
		sr := winnerOf(cfg, a)
		require.NoError(t, sr.Error)
		require.NotNil(t, sr.Result)
	})

	t.Run("no responses at all reports low participants", func(t *testing.T) {
		cfg := &config{
			maxParticipants:    3,
			agreementThreshold: 2,
			preferNonEmpty:     true,
			disputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		}
		a := analyze(cfg, nil)

		requireRule(t, a, noResponsesDesc, 0)
		requireLowParticipants(t, winnerOf(cfg, a))
	})
}

// --- short-circuit rules ------------------------------------------------

func shortCircuitOf(cfg *config, a *consensusAnalysis, winner *slotResult) (string, bool) {
	e := &executor{consensusPolicy: &consensusPolicy{config: cfg}}
	return e.shouldShortCircuit(winner, a)
}

// TestShortCircuit_ConsensusErrorThreshold pins when erpc stops waiting
// because enough upstreams agreed on an error. Stopping too early throws away
// a preference the operator configured; stopping too late costs latency.
func TestShortCircuit_ConsensusErrorThreshold(t *testing.T) {
	u1, u2, u3 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3")
	agreedReverts := func(t *testing.T) []*execResult {
		return []*execResult{
			errorFrom(u1, codedRevert(3), 0),
			errorFrom(u2, codedRevert(3), 1),
		}
	}

	t.Run("an agreed error at threshold stops collection", func(t *testing.T) {
		cfg := &config{maxParticipants: 3, agreementThreshold: 2}
		a := analyze(cfg, agreedReverts(t))

		reason, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.True(t, ok)
		assert.Equal(t, "consensus_error_threshold", reason)
	})

	t.Run("an error below threshold keeps collecting", func(t *testing.T) {
		cfg := &config{maxParticipants: 3, agreementThreshold: 3}
		a := analyze(cfg, agreedReverts(t))

		_, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.False(t, ok)
	})

	t.Run("no responses at all cannot short-circuit", func(t *testing.T) {
		cfg := &config{maxParticipants: 3, agreementThreshold: 2}
		a := analyze(cfg, nil)

		_, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.False(t, ok)
	})

	t.Run("prefer-highest-value-for waits for every response", func(t *testing.T) {
		// The highest value may still be in flight, so an agreed error must
		// not end the round.
		cfg := &config{
			maxParticipants:       3,
			agreementThreshold:    2,
			preferHighestValueFor: map[string][]string{"eth_blockNumber": {"result"}},
		}
		a := analyzeForMethod(cfg, "eth_blockNumber", agreedReverts(t))

		_, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.False(t, ok)
	})

	t.Run("a live prefer-non-empty setting waits for a real answer", func(t *testing.T) {
		cfg := &config{
			maxParticipants:    3,
			agreementThreshold: 2,
			preferNonEmpty:     true,
			disputeBehavior:    common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		}
		a := analyze(cfg, agreedReverts(t))

		_, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.False(t, ok, "a non-empty answer could still arrive and win")
	})

	t.Run("a live prefer-larger setting waits for a bigger answer", func(t *testing.T) {
		cfg := &config{
			maxParticipants:       3,
			agreementThreshold:    2,
			preferLargerResponses: true,
			disputeBehavior:       common.ConsensusDisputeBehaviorAcceptMostCommonValidResult,
		}
		a := analyze(cfg, agreedReverts(t))

		_, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.False(t, ok)
	})

	t.Run("a result group leading keeps this rule out", func(t *testing.T) {
		cfg := &config{maxParticipants: 4, agreementThreshold: 2}
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xaa", 1),
			errorFrom(u3, codedRevert(3), 2),
		})

		reason, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		if ok {
			assert.NotEqual(t, "consensus_error_threshold", reason)
		}
	})
}

// TestShortCircuit_UnassailableLead pins the latency optimisation: stop as
// soon as the remaining upstreams cannot change the winner.
func TestShortCircuit_UnassailableLead(t *testing.T) {
	u1, u2, u3 := taggedUpstream("u1"), taggedUpstream("u2"), taggedUpstream("u3")

	t.Run("an unbeatable lead stops collection", func(t *testing.T) {
		cfg := &config{maxParticipants: 3, agreementThreshold: 2}
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xaa", 1),
		})

		reason, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.True(t, ok)
		assert.Equal(t, "unassailable_lead", reason)
	})

	t.Run("a beatable lead keeps collecting", func(t *testing.T) {
		// Two responses agree and three more can still arrive, so a rival
		// group can still overtake the leader.
		cfg := &config{maxParticipants: 5, agreementThreshold: 2}
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xaa", 1),
		})

		_, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.False(t, ok)
	})

	t.Run("a rival group narrows the lead and blocks the stop", func(t *testing.T) {
		// Lead of 1 over the rival, but 2 responses still to come.
		cfg := &config{maxParticipants: 5, agreementThreshold: 2}
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xaa", 1),
			resultFrom(t, u3, "0xbb", 2),
		})

		_, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.False(t, ok, "the rival can still catch up")
	})

	t.Run("a tie is still allowed to stop once it cannot be overtaken", func(t *testing.T) {
		// Lead of 1 with exactly 1 response left: the rival can tie but not
		// win, and the rules deliberately let a possible tie short-circuit.
		cfg := &config{maxParticipants: 4, agreementThreshold: 2}
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xaa", 1),
			resultFrom(t, u3, "0xbb", 2),
		})

		reason, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.True(t, ok)
		assert.Equal(t, "unassailable_lead", reason)
	})

	t.Run("prefer-highest-value-for blocks the stop while responses remain", func(t *testing.T) {
		cfg := &config{
			maxParticipants:       4,
			agreementThreshold:    2,
			preferHighestValueFor: map[string][]string{"eth_blockNumber": {"result"}},
		}
		a := analyzeForMethod(cfg, "eth_blockNumber", []*execResult{
			resultFrom(t, u1, "0x10", 0),
			resultFrom(t, u2, "0x10", 1),
		})

		_, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.False(t, ok, "a higher value may still arrive")
	})

	t.Run("prefer-larger blocks the stop while responses remain", func(t *testing.T) {
		cfg := &config{maxParticipants: 4, agreementThreshold: 2, preferLargerResponses: true}
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xaa", 1),
		})

		_, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.False(t, ok, "a larger answer may still arrive")
	})

	t.Run("an empty leader never stops collection", func(t *testing.T) {
		// This rule only stops on a non-empty leader, so an agreed empty
		// answer keeps the round open whatever the preferences say. The
		// dedicated prefer-non-empty guard above that check is therefore
		// unobservable; see rules.go:932.
		cfg := &config{maxParticipants: 4, agreementThreshold: 2, preferNonEmpty: true}
		a := analyze(cfg, []*execResult{
			emptyResultFrom(t, u1, 0),
			emptyResultFrom(t, u2, 1),
		})

		_, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.False(t, ok, "a real answer may still arrive")
	})

	t.Run("prefer-larger no longer blocks once every slot answered", func(t *testing.T) {
		cfg := &config{maxParticipants: 2, agreementThreshold: 2, preferLargerResponses: true}
		a := analyze(cfg, []*execResult{
			resultFrom(t, u1, "0xaa", 0),
			resultFrom(t, u2, "0xaa", 1),
		})

		reason, ok := shortCircuitOf(cfg, a, winnerOf(cfg, a))
		assert.True(t, ok)
		assert.Equal(t, "unassailable_lead", reason)
	})
}
