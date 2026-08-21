package erpc

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// Endpoint URLs carry API keys. erpc_config and erpc_project both hand the
// operator a project's upstream list, so both are places a key could leave the
// process. The tests below assert the redaction placeholder is PRESENT, not
// merely that the key is absent: an empty field would pass an absence check
// while telling the operator nothing.

// The literal an operator would put in a vendor URL. It must never appear in an
// admin response.
const adminSecretKey = "SUPER-SECRET-KEY-9d41"

// secretEndpointProject points a real upstream at a live fake node through a URL
// that carries a key in its path. The node ignores the path, so the upstream
// bootstraps normally and the config still holds a secret.
func secretEndpointProject(t *testing.T) *common.ProjectConfig {
	t.Helper()
	node := evmNode(t)
	return &common.ProjectConfig{
		Id:       "prod",
		Networks: []*common.NetworkConfig{{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}}},
		Upstreams: []*common.UpstreamConfig{
			evmUpstream("node-a", node.URL+"/v1/"+adminSecretKey, 123),
		},
	}
}

// requireRedactedEndpoints walks every upstream in an admin response and pins
// both halves of the contract: the placeholder is there, and the key is not.
func requireRedactedEndpoints(t *testing.T, surface string, upstreams []interface{}) {
	t.Helper()
	require.NotEmpty(t, upstreams, "%s returned no upstreams, so it proves nothing about redaction", surface)
	for _, raw := range upstreams {
		u, ok := raw.(map[string]interface{})
		require.True(t, ok, "%s upstream entry must be an object", surface)
		endpoint, ok := u["endpoint"].(string)
		require.True(t, ok, "%s must still report an endpoint field for upstream %v", surface, u["id"])
		require.Contains(t, endpoint, "#redacted=",
			"%s must show the redaction placeholder, not an empty or raw endpoint", surface)
		require.NotContains(t, endpoint, adminSecretKey,
			"%s leaked the API key in the endpoint", surface)
	}
}

// TestAdmin_ConfigRedactsUpstreamEndpoints covers the whole-config read. It is
// the widest admin surface: one call returns every upstream in every project.
func TestAdmin_ConfigRedactsUpstreamEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, secretEndpointProject(t))

	resp, err := e.AdminHandleRequest(ctx, adminRequest(t, "erpc_config", `[]`))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	body := jrr.GetResultString()

	require.NotContains(t, body, adminSecretKey, "erpc_config leaked the API key somewhere in its body")

	var out map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	projects, _ := out["projects"].([]interface{})
	require.Len(t, projects, 1)
	project, _ := projects[0].(map[string]interface{})
	upstreams, _ := project["upstreams"].([]interface{})
	requireRedactedEndpoints(t, "erpc_config", upstreams)
}

// TestAdmin_ProjectRedactsUpstreamEndpoints covers the per-project read, which
// returns the same upstream list through a different handler.
func TestAdmin_ProjectRedactsUpstreamEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, secretEndpointProject(t))

	resp, err := e.AdminHandleRequest(ctx, adminRequest(t, "erpc_project", `["prod"]`))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	body := jrr.GetResultString()

	require.NotContains(t, body, adminSecretKey, "erpc_project leaked the API key somewhere in its body")

	var out map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	cfg, _ := out["config"].(map[string]interface{})
	upstreams, _ := cfg["upstreams"].([]interface{})
	requireRedactedEndpoints(t, "erpc_project", upstreams)
}

// TestAdmin_ListCordonedRejectsAProjectWithoutAnUpstreamRegistry pins the
// remaining argument checks on the cordon read. An operator scripting a
// reconcile loop must get an error, not an empty list that reads as "nothing is
// cordoned".
func TestAdmin_ListCordonedRejectsAMissingOrUnknownProject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, twoNodeProject(t))

	_, err := e.AdminHandleRequest(ctx, adminRequest(t, "erpc_listCordoned", `[]`))
	require.Error(t, err, "no params must be an error, not an empty cordon list")
	require.Contains(t, err.Error(), "projectId is required")

	_, err = e.AdminHandleRequest(ctx, adminRequest(t, "erpc_listCordoned", `[{}]`))
	require.Error(t, err, "an object without projectId must be an error")
	require.Contains(t, err.Error(), "projectId is required")

	_, err = e.AdminHandleRequest(ctx, adminRequest(t, "erpc_listCordoned", `[{"projectId":"staging"}]`))
	require.Error(t, err, "an unknown project must be an error, not an empty cordon list")
}

// TestAdmin_TaxonomyIsNotAWayToReadEndpoints states the shape of the taxonomy
// answer. It names projects, networks and upstreams by id and vendor only, so a
// caller with taxonomy access never sees a URL.
func TestAdmin_TaxonomyIsNotAWayToReadEndpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	e := startAdminErpc(t, ctx, secretEndpointProject(t))

	resp, err := e.AdminHandleRequest(ctx, adminRequest(t, "erpc_taxonomy", `[]`))
	require.NoError(t, err)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	body := jrr.GetResultString()

	require.NotContains(t, body, adminSecretKey, "erpc_taxonomy leaked the API key")
	require.False(t, strings.Contains(body, "endpoint"),
		"taxonomy must carry no endpoint field at all, redacted or otherwise")
	require.Contains(t, body, `"id":"node-a"`, "taxonomy must still name the upstream")
}
