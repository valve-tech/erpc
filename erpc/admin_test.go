package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// The admin surface is what an operator reaches for when production is already
// unhappy: take a bad node out of rotation, put it back, see who is out, revoke
// a leaked API key. Each of those is a write against a running gateway, so an
// admin handler that silently does nothing is worse than one that errors — the
// operator believes the node is out and moves on.
//
// These tests drive AdminHandleRequest directly. The HTTP layer's allow/deny
// filter is already covered by TestHttpServer_AdminMethodFilter; what happens
// after the request gets through was not.

// adminRequest builds the JSON-RPC envelope AdminHandleRequest expects.
func adminRequest(t *testing.T, method string, params string) *common.NormalizedRequest {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":%s}`, method, params)
	return common.NewNormalizedRequest([]byte(body))
}

// adminResult runs an admin call and decodes its result into a map.
func adminResult(t *testing.T, e *ERPC, method, params string) map[string]interface{} {
	t.Helper()
	resp, err := e.AdminHandleRequest(context.Background(), adminRequest(t, method, params))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(jrr.GetResultString()), &out))
	return out
}

// startAdminErpc builds a real eRPC over two live EVM nodes and returns the
// instance plus its project. Two upstreams matter: cordoning one must leave the
// other serving, which is the whole point of the lever.
func startAdminErpc(t *testing.T, ctx context.Context, projectCfg *common.ProjectConfig) *ERPC {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	cfg := &common.Config{
		Projects:     []*common.ProjectConfig{projectCfg},
		RateLimiters: &common.RateLimiterConfig{},
	}
	require.NoError(t, cfg.SetDefaults(nil))

	lg := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.ErrorLevel)
	ssr, err := data.NewSharedStateRegistry(ctx, &lg, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "100MB"},
		},
	})
	require.NoError(t, err)

	instance, err := NewERPC(ctx, &lg, ssr, nil, nil, cfg)
	require.NoError(t, err)
	instance.Bootstrap(ctx)

	// Force the network's upstreams into the registry so the cordon handlers
	// have something to find, the way a first served request would.
	if len(projectCfg.Networks) > 0 {
		nw, err := instance.GetNetwork(ctx, projectCfg.Id, "evm:123")
		require.NoError(t, err)
		require.NoError(t, nw.upstreamsRegistry.PrepareUpstreamsForNetwork(ctx, "evm:123"))
	}
	return instance
}

// twoNodeProject is the standard fixture: one project, one EVM network, two
// healthy upstreams on chain 123.
func twoNodeProject(t *testing.T) *common.ProjectConfig {
	t.Helper()
	nodeA, nodeB := evmNode(t), evmNode(t)
	return &common.ProjectConfig{
		Id:       "prod",
		Networks: []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Alias: "eth-like", Evm: &common.EvmNetworkConfig{ChainId: 123}}},
		Upstreams: []*common.UpstreamConfig{
			evmUpstream("node-a", nodeA.URL, 123),
			evmUpstream("node-b", nodeB.URL, 123),
		},
	}
}

// TestAdmin_RefusesAMethodItDoesNotImplement pins the fallthrough. An operator
// who mistypes a method must get a clear rejection, not a silent success that
// leaves them believing an action was taken.
func TestAdmin_RefusesAMethodItDoesNotImplement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t))

	_, err := e.AdminHandleRequest(ctx, adminRequest(t, "erpc_cordonUpstreams", `[{}]`))
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeEndpointUnsupported),
		"an unknown admin method must be rejected as unsupported, got %v", err)
}

// TestAdmin_CordonTakesAnUpstreamOutOfRotationAndUncordonPutsItBack is the
// lever's core promise. Cordon is how an operator stops traffic to a node that
// is serving bad data; uncordon is how they end an incident. Neither may be a
// no-op, and neither may touch the other upstream.
func TestAdmin_CordonTakesAnUpstreamOutOfRotationAndUncordonPutsItBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t))

	nodeA, err := e.findUpstreamById("prod", "node-a")
	require.NoError(t, err)
	nodeB, err := e.findUpstreamById("prod", "node-b")
	require.NoError(t, err)

	_, cordoned := nodeA.CordonedReason("*")
	require.False(t, cordoned, "nothing is cordoned before the operator asks")

	result := adminResult(t, e, "erpc_cordonUpstream",
		`[{"projectId":"prod","upstream":"node-a","reason":"serving stale heads"}]`)
	require.Equal(t, true, result["cordoned"])
	require.Equal(t, "node-a", result["upstream"])
	require.Equal(t, "*", result["method"],
		"an operator who names no method means the whole upstream")
	require.Equal(t, "serving stale heads", result["reason"])

	reason, cordoned := nodeA.CordonedReason("*")
	require.True(t, cordoned, "the cordon call must actually mark the upstream, not just answer")
	require.Equal(t, "serving stale heads", reason,
		"the operator's reason has to survive: it is what the next responder reads")

	_, cordoned = nodeB.CordonedReason("*")
	require.False(t, cordoned, "cordoning one upstream must not take the whole project out")

	result = adminResult(t, e, "erpc_uncordonUpstream",
		`[{"projectId":"prod","upstream":"node-a"}]`)
	require.Equal(t, false, result["cordoned"])

	_, cordoned = nodeA.CordonedReason("*")
	require.False(t, cordoned, "uncordon must clear the mark or the node never comes back")
}

// TestAdmin_RecordsADefaultReasonWhenTheOperatorGivesNone keeps the audit trail
// non-empty. "cordoned, reason: (blank)" tells the next responder nothing about
// whether the mark was deliberate.
func TestAdmin_RecordsADefaultReasonWhenTheOperatorGivesNone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t))

	result := adminResult(t, e, "erpc_cordonUpstream", `[{"projectId":"prod","upstream":"node-a"}]`)
	require.Equal(t, "admin: manual cordon", result["reason"])

	result = adminResult(t, e, "erpc_uncordonUpstream", `[{"projectId":"prod","upstream":"node-a"}]`)
	require.Equal(t, "admin: manual uncordon", result["reason"],
		"cordon and uncordon must be distinguishable in the audit trail")
}

// TestAdmin_CordonsASingleMethodWithoutTouchingTheRest covers the narrow scope.
// An upstream that only fails eth_getLogs stays useful for everything else, so
// a method-scoped cordon must not take it out wholesale.
func TestAdmin_CordonsASingleMethodWithoutTouchingTheRest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t))

	result := adminResult(t, e, "erpc_cordonUpstream",
		`[{"projectId":"prod","upstream":"node-a","method":"eth_getLogs","reason":"log index broken"}]`)
	require.Equal(t, "eth_getLogs", result["method"])

	nodeA, err := e.findUpstreamById("prod", "node-a")
	require.NoError(t, err)

	reason, cordoned := nodeA.CordonedReason("eth_getLogs")
	require.True(t, cordoned)
	require.Equal(t, "log index broken", reason)

	_, cordoned = nodeA.CordonedReason("*")
	require.False(t, cordoned,
		"a method-scoped cordon must leave the upstream serving every other method")
}

// TestAdmin_ListCordonedShowsOnlyWholeUpstreamCordons DOCUMENTS A GAP. The
// handler's own comment says it "returns every upstream currently cordoned in a
// project", and a reconcile script would read it that way. It does not: it asks
// each upstream for CordonedReason("*") only (admin.go:721), so a method-scoped
// cordon is invisible in the listing.
//
// Why it matters: an operator cordons eth_getLogs on one node during an
// incident, later runs erpc_listCordoned to check what is still marked, sees an
// empty list, and concludes the fleet is clean. The narrow cordon stays in
// place with nothing pointing at it. This test asserts today's behaviour so the
// gap is visible and a fix breaks the test rather than passing unnoticed.
func TestAdmin_ListCordonedShowsOnlyWholeUpstreamCordons(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t))

	listed := func() []interface{} {
		result := adminResult(t, e, "erpc_listCordoned", `[{"projectId":"prod"}]`)
		require.Equal(t, "prod", result["projectId"])
		rows, _ := result["cordoned"].([]interface{})
		return rows
	}

	require.Empty(t, listed(), "a healthy project lists nothing")

	adminResult(t, e, "erpc_cordonUpstream",
		`[{"projectId":"prod","upstream":"node-a","method":"eth_getLogs","reason":"log index broken"}]`)
	require.Empty(t, listed(),
		"read the note above: a method-scoped cordon SHOULD appear here and does not")

	adminResult(t, e, "erpc_cordonUpstream",
		`[{"projectId":"prod","upstream":"node-b","reason":"stale heads"}]`)
	rows := listed()
	require.Len(t, rows, 1)
	row, _ := rows[0].(map[string]interface{})
	require.Equal(t, "node-b", row["upstream"])
	require.Equal(t, "stale heads", row["reason"],
		"the listing has to carry the reason, or the operator cannot decide whether to lift it")
}

// TestAdmin_CordonRejectsAnIncompleteOrUnknownTarget keeps a mistyped cordon
// from reading as success. Every one of these returns an error today, and each
// one is a way an operator can believe a node is out when it is still serving.
func TestAdmin_CordonRejectsAnIncompleteOrUnknownTarget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t))

	for name, params := range map[string]string{
		"NoParamsAtAll":     `[]`,
		"NoProjectId":       `[{"upstream":"node-a"}]`,
		"NoUpstream":        `[{"projectId":"prod"}]`,
		"UnknownProject":    `[{"projectId":"staging","upstream":"node-a"}]`,
		"UnknownUpstream":   `[{"projectId":"prod","upstream":"node-z"}]`,
		"ParamsNotAnObject": `["node-a"]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := e.AdminHandleRequest(ctx, adminRequest(t, "erpc_cordonUpstream", params))
			require.Error(t, err, "a cordon that cannot be applied must not report success")
		})
	}

	// The listing has its own required-parameter check: without a project it
	// would otherwise have to guess which fleet the operator meant.
	_, err := e.AdminHandleRequest(ctx, adminRequest(t, "erpc_listCordoned", `[]`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "projectId is required")

	_, err = e.AdminHandleRequest(ctx, adminRequest(t, "erpc_listCordoned", `[{"projectId":"staging"}]`))
	require.Error(t, err, "listing an unknown project must fail rather than return an empty fleet")
}

// TestAdmin_TaxonomyNamesEveryProjectNetworkAndUpstream covers the read an
// operator or dashboard starts from. A network or upstream missing here reads
// as "not configured" and sends someone hunting a fault that does not exist.
func TestAdmin_TaxonomyNamesEveryProjectNetworkAndUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t))

	result := adminResult(t, e, "erpc_taxonomy", `[]`)
	projects, _ := result["projects"].([]interface{})
	require.Len(t, projects, 1)

	project, _ := projects[0].(map[string]interface{})
	require.Equal(t, "prod", project["id"])

	networks, _ := project["networks"].([]interface{})
	require.Len(t, networks, 1)
	network, _ := networks[0].(map[string]interface{})
	require.Equal(t, "evm:123", network["id"])
	require.Equal(t, "eth-like", network["alias"],
		"the alias is how an operator names the network in requests, so it belongs here")

	upstreams, _ := network["upstreams"].([]interface{})
	ids := []string{}
	for _, u := range upstreams {
		um, _ := u.(map[string]interface{})
		ids = append(ids, um["id"].(string))
	}
	require.ElementsMatch(t, []string{"node-a", "node-b"}, ids)
}

// TestAdmin_ProjectReturnsItsConfigAndHealth covers the per-project read. It
// also pins the two argument rejections: an operator who omits the project id
// must be told, not handed some default project's state.
func TestAdmin_ProjectReturnsItsConfigAndHealth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t))

	result := adminResult(t, e, "erpc_project", `["prod"]`)
	cfg, _ := result["config"].(map[string]interface{})
	require.Equal(t, "prod", cfg["id"])
	require.Contains(t, result, "health", "the health block is why an operator calls this")

	_, err := e.AdminHandleRequest(ctx, adminRequest(t, "erpc_project", `[]`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "project id (params[0]) is required")

	_, err = e.AdminHandleRequest(ctx, adminRequest(t, "erpc_project", `[123]`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "must be a string")

	_, err = e.AdminHandleRequest(ctx, adminRequest(t, "erpc_project", `["staging"]`))
	require.Error(t, err, "an unknown project must be an error, not an empty report")
}

// TestAdmin_ConfigReturnsTheConfigurationThatIsActuallyLoaded is how an operator
// confirms which config a running instance holds after a deploy. Returning a
// stale or empty document would end an investigation at the wrong conclusion.
func TestAdmin_ConfigReturnsTheConfigurationThatIsActuallyLoaded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t))

	result := adminResult(t, e, "erpc_config", `[]`)
	projects, _ := result["projects"].([]interface{})
	require.Len(t, projects, 1)
	project, _ := projects[0].(map[string]interface{})
	require.Equal(t, "prod", project["id"])

	upstreams, _ := project["upstreams"].([]interface{})
	require.Len(t, upstreams, 2)
}

// TestAdmin_AuthenticateFailsClosedWithoutAnAdminAuthRegistry states the safe
// default. An instance with no admin auth configured must reject admin callers
// rather than treat "nothing to check" as "everyone passes".
func TestAdmin_AuthenticateFailsClosedWithoutAnAdminAuthRegistry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t))
	require.Nil(t, e.adminAuthRegistry, "this fixture configures no admin auth")

	user, err := e.AdminAuthenticate(ctx, adminRequest(t, "erpc_config", `[]`), "erpc_config", nil)
	require.Error(t, err)
	require.Nil(t, user)
	require.Contains(t, err.Error(), "admin auth not configured")
}

// apiKeyProject wires a project whose consumer auth reads keys from an
// in-memory database connector, which is what the API-key admin RPCs write to.
func apiKeyProject(t *testing.T) *common.ProjectConfig {
	t.Helper()
	node := evmNode(t)
	return &common.ProjectConfig{
		Id:       "prod",
		Networks: []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}},
		Upstreams: []*common.UpstreamConfig{
			evmUpstream("node-a", node.URL, 123),
		},
		Auth: &common.AuthConfig{
			Strategies: []*common.AuthStrategyConfig{
				{
					Type: common.AuthTypeDatabase,
					Database: &common.DatabaseStrategyConfig{
						Connector: &common.ConnectorConfig{
							Id:     "keys",
							Driver: "memory",
							Memory: &common.MemoryConnectorConfig{MaxItems: 1000, MaxTotalSize: "10MB"},
						},
					},
				},
			},
		},
	}
}

// TestAdmin_AddApiKeyWritesTheRecordTheAuthPathReads is the write half of key
// management. Granting access is a live change to who may call the gateway, so
// the handler answering "success" is not enough — the record has to be in the
// store, with the fields the consumer auth strategy looks for.
func TestAdmin_AddApiKeyWritesTheRecordTheAuthPathReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, apiKeyProject(t))

	connector, err := e.findDatabaseConnectorById("prod", "keys")
	require.NoError(t, err)

	// The record is read back straight from the connector, at the exact
	// (apiKey, userId) coordinates the handler chose to write it to. Reading it
	// back through erpc_listApiKeys instead would let a handler that answers
	// correctly while storing nothing pass.
	//
	// The poll is for the in-memory connector, not for the handler: it is backed
	// by ristretto, which admits a Set asynchronously, so a write is not
	// readable the instant Set returns. The handler's own work is finished
	// before it answers.
	stored := func(apiKey, userId string) map[string]interface{} {
		t.Helper()
		var out map[string]interface{}
		require.Eventually(t, func() bool {
			raw, err := connector.Get(ctx, data.ConnectorMainIndex, apiKey, userId, nil)
			if err != nil {
				return false
			}
			out = nil
			return json.Unmarshal(raw, &out) == nil
		}, 10*time.Second, 5*time.Millisecond,
			"the add reported success but %s/%s never appeared in the store", apiKey, userId)
		return out
	}

	added := adminResult(t, e, "erpc_addApiKey",
		`[{"projectId":"prod","connectorId":"keys","apiKey":"key-1","userId":"alice","rateLimitBudget":"gold"}]`)
	require.Equal(t, true, added["success"])
	require.Equal(t, "key-1", added["apiKey"])
	require.Equal(t, "alice", added["userId"])

	record := stored("key-1", "alice")
	require.Equal(t, "alice", record["userId"])
	require.Equal(t, true, record["enabled"], "a new key is usable unless the operator says otherwise")
	require.Equal(t, "gold", record["rateLimitBudget"])

	adminResult(t, e, "erpc_addApiKey",
		`[{"projectId":"prod","connectorId":"keys","apiKey":"key-2","userId":"bob","enabled":false}]`)
	require.Equal(t, false, stored("key-2", "bob")["enabled"],
		"a key created disabled must be stored disabled, or it works from the moment it exists")
	require.NotContains(t, stored("key-2", "bob"), "rateLimitBudget",
		"an omitted budget must be absent, not stored as an empty string that matches no budget")
}

// TestAdmin_UpdateAndDeleteNeedAStoreThatResolvesTheRangeWildcard states a
// deployment constraint that costs an operator an incident if they meet it
// during one. Both handlers locate the record with Get(rangeKey = "*"), because
// they know the API key but not the user it belongs to. Only a store that
// resolves that wildcard can answer — the in-memory and Redis connectors look
// up the literal key "<apiKey>:*" and miss.
//
// So on an in-memory auth store an operator CAN create a key and CANNOT revoke
// it. The failure is at least loud, which is what this pins: revocation returns
// an error rather than reporting success while leaving the key live.
//
// The same wildcard is what the consumer auth strategy itself reads with
// (auth/strategy_database.go:179), so an in-memory store cannot authenticate
// those keys either. The fix belongs in the connectors, not in admin.go.
func TestAdmin_UpdateAndDeleteNeedAStoreThatResolvesTheRangeWildcard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, apiKeyProject(t))

	adminResult(t, e, "erpc_addApiKey",
		`[{"projectId":"prod","connectorId":"keys","apiKey":"key-1","userId":"alice"}]`)

	_, err := e.AdminHandleRequest(ctx, adminRequest(t, "erpc_updateApiKey",
		`[{"projectId":"prod","connectorId":"keys","apiKey":"key-1","updates":{"enabled":false}}]`))
	require.Error(t, err, "a revocation that cannot read the record must not report success")
	require.Contains(t, err.Error(), "failed to get current API key data")

	_, err = e.AdminHandleRequest(ctx, adminRequest(t, "erpc_deleteApiKey",
		`[{"projectId":"prod","connectorId":"keys","apiKey":"key-1"}]`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to get current API key data")

	// The record is still exactly where the add put it, which is what makes the
	// two failures above a visible gap rather than a lost write.
	connector, err := e.findDatabaseConnectorById("prod", "keys")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		raw, err := connector.Get(ctx, data.ConnectorMainIndex, "key-1", "alice", nil)
		return err == nil && strings.Contains(string(raw), `"alice"`)
	}, 10*time.Second, 5*time.Millisecond,
		"the add itself must have landed, or the two failures above prove nothing")
}

// TestAdmin_ListApiKeysSurfacesAConnectorThatCannotList keeps a storage limit
// from reading as "this project has no keys". The in-memory connector cannot
// iterate, and an operator auditing access needs that stated rather than
// answered with an empty list.
func TestAdmin_ListApiKeysSurfacesAConnectorThatCannotList(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, apiKeyProject(t))
	adminResult(t, e, "erpc_addApiKey",
		`[{"projectId":"prod","connectorId":"keys","apiKey":"key-1","userId":"alice"}]`)

	_, err := e.AdminHandleRequest(ctx, adminRequest(t, "erpc_listApiKeys",
		`[{"projectId":"prod","connectorId":"keys","limit":10,"paginationToken":"next"}]`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to list API keys",
		"a listing that cannot run must say so, not return zero keys")
}

// TestAdmin_ApiKeyCallsRejectMissingOrMistypedArguments covers the argument
// gate on all four handlers. These are the calls an operator scripts against;
// a handler that accepted a missing userId would write an unattributable key.
func TestAdmin_ApiKeyCallsRejectMissingOrMistypedArguments(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, apiKeyProject(t))

	cases := map[string][2]string{
		"AddWithNoParams":        {"erpc_addApiKey", `[]`},
		"AddWithScalarParam":     {"erpc_addApiKey", `["key-1"]`},
		"AddWithoutProjectId":    {"erpc_addApiKey", `[{"connectorId":"keys","apiKey":"k","userId":"u"}]`},
		"AddWithoutConnectorId":  {"erpc_addApiKey", `[{"projectId":"prod","apiKey":"k","userId":"u"}]`},
		"AddWithoutApiKey":       {"erpc_addApiKey", `[{"projectId":"prod","connectorId":"keys","userId":"u"}]`},
		"AddWithoutUserId":       {"erpc_addApiKey", `[{"projectId":"prod","connectorId":"keys","apiKey":"k"}]`},
		"AddWithNumericBudget":   {"erpc_addApiKey", `[{"projectId":"prod","connectorId":"keys","apiKey":"k","userId":"u","rateLimitBudget":5}]`},
		"AddWithNonBooleanFlag":  {"erpc_addApiKey", `[{"projectId":"prod","connectorId":"keys","apiKey":"k","userId":"u","enabled":"yes"}]`},
		"AddToUnknownConnector":  {"erpc_addApiKey", `[{"projectId":"prod","connectorId":"nope","apiKey":"k","userId":"u"}]`},
		"AddToUnknownProject":    {"erpc_addApiKey", `[{"projectId":"staging","connectorId":"keys","apiKey":"k","userId":"u"}]`},
		"ListWithNoParams":       {"erpc_listApiKeys", `[]`},
		"ListWithScalarParam":    {"erpc_listApiKeys", `["prod"]`},
		"ListWithoutProjectId":   {"erpc_listApiKeys", `[{"connectorId":"keys"}]`},
		"ListWithoutConnectorId": {"erpc_listApiKeys", `[{"projectId":"prod"}]`},
		"UpdateWithNoParams":     {"erpc_updateApiKey", `[]`},
		"UpdateWithScalarParam":  {"erpc_updateApiKey", `["key-1"]`},
		"UpdateWithoutProjectId": {"erpc_updateApiKey", `[{"connectorId":"keys","apiKey":"k","updates":{}}]`},
		"UpdateWithoutConnector": {"erpc_updateApiKey", `[{"projectId":"prod","apiKey":"k","updates":{}}]`},
		"UpdateWithoutApiKey":    {"erpc_updateApiKey", `[{"projectId":"prod","connectorId":"keys","updates":{}}]`},
		"UpdateWithoutUpdates":   {"erpc_updateApiKey", `[{"projectId":"prod","connectorId":"keys","apiKey":"k"}]`},
		"DeleteWithNoParams":     {"erpc_deleteApiKey", `[]`},
		"DeleteWithScalarParam":  {"erpc_deleteApiKey", `["key-1"]`},
		"DeleteWithoutProjectId": {"erpc_deleteApiKey", `[{"connectorId":"keys","apiKey":"k"}]`},
		"DeleteWithoutConnector": {"erpc_deleteApiKey", `[{"projectId":"prod","apiKey":"k"}]`},
		"DeleteWithoutApiKey":    {"erpc_deleteApiKey", `[{"projectId":"prod","connectorId":"keys"}]`},
	}

	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.AdminHandleRequest(ctx, adminRequest(t, call[0], call[1]))
			require.Error(t, err, "%s must be rejected rather than silently accepted", name)
		})
	}
}

// TestAdmin_ApiKeyCallsRejectAProjectWithoutAnAuthRegistry covers the lookup
// that stands between the admin RPCs and storage. A project with no consumer
// auth has nowhere to put a key, and saying so beats a nil dereference.
func TestAdmin_ApiKeyCallsRejectAProjectWithoutAnAuthRegistry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t)) // no Auth block

	_, err := e.findDatabaseConnectorById("prod", "keys")
	require.Error(t, err)
	require.Contains(t, err.Error(), "has no auth registry")

	_, err = e.findDatabaseConnectorById("staging", "keys")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}
