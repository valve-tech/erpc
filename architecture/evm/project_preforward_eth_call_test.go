package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Some upstreams reject an eth_call that carries no block parameter, so eRPC
// appends "latest" when the caller sent only the call object. The rewrite lands
// in the cache key and in the bytes every upstream receives, so it needs a test
// that reads the params the upstream receives. The project hook that did this
// (eth_call.go) is gone since #1335; NormalizeHttpJsonRpc does it now, and
// HandleProjectPreForward must leave every eth_call shape alone.

// forwardedParams captures the params of the request the network received.
func forwardedParams(t *testing.T, nq *common.NormalizedRequest) []interface{} {
	t.Helper()
	jrq, err := nq.JsonRpcRequest()
	require.NoError(t, err)
	jrq.RLock()
	defer jrq.RUnlock()
	out := make([]interface{}, len(jrq.Params))
	copy(out, jrq.Params)
	return out
}

func TestNormalizeHttpJsonRpc_EthCall_AppendsLatestWhenTheBlockIsMissing(t *testing.T) {
	// Since #1335 the normalizer, not a project hook, spells out an absent
	// selector. The fork's defaults keep "latest" for eth_call, so the word
	// itself is appended rather than the head's number.
	net := &tagTestNetwork{highestLatest: 0x1234}
	nq, jrq := newTagRequest(t, net, "eth_call", map[string]interface{}{"to": "0xdead", "data": "0x01"})

	NormalizeHttpJsonRpc(normalizeCtx(t), nq, jrq)

	seen := forwardedParams(t, nq)
	require.Len(t, seen, 2, "the upstream must receive the call object plus a block parameter")
	assert.Equal(t, "latest", seen[1], "the appended block parameter is the latest tag")
	callObj, ok := seen[0].(map[string]interface{})
	require.True(t, ok, "the original call object must survive the rewrite")
	assert.Equal(t, "0xdead", callObj["to"])
}

func TestProjectPreForward_EthCall_LeavesAnExplicitBlockAlone(t *testing.T) {
	ctx := context.Background()

	n := new(mockNetwork)
	n.On("Id").Return("evm:1").Maybe()
	n.On("ProjectId").Return("call-explicit-block").Maybe()
	// No Forward expectation: the hook must not forward this request itself.

	nq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xdead"},"0x64"]}`))

	handled, resp, err := HandleProjectPreForward(ctx, n, nq)
	require.NoError(t, err)
	assert.False(t, handled, "a caller-supplied block must reach the upstream untouched")
	assert.Nil(t, resp)

	params := forwardedParams(t, nq)
	require.Len(t, params, 2)
	assert.Equal(t, "0x64", params[1], "the hook must not overwrite the requested block")
}

func TestProjectPreForward_EthCall_PassesThroughMalformedParams(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		params string
	}{
		{"NoParams", `[]`},
		{"ThreeParams", `[{"to":"0xdead"},"0x64",{"0xdead":{"balance":"0x1"}}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := new(mockNetwork)
			n.On("Id").Return("evm:1").Maybe()
			n.On("ProjectId").Return("call-passthrough").Maybe()

			nq := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":` + tc.params + `}`))

			handled, resp, err := HandleProjectPreForward(ctx, n, nq)
			require.NoError(t, err)
			assert.False(t, handled, "the hook only rewrites the single-param shape it understands")
			assert.Nil(t, resp)
		})
	}
}
