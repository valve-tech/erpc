package evm

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// projectPreForward_eth_call rewrites the request before anything else sees it.
// Some upstreams reject an eth_call that carries no block parameter, so the hook
// appends "latest" when the caller sent only the call object. The rewrite lands
// in the cache key and in the bytes every upstream receives, so it needs a test
// that reads the params the network actually forwarded.

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

func TestProjectPreForward_EthCall_AppendsLatestWhenTheBlockIsMissing(t *testing.T) {
	ctx := context.Background()

	var seen []interface{}
	n := new(mockNetwork)
	n.On("Id").Return("evm:1").Maybe()
	n.On("ProjectId").Return("call-appends-latest").Maybe()
	n.On("Forward", mock.Anything, mock.Anything).Return(
		func(ctx context.Context, r *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			seen = forwardedParams(t, r)
			return common.NewNormalizedResponse().WithJsonRpcResponse(
				common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0x2a"`), nil),
			), nil
		}, nil,
	).Once()

	nq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xdead","data":"0x01"}]}`))

	handled, resp, err := HandleProjectPreForward(ctx, n, nq)
	require.NoError(t, err)
	assert.True(t, handled, "the hook forwards the request itself, so it owns the answer")
	require.NotNil(t, resp)

	require.Len(t, seen, 2, "the upstream must receive the call object plus a block parameter")
	assert.Equal(t, "latest", seen[1], "the appended block parameter is the latest tag")
	callObj, ok := seen[0].(map[string]interface{})
	require.True(t, ok, "the original call object must survive the rewrite")
	assert.Equal(t, "0xdead", callObj["to"])
	n.AssertExpectations(t)
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

func TestProjectPreForward_EthCall_ReturnsTheNetworkError(t *testing.T) {
	ctx := context.Background()

	n := new(mockNetwork)
	n.On("Id").Return("evm:1").Maybe()
	n.On("ProjectId").Return("call-error").Maybe()
	n.On("Forward", mock.Anything, mock.Anything).Return(nil, errors.New("upstream refused")).Once()

	nq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xdead"}]}`))

	handled, resp, err := HandleProjectPreForward(ctx, n, nq)
	assert.True(t, handled, "the hook still owns the request when the forward fails")
	assert.Nil(t, resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upstream refused",
		"the caller must see the real failure, not a silent miss")
}
