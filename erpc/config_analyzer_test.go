package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// `erpc validate` is the last thing between an operator and a fleet that answers
// with the wrong chain's data. Every finding in this file is something the
// operator only learns from that command: a node on the wrong network, a node
// forked away from its peers, a rate-limit budget that protects nothing. A
// missing finding is silent — the command exits 0 and the config ships.

// fakeEvmNode is a stand-in EVM node for the validator's probes. It answers
// eth_chainId and eth_getBlockByNumber from per-test values, so a test can put
// one node on a different chain or a different fork and see what the report
// says. It records nothing: what matters here is the verdict, not the traffic.
type fakeEvmNode struct {
	*httptest.Server

	// chainIdHex is what eth_chainId returns, e.g. "0x7b" for 123.
	chainIdHex string
	// genesisHash answers eth_getBlockByNumber("0x0").
	genesisHash string
	// finalizedNum is the block number reported for the "finalized" tag.
	finalizedNum int64
	// latestNum is the block number reported for the "latest" tag.
	latestNum int64
	// deepHash answers every numeric block tag — the historical comparison
	// point the validator picks below the finalized head.
	deepHash string

	// supportsFinalized off makes the node answer the "finalized" tag with an
	// empty result, the way a chain without finality does.
	supportsFinalized bool
}

func newFakeEvmNode(t *testing.T, n *fakeEvmNode) *fakeEvmNode {
	t.Helper()
	n.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Id     interface{}   `json:"id"`
			Method string        `json:"method"`
			Params []interface{} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)

		reply := func(result interface{}) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.Id,
				"result":  result,
			})
		}

		switch req.Method {
		case "eth_chainId":
			reply(n.chainIdHex)
		case "eth_syncing":
			reply(false)
		case "eth_blockNumber":
			reply(fmt.Sprintf("0x%x", n.latestNum))
		case "eth_getBlockByNumber":
			tag, _ := req.Params[0].(string)
			switch tag {
			case "0x0":
				reply(map[string]interface{}{"number": "0x0", "hash": n.genesisHash})
			case "latest":
				reply(map[string]interface{}{
					"number": fmt.Sprintf("0x%x", n.latestNum),
					"hash":   n.deepHash,
				})
			case "finalized":
				if !n.supportsFinalized {
					reply(nil)
					return
				}
				reply(map[string]interface{}{
					"number": fmt.Sprintf("0x%x", n.finalizedNum),
					"hash":   n.deepHash,
				})
			default:
				reply(map[string]interface{}{"number": tag, "hash": n.deepHash})
			}
		default:
			reply(nil)
		}
	}))
	t.Cleanup(n.Server.Close)
	return n
}

// evmNode builds a healthy node on chain 123 that agrees with its peers
// everywhere. A test then changes the one field it wants to disagree about.
func evmNode(t *testing.T) *fakeEvmNode {
	t.Helper()
	return newFakeEvmNode(t, &fakeEvmNode{
		chainIdHex:        "0x7b", // 123
		genesisHash:       "0xaaaa000000000000000000000000000000000000000000000000000000000000",
		deepHash:          "0xbbbb000000000000000000000000000000000000000000000000000000000000",
		finalizedNum:      1000,
		latestNum:         1100,
		supportsFinalized: true,
	})
}

// validationConfig wraps upstreams in the smallest config the validator accepts.
// Metrics is set because GenerateValidationReport reads cfg.Metrics without a
// nil check; every real caller has run SetDefaults by then.
func validationConfig(ups ...*common.UpstreamConfig) *common.Config {
	return &common.Config{
		Metrics: &common.MetricsConfig{},
		Projects: []*common.ProjectConfig{
			{
				Id:        "main",
				Networks:  []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}},
				Upstreams: ups,
			},
		},
	}
}

// validationLogger keeps AnalyseConfig's startup chatter out of the test output
// while still exercising the code that writes it.
func validationLogger() zerolog.Logger {
	return zerolog.New(io.Discard)
}

func evmUpstream(id, endpoint string, chainId int64) *common.UpstreamConfig {
	return &common.UpstreamConfig{
		Id:       id,
		Type:     common.UpstreamTypeEvm,
		Endpoint: endpoint,
		Evm:      &common.EvmUpstreamConfig{ChainId: chainId},
	}
}

// joined flattens a finding list so a test can assert on substrings without
// depending on the order the concurrent probes finished in.
func joined(findings []string) string { return strings.Join(findings, "\n") }

// TestValidate_ReportsAnUpstreamOnTheWrongChain covers the single most damaging
// misconfiguration: an endpoint pointing at another network. Serving it would
// return real, well-formed, wrong data, so the validator must call it an error
// and not a warning.
func TestValidate_ReportsAnUpstreamOnTheWrongChain(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	node.chainIdHex = "0x1" // mainnet, while the config claims chain 123

	cfg := validationConfig(evmUpstream("wrong-chain", node.URL, 123))
	report := GenerateValidationReport(context.Background(), cfg)

	require.Contains(t, joined(report.Errors), "reported chain ID 1 but the configuration expects 123",
		"an upstream on another chain must be an error; a warning would let it ship")
}

// TestValidate_AcceptsAnUpstreamThatMatchesItsConfiguredChain is the other half
// of the same branch. Without it, a validator that flagged every upstream would
// also pass the test above.
func TestValidate_AcceptsAnUpstreamThatMatchesItsConfiguredChain(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	cfg := validationConfig(evmUpstream("right-chain", node.URL, 123))
	report := GenerateValidationReport(context.Background(), cfg)

	require.Empty(t, report.Errors, "a correctly configured upstream must produce no errors")
	require.NotContains(t, joined(report.Warnings), "chain ID",
		"a correctly configured upstream must produce no chain ID warnings")
}

// TestValidate_ReportsAnEmptyEndpointBeforeProbing pins that a blank endpoint is
// named as an error rather than crashing the validator or being silently
// probed as an empty URL.
func TestValidate_ReportsAnEmptyEndpointBeforeProbing(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	cfg := validationConfig(&common.UpstreamConfig{
		Id:       "blank",
		Type:     common.UpstreamTypeEvm,
		Endpoint: "   ",
		Evm:      &common.EvmUpstreamConfig{ChainId: 123},
	})
	report := GenerateValidationReport(context.Background(), cfg)

	require.Contains(t, joined(report.Errors), "upstream 'blank' has an empty endpoint URL")
}

// TestValidate_CallsAForkedUpstreamAnErrorOnlyWhenAMajorityDisagrees is the
// genesis-hash consensus rule. Two upstreams that differ prove one of them is
// wrong but not which, so the operator gets a warning. A third that agrees with
// one of them settles it, and the odd one out becomes an error.
func TestValidate_CallsAForkedUpstreamAnErrorOnlyWhenAMajorityDisagrees(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	const forkedGenesis = "0xdead000000000000000000000000000000000000000000000000000000000000"

	t.Run("TwoUpstreamsDisagreeSoTheVerdictIsAWarning", func(t *testing.T) {
		good := evmNode(t)
		forked := evmNode(t)
		forked.genesisHash = forkedGenesis

		cfg := validationConfig(
			evmUpstream("good", good.URL, 123),
			evmUpstream("forked", forked.URL, 123),
		)
		report := GenerateValidationReport(context.Background(), cfg)

		require.NotContains(t, joined(report.Errors), "genesis hash mismatch",
			"with only two votes the validator cannot say which upstream is wrong")
		require.Contains(t, joined(report.Warnings), "genesis hash differs from other upstream(s)")
		require.Contains(t, joined(report.Warnings), "only 2 upstreams compared")
	})

	t.Run("AMajorityOfThreeMakesTheOddOneOutAnError", func(t *testing.T) {
		goodA := evmNode(t)
		goodB := evmNode(t)
		forked := evmNode(t)
		forked.genesisHash = forkedGenesis

		cfg := validationConfig(
			evmUpstream("good-a", goodA.URL, 123),
			evmUpstream("good-b", goodB.URL, 123),
			evmUpstream("forked", forked.URL, 123),
		)
		report := GenerateValidationReport(context.Background(), cfg)

		errs := joined(report.Errors)
		require.Contains(t, errs, "upstream=forked")
		require.Contains(t, errs, "genesis hash mismatch (majority 2/3 agree on")
		require.NotContains(t, errs, "upstream=good-a",
			"the two agreeing upstreams must not be blamed for the third one's fork")
		require.NotContains(t, errs, "upstream=good-b")
	})
}

// TestValidate_ComparesHistoryBelowTheFinalizedHead checks the deep-history
// probe. Genesis alone cannot catch a node that forked last week, so the
// validator re-checks a block that every node should long since agree on. It
// picks the lowest finalized head across the group and drops another 64 blocks,
// because comparing anywhere near the tip produces false alarms on every reorg.
func TestValidate_ComparesHistoryBelowTheFinalizedHead(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	goodA := evmNode(t)
	goodB := evmNode(t)
	forked := evmNode(t)
	// Same genesis as its peers, so only the history probe can catch it.
	forked.deepHash = "0xcccc000000000000000000000000000000000000000000000000000000000000"
	// The lowest finalized head in the group; the comparison point derives
	// from this one, not from the higher heads.
	forked.finalizedNum = 800

	cfg := validationConfig(
		evmUpstream("good-a", goodA.URL, 123),
		evmUpstream("good-b", goodB.URL, 123),
		evmUpstream("forked", forked.URL, 123),
	)
	report := GenerateValidationReport(context.Background(), cfg)

	errs := joined(report.Errors)
	require.Contains(t, errs, "block=736",
		"the comparison block must be min(finalized)=800 less the 64-block reorg margin")
	require.Contains(t, errs, "upstream=forked")
	require.Contains(t, errs, "hash mismatch (majority 2/3 agree on")
}

// TestValidate_FallsBackToADeepLatestBlockWhenFinalityIsUnavailable covers
// chains with no finalized tag. The validator must still compare history, and
// it must go far enough back — 1024 blocks — that the answer is stable.
func TestValidate_FallsBackToADeepLatestBlockWhenFinalityIsUnavailable(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	makeNode := func() *fakeEvmNode {
		n := evmNode(t)
		n.supportsFinalized = false
		n.latestNum = 5000
		return n
	}
	goodA, goodB := makeNode(), makeNode()
	forked := makeNode()
	forked.deepHash = "0xcccc000000000000000000000000000000000000000000000000000000000000"
	forked.latestNum = 4000 // the lowest latest head decides the target

	cfg := validationConfig(
		evmUpstream("good-a", goodA.URL, 123),
		evmUpstream("good-b", goodB.URL, 123),
		evmUpstream("forked", forked.URL, 123),
	)
	report := GenerateValidationReport(context.Background(), cfg)

	errs := joined(report.Errors)
	require.Contains(t, errs, "block=2976",
		"without finality the comparison point is min(latest)=4000 less 1024 blocks")
	require.Contains(t, errs, "upstream=forked")
}

// nonEvmNode refuses every eth_* call the way a real Solana node does: a
// JSON-RPC "method not found". A node that answered them would make the skip
// below untestable, because probing it would look identical to not probing it.
func nonEvmNode(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Id     interface{} `json:"id"`
			Method string      `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.Id,
			"error": map[string]interface{}{
				"code":    -32601,
				"message": "Method not found: " + req.Method,
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestValidate_SkipsEvmProbesForANonEvmUpstream pins a deliberate silence. A
// Solana upstream cannot answer eth_chainId, and reporting that as a problem
// buries the real findings under noise the operator cannot act on.
func TestValidate_SkipsEvmProbesForANonEvmUpstream(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := nonEvmNode(t)
	cfg := validationConfig(&common.UpstreamConfig{
		Id:       "solana",
		Type:     common.UpstreamTypeSvm,
		Endpoint: node.URL,
	})
	report := GenerateValidationReport(context.Background(), cfg)

	require.Empty(t, report.Errors)
	require.Empty(t, report.Warnings,
		"a non-EVM upstream must not be probed with EVM methods")
}

// TestValidate_SkipsGenesisForANodeThatCannotHaveIt states why the skip exists:
// a pruned or full node legitimately has no block 0, so asking for it produces
// a failure that means nothing. The notice keeps the skip visible.
func TestValidate_SkipsGenesisForANodeThatCannotHaveIt(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	ups := evmUpstream("pruned", node.URL, 123)
	ups.Evm.NodeType = common.EvmNodeTypeFull

	report := GenerateValidationReport(context.Background(), validationConfig(ups))

	require.Contains(t, joined(report.Notices), "skipped genesis hash check")
	require.Empty(t, report.Errors)
}

// TestValidate_RejectsAnUnparsableHistogramBucketList covers the one static
// check that can take down the metrics endpoint. A bad bucket string silently
// falls back to defaults at runtime, so validation is the only place an
// operator finds out their tuning never applied.
func TestValidate_RejectsAnUnparsableHistogramBucketList(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	cfg := validationConfig()
	cfg.Metrics.HistogramBuckets = "0.1,not-a-number,5"
	report := GenerateValidationReport(context.Background(), cfg)
	require.Contains(t, joined(report.Errors), "invalid metrics histogramBuckets")

	// Restore the process-wide histograms this call replaced, so later tests
	// in the package measure against the same buckets they started with.
	cfg.Metrics.HistogramBuckets = ""
	require.Empty(t, GenerateValidationReport(context.Background(), cfg).Errors)
}

// TestValidate_NoticesRateLimitBudgetsThatProtectNothing covers the two budget
// findings. An orphan budget is a limit the operator believes is in force and
// is not. A budget without auto-tune throttles an upstream that could have
// recovered on its own.
func TestValidate_NoticesRateLimitBudgetsThatProtectNothing(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	ups := evmUpstream("u1", node.URL, 123)
	ups.RateLimitBudget = "used-budget"

	cfg := validationConfig(ups)
	cfg.RateLimiters = &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{
			{Id: "used-budget", Rules: []*common.RateLimitRuleConfig{{Method: "*"}}},
			{Id: "nobody-uses-me"},
		},
	}

	report := GenerateValidationReport(context.Background(), cfg)
	notices := joined(report.Notices)

	require.Contains(t, notices, "orphan rateLimit budget 'nobody-uses-me'")
	require.NotContains(t, notices, "orphan rateLimit budget 'used-budget'",
		"a budget referenced by an upstream is not an orphan")
	require.Contains(t, notices, "rateLimit budget 'used-budget' defined but auto-tune disabled")

	require.Equal(t, 2, report.Resources.Totals.RateLimitBudgetsTotal)
	require.Len(t, report.Resources.Tree.RateLimiters.Budgets, 2)
	require.Equal(t, 1, report.Resources.Tree.RateLimiters.Budgets[0].RulesCount)
}

// TestValidate_CountsABudgetUsedOnlyByAProviderOverrideAsUsed guards a false
// positive. Provider overrides are the least obvious place a budget is
// referenced, so if the orphan scan misses them the operator is told to delete
// a budget that is live.
func TestValidate_CountsABudgetUsedOnlyByAProviderOverrideAsUsed(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	cfg := validationConfig()
	cfg.Projects[0].Providers = []*common.ProviderConfig{
		{
			Id: "alchemy",
			Overrides: map[string]*common.UpstreamConfig{
				"evm:123": {RateLimitBudget: "provider-budget"},
			},
		},
	}
	cfg.RateLimiters = &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{{Id: "provider-budget"}},
	}

	report := GenerateValidationReport(context.Background(), cfg)
	require.NotContains(t, joined(report.Notices), "orphan rateLimit budget 'provider-budget'")
}

// TestValidate_CountsABudgetUsedOnlyByAdminAuthAsUsed is the same guard for the
// admin surface, which lives outside the project loop entirely.
func TestValidate_CountsABudgetUsedOnlyByAdminAuthAsUsed(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	cfg := validationConfig()
	cfg.Admin = &common.AdminConfig{
		Auth: &common.AuthConfig{
			Strategies: []*common.AuthStrategyConfig{
				{Type: common.AuthTypeSecret, RateLimitBudget: "admin-budget"},
			},
		},
	}
	cfg.RateLimiters = &common.RateLimiterConfig{
		Budgets: []*common.RateLimitBudgetConfig{{Id: "admin-budget"}},
	}

	report := GenerateValidationReport(context.Background(), cfg)
	require.NotContains(t, joined(report.Notices), "orphan rateLimit budget 'admin-budget'")
}

// TestValidate_NoticesANetworkWithNoFailsafePolicy covers the notice an
// operator most often needs: a network with no retry, hedge or timeout policy
// passes every other check and then serves a single upstream failure straight
// to the caller.
func TestValidate_NoticesANetworkWithNoFailsafePolicy(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	t.Run("NeitherDefaultsNorTheNetworkDefineOne", func(t *testing.T) {
		cfg := validationConfig()
		report := GenerateValidationReport(context.Background(), cfg)
		require.Contains(t, joined(report.Notices), "network=evm/123 has no failsafe policies")
	})

	t.Run("NetworkDefaultsCoverEveryNetwork", func(t *testing.T) {
		cfg := validationConfig()
		cfg.Projects[0].NetworkDefaults = &common.NetworkDefaults{
			Failsafe: []*common.FailsafeConfig{{MatchMethod: "*"}},
		}
		report := GenerateValidationReport(context.Background(), cfg)
		require.NotContains(t, joined(report.Notices), "has no failsafe policies",
			"a defaults-level policy applies to every network and must silence the notice")
	})
}

// TestValidate_BuildsAResourceTreeAnOperatorCanRead pins the tree the CLI
// prints. Upstreams attach to the network that shares their chain ID; anything
// unmatched still appears rather than vanishing from the listing.
func TestValidate_BuildsAResourceTreeAnOperatorCanRead(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	nodeA, nodeB := evmNode(t), evmNode(t)
	cfg := &common.Config{
		Metrics: &common.MetricsConfig{},
		Projects: []*common.ProjectConfig{
			{
				Id: "main",
				Networks: []*common.NetworkConfig{
					{Architecture: common.ArchitectureEvm, Alias: "eth", Evm: &common.EvmNetworkConfig{ChainId: 1}},
					{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}},
				},
				Upstreams: []*common.UpstreamConfig{
					evmUpstream("on-123", nodeA.URL, 123),
					// No chain ID in config: it cannot be matched to a network,
					// so it must land on the first one and stay visible.
					{Id: "chainless", Type: common.UpstreamTypeSvm, Endpoint: nodeB.URL},
				},
			},
		},
	}

	report := GenerateValidationReport(context.Background(), cfg)

	require.Equal(t, 1, report.Resources.Totals.ProjectsTotal)
	require.Equal(t, 2, report.Resources.Totals.NetworksTotal)
	require.Equal(t, 2, report.Resources.Totals.UpstreamsTotal)

	tree := report.Resources.Tree.Projects
	require.Len(t, tree, 1)
	require.Equal(t, "evm:1", tree[0].Networks[0].Id)
	require.Equal(t, "eth", tree[0].Networks[0].Alias)
	require.Equal(t, "evm:123", tree[0].Networks[1].Id)

	require.Equal(t, []UpstreamNode{{Id: "chainless"}}, tree[0].Networks[0].Upstreams,
		"an upstream with no chain ID falls to the first network rather than disappearing")
	require.Equal(t, []UpstreamNode{{Id: "on-123"}}, tree[0].Networks[1].Upstreams,
		"an upstream must attach to the network with its chain ID, not the first one")
}

// TestRenderValidationReport_KeepsEveryFindingVisible covers both renderers.
// The markdown form is what lands in a CI comment, so an error that is present
// in the struct and absent from the render is a finding nobody reads.
func TestRenderValidationReport_KeepsEveryFindingVisible(t *testing.T) {
	report := &ValidationReport{
		Errors:   []string{"upstream u1 is on the wrong chain"},
		Warnings: []string{"upstream u2 did not answer"},
		Notices:  []string{"orphan rateLimit budget 'b1'"},
		Resources: ValidationResources{
			Totals: ValidationTotals{ProjectsTotal: 1, NetworksTotal: 2, UpstreamsTotal: 3, RateLimitBudgetsTotal: 1},
			Tree: ValidationResourcesTree{
				Projects: []ProjectNode{{
					Id: "main",
					Networks: []NetworkNode{
						{Id: "evm:1", Alias: "eth", Upstreams: []UpstreamNode{{Id: "u1"}}},
						{Id: "evm:123"},
					},
				}},
				RateLimiters: RateLimitersNode{Budgets: []RateLimitBudgetNode{{Id: "b1", RulesCount: 2}}},
			},
		},
	}

	md := RenderValidationReportMarkdown(report)
	require.Contains(t, md, "upstream u1 is on the wrong chain")
	require.Contains(t, md, "Warnings (1)")
	require.Contains(t, md, "upstream u2 did not answer")
	require.Contains(t, md, "Notices (1)")
	require.Contains(t, md, "orphan rateLimit budget 'b1'")
	require.Contains(t, md, "- upstreamsTotal: 3")
	require.Contains(t, md, "network evm:1 (eth)")
	require.Contains(t, md, "    upstream u1")
	require.Contains(t, md, "  budget b1 rules=2")

	compact, err := RenderValidationReportJSON(report, false)
	require.NoError(t, err)
	require.NotContains(t, compact, "\n", "the compact form must stay one line for log ingestion")

	pretty, err := RenderValidationReportJSON(report, true)
	require.NoError(t, err)
	require.Contains(t, pretty, "\n  \"errors\": [")

	var roundTrip ValidationReport
	require.NoError(t, json.Unmarshal([]byte(compact), &roundTrip))
	require.Equal(t, report.Errors, roundTrip.Errors)
	require.Equal(t, 3, roundTrip.Resources.Totals.UpstreamsTotal)
}

// TestRenderValidationReportMarkdown_OmitsEmptySections keeps a clean run
// readable: no empty "Warnings (0)" block for an operator to scan past.
func TestRenderValidationReportMarkdown_OmitsEmptySections(t *testing.T) {
	md := RenderValidationReportMarkdown(&ValidationReport{})
	require.NotContains(t, md, "### Errors")
	require.NotContains(t, md, "Warnings (")
	require.NotContains(t, md, "Notices (")
	require.Contains(t, md, "### Resources")
}

// TestIsLocalEndpoint_RecognisesEveryPrivateAddressShape backs
// ERPC_IGNORE_LOCAL_ENDPOINT_VALIDATION. A private address the function fails
// to recognise gets probed at startup and blocks the boot of a deployment that
// cannot reach it — the exact case the flag exists for.
func TestIsLocalEndpoint_RecognisesEveryPrivateAddressShape(t *testing.T) {
	local := []string{
		"http://localhost:8545",
		"http://127.0.0.1:8545",
		"http://[::1]:8545",
		"http://erpc-node-0.erpc.svc.cluster.local:8545",
		"http://10.0.0.4:8545",
		"http://192.168.1.10:8545",
		"http://172.16.5.5:8545",
		"http://[fd00::1]:8545",
	}
	for _, endpoint := range local {
		got, err := isLocalEndpoint(endpoint)
		require.NoError(t, err, endpoint)
		require.True(t, got, "%s is unreachable from outside the cluster and must count as local", endpoint)
	}

	public := []string{
		"https://eth-mainnet.example.com",
		"https://8.8.8.8",
		"http://172.32.0.1:8545", // just outside the 172.16/12 private range
	}
	for _, endpoint := range public {
		got, err := isLocalEndpoint(endpoint)
		require.NoError(t, err, endpoint)
		require.False(t, got, "%s is a public endpoint and must still be validated", endpoint)
	}

	// A URL with no host at all is not local — treating it as local would skip
	// validation for a string that is simply malformed.
	got, err := isLocalEndpoint("not-a-url")
	require.NoError(t, err)
	require.False(t, got)

	_, err = isLocalEndpoint("http://bad\x7fhost")
	require.Error(t, err, "an unparsable endpoint must surface as an error, not as 'not local'")
}

// TestValidateUpstreamEndpoints_MisparsesTheChainIdItJustFetched PINS A BUG. It
// asserts today's broken behaviour on purpose, so that fixing the bug makes this
// test fail and whoever fixes it finds this note.
//
// THE BUG. AnalyseConfig -> validateUpstreamEndpoints reads the chain ID with
// Upstream.EvmGetChainId, which returns a DECIMAL string
// (upstream/evm_upstream_ops.go:51 formats it with strconv.FormatUint(dec, 10)).
// config_analyzer.go:1087 then hands that decimal to common.HexToInt64, which
// demands a 0x-prefixed value and rejects it. So a HEALTHY upstream on the
// RIGHT chain fails the boot gate with "invalid hex string", and the mismatch
// check three lines further down (config_analyzer.go:1094) can never run.
//
// The same file gets it right on the other path: GenerateValidationReport uses
// strconv.ParseInt(chainStr, 0, 0) at config_analyzer.go:398, which accepts the
// decimal. That disagreement between two readers of the same value is the tell.
//
// Blast radius today: AnalyseConfig has no caller in this repo, so nothing boots
// through it. It is exported, so a fork or a future CLI path that calls it would
// refuse to start against a perfectly good fleet.
func TestValidateUpstreamEndpoints_MisparsesTheChainIdItJustFetched(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	t.Run("AMatchingChainIdIsRejectedAnyway", func(t *testing.T) {
		node := evmNode(t) // returns 0x7b, exactly what the config asks for
		err := AnalyseConfig(context.Background(),
			validationConfig(evmUpstream("u1", node.URL, 123)), validationLogger())
		require.Error(t, err, "read the note above: this SHOULD be nil")
		require.Contains(t, err.Error(), "failed to parse chain id")
		require.Contains(t, err.Error(), "invalid hex string: 123",
			"the decimal chain ID reaches a hex-only parser")
	})

	t.Run("AMismatchNeverReachesTheMismatchCheck", func(t *testing.T) {
		node := evmNode(t)
		node.chainIdHex = "0x1" // mainnet, while the config claims chain 123

		err := AnalyseConfig(context.Background(),
			validationConfig(evmUpstream("u1", node.URL, 123)), validationLogger())
		require.Error(t, err)
		require.NotContains(t, err.Error(), "chain id mismatch",
			"the gate aborts on the parse, so it never gets to compare the two chain IDs")
		require.Contains(t, err.Error(), "invalid hex string: 1")
	})
}

// TestValidateUpstreamEndpoints_RejectsABlankEndpoint keeps the startup gate
// from constructing an upstream with nowhere to send traffic.
func TestValidateUpstreamEndpoints_RejectsABlankEndpoint(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	cfg := validationConfig(evmUpstream("u1", "", 123))
	err := AnalyseConfig(context.Background(), cfg, validationLogger())
	require.Error(t, err)
	require.Contains(t, err.Error(), "upstream endpoint is empty")
}

// TestValidateUpstreamEndpoints_SkipsAnUpstreamWithoutAChainId documents a
// deliberate hole: an upstream with no configured chain ID cannot be checked
// against one, so the boot proceeds with a warning instead of failing.
func TestValidateUpstreamEndpoints_SkipsAnUpstreamWithoutAChainId(t *testing.T) {
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	// The upstream is skipped before eth_chainId is ever sent, so neither the
	// mismatch check nor the parse bug above can be reached from here. This is
	// also the only path on which AnalyseConfig currently reaches its stats
	// printing.
	node.chainIdHex = "0x1"

	cfg := validationConfig(&common.UpstreamConfig{
		Id:       "no-chain",
		Type:     common.UpstreamTypeEvm,
		Endpoint: node.URL,
	})
	require.NoError(t, AnalyseConfig(context.Background(), cfg, validationLogger()))
}

// TestCalculateConfigStats_NamesAnUpstreamThatHasNoId covers the display-name
// fallback. Startup stats are how an operator confirms the config that loaded
// is the config they wrote, so an unnamed upstream must still be counted and
// labelled rather than printed as an empty string.
func TestCalculateConfigStats_NamesAnUpstreamThatHasNoId(t *testing.T) {
	cfg := &common.Config{
		Projects: []*common.ProjectConfig{{
			Id: "main",
			Upstreams: []*common.UpstreamConfig{
				{Id: "named", Type: common.UpstreamTypeEvm},
				{Type: common.UpstreamTypeEvm, VendorName: "alchemy"},
				{Type: common.UpstreamTypeEvm},
			},
		}},
	}

	stats := calculateConfigStats(cfg)
	require.Equal(t, 3, stats.Projects[0].UpstreamCount)
	require.Equal(t, []string{"named", "evm-alchemy", "evm-unknown"}, stats.Projects[0].Upstreams)
}

// TestCalculateConfigStats_TracesEveryPlaceABudgetIsUsed covers the orphan scan
// that feeds the startup warning. Each source the scan forgets turns a live
// budget into a phantom orphan the operator may then delete.
func TestCalculateConfigStats_TracesEveryPlaceABudgetIsUsed(t *testing.T) {
	cfg := &common.Config{
		RateLimiters: &common.RateLimiterConfig{
			Budgets: []*common.RateLimitBudgetConfig{
				{Id: "proj"}, {Id: "ups"}, {Id: "net"}, {Id: "auth"}, {Id: "admin"}, {Id: "orphan"},
			},
		},
		Admin: &common.AdminConfig{
			Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
				{Type: common.AuthTypeSecret, RateLimitBudget: "admin"},
			}},
		},
		Projects: []*common.ProjectConfig{{
			Id:              "main",
			RateLimitBudget: "proj",
			Upstreams:       []*common.UpstreamConfig{{Id: "u1", RateLimitBudget: "ups"}},
			Networks: []*common.NetworkConfig{
				{Architecture: common.ArchitectureEvm, RateLimitBudget: "net", Evm: &common.EvmNetworkConfig{ChainId: 1}},
			},
			Auth: &common.AuthConfig{Strategies: []*common.AuthStrategyConfig{
				{Type: common.AuthTypeSecret, RateLimitBudget: "auth"},
			}},
		}},
	}

	stats := calculateConfigStats(cfg)

	require.Equal(t, 6, stats.RateLimits.TotalBudgets)
	require.Equal(t, []string{"orphan"}, stats.RateLimits.OrphanBudgets,
		"every budget except 'orphan' is referenced somewhere and must not be reported")

	require.Equal(t, []string{"project:main"}, stats.RateLimits.UsageBySources["proj"])
	require.Equal(t, []string{"project:main:upstream:u1"}, stats.RateLimits.UsageBySources["ups"])
	require.Equal(t, []string{"project:main:network:evm"}, stats.RateLimits.UsageBySources["net"])
	require.Equal(t, []string{"project:main:auth:secret"}, stats.RateLimits.UsageBySources["auth"])
	require.Equal(t, []string{"admin:auth:secret"}, stats.RateLimits.UsageBySources["admin"])

	require.Equal(t, 1, stats.ProjectCount)
	require.Equal(t, 1, stats.Projects[0].NetworkCount)
	require.Equal(t, []string{"evm"}, stats.Projects[0].Networks)
}
