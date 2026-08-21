package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eRPC builds internal eth_chainId probes that carry no JSON-RPC id. The three
// chainId hooks answer those probes from config, and a response with a null id
// cannot be matched back to the request that asked for it. So each hook mints an
// id when the request has none.

// idlessChainIdRequest builds an eth_chainId request with no JSON-RPC id, the
// shape an internal probe has before it reaches the wire.
func idlessChainIdRequest() *common.NormalizedRequest {
	return common.NewNormalizedRequestFromJsonRpcRequest(
		common.NewJsonRpcRequest("eth_chainId", []interface{}{}))
}

func TestPreForward_ethChainId_MintsAnIdForAnIdlessRequest(t *testing.T) {
	ctx := context.Background()

	t.Run("Project", func(t *testing.T) {
		r := idlessChainIdRequest()
		require.Nil(t, r.ID(), "the fixture must really start without an id")

		handled, resp, err := projectPreForward_eth_chainId(ctx, evmNetwork(1), r)
		require.NoError(t, err)
		require.True(t, handled)
		assertAnsweredWithAnId(t, resp, "0x1")
	})

	t.Run("Network", func(t *testing.T) {
		r := idlessChainIdRequest()
		handled, resp, err := networkPreForward_eth_chainId(ctx, evmNetwork(1), nil, r)
		require.NoError(t, err)
		require.True(t, handled)
		assertAnsweredWithAnId(t, resp, "0x1")
	})

	t.Run("Upstream", func(t *testing.T) {
		r := idlessChainIdRequest()
		up := newForwardingUpstream(10)
		handled, resp, err := upstreamPreForward_eth_chainId(ctx, evmNetwork(1), up, r)
		require.NoError(t, err)
		require.True(t, handled)
		assertAnsweredWithAnId(t, resp, "0xa")
	})
}

// assertAnsweredWithAnId checks both halves of the contract: the right chainId,
// and an id the caller can correlate.
func assertAnsweredWithAnId(t *testing.T, resp *common.NormalizedResponse, wantChainId string) {
	t.Helper()
	assert.Equal(t, wantChainId, answeredChainId(t, resp))
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.NotNil(t, jrr.ID(), "an idless probe must still get a correlatable answer")
}
