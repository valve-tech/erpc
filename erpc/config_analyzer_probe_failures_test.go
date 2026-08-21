package erpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// `erpc validate` answers one question: will this config serve? The tests in
// config_analyzer_test.go cover the checks that pass or find a disagreement.
// These cover what the report says when a probe cannot complete at all — the
// node is down, it answers nonsense, or it hides the genesis block. Each of
// those is a warning the operator must be able to trace back to one upstream by
// name, because the alternative is reading every endpoint in the file.

// deadNode returns an endpoint that refuses every request. It is a closed
// httptest server, which is the closest a test gets to an unreachable node.
func deadNode(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}

// TestValidate_NamesTheUpstreamWhoseChainIdProbeFailed covers the most common
// validation failure: the endpoint is wrong, dead, or behind a firewall. The
// operator has to learn which upstream, or the message sends them reading the
// whole file.
func TestValidate_NamesTheUpstreamWhoseChainIdProbeFailed(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	cfg := validationConfig(evmUpstream("unreachable", deadNode(t), 123))
	report := GenerateValidationReport(context.Background(), cfg)

	require.Contains(t, joined(report.Warnings),
		"upstream 'unreachable' could not fetch chain ID via eth_chainId",
		"the operator must be told which upstream failed the probe")
	require.Empty(t, report.Errors,
		"an unreachable node is not proof of a wrong chain, so it stays a warning")
}

// TestValidate_ReportsAnEmptyChainIdAnswer covers a node that answers the call
// and returns nothing. eth_chainId normalisation turns the empty answer into 0,
// so the report reads as chain ID 0 rather than "no answer". Either way the
// operator must get an error: treating it as a match would ship a node that
// never proved which chain it serves.
func TestValidate_ReportsAnEmptyChainIdAnswer(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	node.chainIdHex = ""

	cfg := validationConfig(evmUpstream("empty-answer", node.URL, 123))
	report := GenerateValidationReport(context.Background(), cfg)

	require.Contains(t, joined(report.Warnings),
		"upstream 'empty-answer' returned chain ID 0 from eth_chainId, which is invalid",
		"the report must name the upstream that answered nothing")
	require.Contains(t, joined(report.Errors),
		"upstream 'empty-answer' reported chain ID 0 but the configuration expects 123",
		"an unproven chain must not pass validation")
}

// TestValidate_ReportsChainIdZero covers a node that answers 0. Zero is not a
// chain, so accepting it would mean validating a config against nothing.
func TestValidate_ReportsChainIdZero(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	node.chainIdHex = "0x0"

	cfg := validationConfig(evmUpstream("zero-chain", node.URL, 123))
	report := GenerateValidationReport(context.Background(), cfg)

	require.Contains(t, joined(report.Warnings),
		"upstream 'zero-chain' returned chain ID 0 from eth_chainId, which is invalid")
	require.Contains(t, joined(report.Errors),
		"upstream 'zero-chain' reported chain ID 0 but the configuration expects 123",
		"chain ID 0 still disagrees with the configured chain, and that is an error")
}

// TestValidate_ReportsAChainIdTooLargeToBeOne covers a node answering a value
// past the signed 64-bit range. The validator cannot compare it, so it must say
// so and name the value rather than silently dropping the upstream.
func TestValidate_ReportsAChainIdTooLargeToBeOne(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	node.chainIdHex = "0xffffffffffffffff" // 2^64-1, past what int64 holds

	cfg := validationConfig(evmUpstream("huge-chain", node.URL, 123))
	report := GenerateValidationReport(context.Background(), cfg)

	require.Contains(t, joined(report.Warnings),
		"upstream 'huge-chain' returned an invalid chain ID '18446744073709551615'",
		"the report must quote the value the node returned")
}

// TestValidate_WarnsWhenNoUpstreamReturnsTheGenesisBlock covers a fleet where
// the genesis comparison cannot run. Staying silent would let the operator read
// a clean report and believe the fork check passed.
func TestValidate_WarnsWhenNoUpstreamReturnsTheGenesisBlock(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	node.genesisHash = "" // answers the call, returns no hash

	cfg := validationConfig(evmUpstream("no-genesis", node.URL, 123))
	report := GenerateValidationReport(context.Background(), cfg)

	require.Contains(t, joined(report.Warnings), "no upstream returned genesis block",
		"a genesis check that never ran must not read as a genesis check that passed")
	require.Contains(t, joined(report.Warnings), "chain=123",
		"the warning must name the chain it could not check")
}

// TestValidate_NamesTheOneUpstreamThatHidesGenesis covers the mixed case: one
// node answers genesis and one does not. The report must still single out the
// one that failed, so the operator fixes that node instead of the fleet.
func TestValidate_NamesTheOneUpstreamThatHidesGenesis(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	good := evmNode(t)
	quiet := evmNode(t)
	quiet.genesisHash = ""

	cfg := validationConfig(
		evmUpstream("answers-genesis", good.URL, 123),
		evmUpstream("hides-genesis", quiet.URL, 123),
	)
	report := GenerateValidationReport(context.Background(), cfg)

	require.Contains(t, joined(report.Warnings),
		"upstream=hides-genesis chain=123 could not fetch genesis block: returned no block hash",
		"a node that answers with no hash never joins the fork comparison, so the report must say so")
	require.NotContains(t, joined(report.Warnings), "upstream=answers-genesis chain=123 could not fetch genesis",
		"the upstream that answered must not be blamed")
}

// TestValidate_WarnsWhenTheHistoricalBlockIsUnreachable covers the deep-block
// comparison failing across the whole group. The operator must learn that the
// fork check did not run, and at which block.
func TestValidate_WarnsWhenTheHistoricalBlockIsUnreachable(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	// Both nodes answer chain id, genesis, latest and finalized, but return no
	// hash for any numeric block tag — which is the block the validator picks
	// for its historical comparison.
	a, b := evmNode(t), evmNode(t)
	a.deepHash, b.deepHash = "", ""

	cfg := validationConfig(
		evmUpstream("node-a", a.URL, 123),
		evmUpstream("node-b", b.URL, 123),
	)
	report := GenerateValidationReport(context.Background(), cfg)

	require.Contains(t, joined(report.Warnings), "could not fetch block 936 from any upstream",
		"the warning must name the block the comparison needed")
	require.Contains(t, joined(report.Warnings), "chain=123 project=main")
}

// TestValidate_DoesNotCallAProjectLevelBudgetAnOrphan pins the rate-limit
// accounting an operator relies on. A budget attached to the project protects
// every request through it; reporting it orphaned would invite the operator to
// delete a live limit.
func TestValidate_DoesNotCallAProjectLevelBudgetAnOrphan(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	cfg := validationConfig(evmUpstream("node-a", node.URL, 123))
	cfg.RateLimiters = &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{
			{Id: "project-wide", Rules: []*common.RateLimitRuleConfig{{Method: "*"}}},
			{Id: "unused", Rules: []*common.RateLimitRuleConfig{{Method: "*"}}},
		},
	}
	cfg.Projects[0].RateLimitBudget = "project-wide"

	report := GenerateValidationReport(context.Background(), cfg)

	require.NotContains(t, joined(report.Notices), "orphan rateLimit budget 'project-wide'",
		"a budget used at project level is in force and must not be called orphaned")
	require.Contains(t, joined(report.Notices), "orphan rateLimit budget 'unused'",
		"a budget nothing references must still be reported")
	require.Equal(t, 2, report.Resources.Totals.RateLimitBudgetsTotal)
}

// TestValidate_DoesNotCallAConsumerAuthBudgetAnOrphan is the same rule for a
// budget reachable only through a project auth strategy. That path limits real
// consumer traffic, so the budget is in force.
func TestValidate_DoesNotCallAConsumerAuthBudgetAnOrphan(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	cfg := validationConfig(evmUpstream("node-a", node.URL, 123))
	cfg.RateLimiters = &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{
			{Id: "consumer-budget", Rules: []*common.RateLimitRuleConfig{{Method: "*"}}},
		},
	}
	cfg.Projects[0].Auth = &common.AuthConfig{
		Strategies: []*common.AuthStrategyConfig{
			{Type: common.AuthTypeSecret, RateLimitBudget: "consumer-budget",
				Secret: &common.SecretStrategyConfig{Value: "s3cret"}},
		},
	}

	report := GenerateValidationReport(context.Background(), cfg)

	require.NotContains(t, joined(report.Notices), "orphan rateLimit budget 'consumer-budget'",
		"a budget used by a consumer auth strategy limits real traffic")
}

// TestValidate_ShowsAnUpstreamWhoseChainMatchesNoNetwork covers the resource
// tree an operator reads to confirm what they configured. An upstream on a chain
// no network declares must still appear, or it vanishes from the one view that
// was supposed to show everything.
func TestValidate_ShowsAnUpstreamWhoseChainMatchesNoNetwork(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	cfg := validationConfig(
		evmUpstream("on-chain-123", node.URL, 123),
		evmUpstream("on-chain-999", deadNode(t), 999),
	)

	report := GenerateValidationReport(context.Background(), cfg)

	require.Equal(t, 2, report.Resources.Totals.UpstreamsTotal)
	require.Len(t, report.Resources.Tree.Projects, 1)
	networks := report.Resources.Tree.Projects[0].Networks
	require.Len(t, networks, 1, "the config declares one network")

	ids := []string{}
	for _, u := range networks[0].Upstreams {
		ids = append(ids, u.Id)
	}
	require.ElementsMatch(t, []string{"on-chain-123", "on-chain-999"}, ids,
		"an upstream that matches no network must still be visible in the tree")
}
