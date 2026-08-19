package erpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// AdminAuthenticate is the gate in front of every write in admin.go: cordon a
// node, revoke a key, read the whole config. The fail-closed default is already
// covered; what was not is the configured case, where a wrong secret has to be
// refused as firmly as a missing one.

// adminAuthErpc builds an eRPC whose admin API is protected by one shared
// secret, the way an operator configures it.
func adminAuthErpc(t *testing.T, ctx context.Context, secret string) *ERPC {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	node := evmNode(t)
	cfg := &common.Config{
		Projects: []*common.ProjectConfig{{
			Id:        "prod",
			Networks:  []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}},
			Upstreams: []*common.UpstreamConfig{evmUpstream("node-a", node.URL, 123)},
		}},
		RateLimiters: &common.RateLimiterConfig{},
		Admin: &common.AdminConfig{
			Auth: &common.AuthConfig{
				Strategies: []*common.AuthStrategyConfig{
					{Type: common.AuthTypeSecret, Secret: &common.SecretStrategyConfig{Value: secret}},
				},
			},
		},
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
	return instance
}

// secretPayload builds the auth payload the HTTP layer hands to
// AdminAuthenticate when a caller presents a token.
func secretPayload(t *testing.T, token string) *auth.AuthPayload {
	t.Helper()
	ap, err := auth.NewPayloadFromHttp("erpc_config", "127.0.0.1:1234",
		http.Header{"X-Erpc-Secret-Token": []string{token}}, url.Values{})
	require.NoError(t, err)
	return ap
}

// TestAdmin_AuthenticateAcceptsTheConfiguredSecretAndRefusesAnother pins both
// sides of the admin gate. Accepting only is not enough — a gate that accepts
// everything would pass that half.
func TestAdmin_AuthenticateAcceptsTheConfiguredSecretAndRefusesAnother(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := adminAuthErpc(t, ctx, "correct-horse")
	require.NotNil(t, e.adminAuthRegistry, "this fixture configures admin auth")

	req := adminRequest(t, "erpc_config", `[]`)

	user, err := e.AdminAuthenticate(ctx, req, "erpc_config", secretPayload(t, "correct-horse"))
	require.NoError(t, err, "the configured secret must be accepted")
	require.NotNil(t, user)

	_, err = e.AdminAuthenticate(ctx, req, "erpc_config", secretPayload(t, "battery-staple"))
	require.Error(t, err, "a wrong secret must be refused")
	require.True(t, common.HasErrorCode(err, common.ErrCodeAuthUnauthorized),
		"a wrong secret must be refused as unauthorized, got %v", err)
}

// TestAdmin_ListApiKeysSkipsOneCorruptRecord pins that a single unreadable row
// does not hide the rest of the key list. An operator auditing who has access
// must see every key the store still holds, not an empty answer.
func TestAdmin_ListApiKeysSkipsOneCorruptRecord(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, apiKeyProjectOnRedis(t))

	strategy, err := e.findDatabaseStrategyById("prod", "keys")
	require.NoError(t, err)
	connector := strategy.GetConnector()

	adminResult(t, e, "erpc_addApiKey",
		`[{"projectId":"prod","connectorId":"keys","apiKey":"key-good","userId":"alice"}]`)
	storedApiKey(t, ctx, connector, "key-good")

	// A row whose body is not JSON — the shape a partial write or a hand-edited
	// store leaves behind.
	require.NoError(t, connector.Set(ctx, "key-corrupt", data.ConnectorApiKeyRangeKey, []byte("{not json"), nil))

	var listed map[string]interface{}
	require.Eventually(t, func() bool {
		listed = adminResult(t, e, "erpc_listApiKeys",
			`[{"projectId":"prod","connectorId":"keys"}]`)
		keys, _ := listed["apiKeys"].([]interface{})
		return len(keys) >= 1
	}, 10*time.Second, 10*time.Millisecond, "the good key never appeared in the listing")

	keys, _ := listed["apiKeys"].([]interface{})
	ids := []string{}
	for _, raw := range keys {
		k, _ := raw.(map[string]interface{})
		ids = append(ids, k["key"].(string))
	}
	require.Contains(t, ids, "key-good",
		"a corrupt neighbour must not hide a readable key from the audit")
	require.NotContains(t, ids, "key-corrupt",
		"an unreadable record must be skipped, not reported as a working key")
}

// TestAdmin_UpdateRemovesAFieldWhenTheOperatorSendsNull covers how an operator
// takes a rate-limit budget off a key. Storing a null, or keeping the old
// budget, would leave the key limited by something the operator believes they
// removed.
func TestAdmin_UpdateRemovesAFieldWhenTheOperatorSendsNull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, apiKeyProject(t))

	strategy, err := e.findDatabaseStrategyById("prod", "keys")
	require.NoError(t, err)
	connector := strategy.GetConnector()

	adminResult(t, e, "erpc_addApiKey",
		`[{"projectId":"prod","connectorId":"keys","apiKey":"key-1","userId":"alice","rateLimitBudget":"gold"}]`)
	require.Equal(t, "gold", storedApiKey(t, ctx, connector, "key-1")["rateLimitBudget"])

	adminResult(t, e, "erpc_updateApiKey",
		`[{"projectId":"prod","connectorId":"keys","apiKey":"key-1","updates":{"rateLimitBudget":null}}]`)

	var record map[string]interface{}
	require.Eventually(t, func() bool {
		record = storedApiKey(t, ctx, connector, "key-1")
		_, still := record["rateLimitBudget"]
		return !still
	}, 10*time.Second, 5*time.Millisecond,
		"a null update must delete the field, not store a null the auth path reads back")

	require.Equal(t, "alice", record["userId"],
		"removing one field must not drop the user the key belongs to")
}

// TestAdmin_UpdateRefusesARecordWithNoUser pins the refusal that keeps a broken
// record from being written back. A key with no user authenticates nobody, so
// answering "success" would tell the operator a grant landed when it cannot.
func TestAdmin_UpdateRefusesARecordWithNoUser(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, apiKeyProject(t))

	strategy, err := e.findDatabaseStrategyById("prod", "keys")
	require.NoError(t, err)
	connector := strategy.GetConnector()

	orphan, err := json.Marshal(map[string]interface{}{"enabled": true})
	require.NoError(t, err)
	require.NoError(t, connector.Set(ctx, "key-orphan", data.ConnectorApiKeyRangeKey, orphan, nil))

	require.Eventually(t, func() bool {
		_, err := connector.Get(ctx, data.ConnectorMainIndex, "key-orphan", data.ConnectorApiKeyRangeKey, nil)
		return err == nil
	}, 10*time.Second, 5*time.Millisecond, "the fixture record never landed in the store")

	_, err = e.AdminHandleRequest(ctx, adminRequest(t, "erpc_updateApiKey",
		`[{"projectId":"prod","connectorId":"keys","apiKey":"key-orphan","updates":{"enabled":false}}]`))
	require.Error(t, err, "a record with no user must not be written back")
	require.Contains(t, err.Error(), "missing or invalid userId in current data")
}
