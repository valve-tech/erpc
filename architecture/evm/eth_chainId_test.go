package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The three eth_chainId hooks answer from configuration instead of forwarding.
// Each one must decide two things: whether it may answer at all, and which
// chainId it answers with. Both decisions are observable, so every test below
// asserts the ANSWER (or its absence) rather than just the handled flag — a
// hook that answers the wrong chain still reports handled=true.

func chainIdRequest(t *testing.T, skipCacheRead bool) *common.NormalizedRequest {
	t.Helper()
	jrq, err := BuildEthChainIdRequest()
	require.NoError(t, err)
	r := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	if skipCacheRead {
		r.SetDirectives(&common.RequestDirectives{SkipCacheRead: "true"})
	}
	return r
}

// answeredChainId reads the hex chainId out of a hook's response.
func answeredChainId(t *testing.T, resp *common.NormalizedResponse) string {
	t.Helper()
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	require.Nil(t, jrr.Error)
	var out string
	require.NoError(t, common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &out))
	return out
}

func evmNetwork(chainId int64) *testNetwork {
	return &testNetwork{cfg: &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: chainId},
	}}
}

func TestBuildEthChainIdRequest(t *testing.T) {
	jrq, err := BuildEthChainIdRequest()
	require.NoError(t, err)
	assert.Equal(t, "eth_chainId", jrq.Method)
	assert.Empty(t, jrq.Params)
	// Discriminating: an unset id makes the response un-correlatable, and two
	// concurrent probes would collide on it.
	assert.NotNil(t, jrq.ID)
	assert.NotEqual(t, 0, jrq.ID)
}

func TestProjectPreForward_ethChainId(t *testing.T) {
	t.Run("AnswersFromNetworkConfig", func(t *testing.T) {
		r := chainIdRequest(t, false)

		handled, resp, err := projectPreForward_eth_chainId(context.Background(), evmNetwork(137), r)

		require.NoError(t, err)
		require.True(t, handled)
		assert.Equal(t, "0x89", answeredChainId(t, resp), "137 must render as 0x89")
	})

	t.Run("SkipCacheReadFallsThroughToTheUpstream", func(t *testing.T) {
		// An operator asking for a fresh read wants the node's own answer, not
		// the configured one — that is how a cross-wired node is caught.
		r := chainIdRequest(t, true)

		handled, resp, err := projectPreForward_eth_chainId(context.Background(), evmNetwork(137), r)

		require.NoError(t, err)
		assert.False(t, handled)
		assert.Nil(t, resp)
	})

	t.Run("UnconfiguredChainIdFallsThrough", func(t *testing.T) {
		for name, n := range map[string]*testNetwork{
			"zeroChainId": evmNetwork(0),
			"noEvmBlock":  {cfg: &common.NetworkConfig{Architecture: common.ArchitectureEvm}},
			"noConfig":    {},
		} {
			t.Run(name, func(t *testing.T) {
				handled, resp, err := projectPreForward_eth_chainId(
					context.Background(), n, chainIdRequest(t, false))
				require.NoError(t, err)
				assert.False(t, handled)
				assert.Nil(t, resp)
			})
		}
	})

	t.Run("ResponseCarriesTheRequestId", func(t *testing.T) {
		// The batch splitter matches replies to calls by id. A hook-built
		// response with the wrong id is delivered to the wrong caller.
		jrq := common.NewJsonRpcRequest("eth_chainId", []interface{}{})
		require.NoError(t, jrq.SetID(4242))
		r := common.NewNormalizedRequestFromJsonRpcRequest(jrq)

		_, resp, err := projectPreForward_eth_chainId(context.Background(), evmNetwork(1), r)
		require.NoError(t, err)
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.EqualValues(t, 4242, jrr.ID())
	})
}

func TestNetworkPreForward_ethChainId(t *testing.T) {
	t.Run("AnswersFromNetworkConfig", func(t *testing.T) {
		handled, resp, err := networkPreForward_eth_chainId(
			context.Background(), evmNetwork(42161), nil, chainIdRequest(t, false))

		require.NoError(t, err)
		require.True(t, handled)
		assert.Equal(t, "0xa4b1", answeredChainId(t, resp))
	})

	t.Run("NilRequestOrNetworkFallsThrough", func(t *testing.T) {
		handled, _, err := networkPreForward_eth_chainId(context.Background(), evmNetwork(1), nil, nil)
		require.NoError(t, err)
		assert.False(t, handled)

		handled, _, err = networkPreForward_eth_chainId(context.Background(), nil, nil, chainIdRequest(t, false))
		require.NoError(t, err)
		assert.False(t, handled)
	})

	t.Run("SkipCacheReadFallsThroughToTheUpstream", func(t *testing.T) {
		handled, resp, err := networkPreForward_eth_chainId(
			context.Background(), evmNetwork(42161), nil, chainIdRequest(t, true))
		require.NoError(t, err)
		assert.False(t, handled)
		assert.Nil(t, resp)
	})

	t.Run("UnconfiguredChainIdFallsThrough", func(t *testing.T) {
		handled, resp, err := networkPreForward_eth_chainId(
			context.Background(), evmNetwork(0), nil, chainIdRequest(t, false))
		require.NoError(t, err)
		assert.False(t, handled)
		assert.Nil(t, resp)
	})
}

func TestUpstreamPreForward_ethChainId(t *testing.T) {
	t.Run("PrefersTheUpstreamConfigOverEverythingElse", func(t *testing.T) {
		// The upstream's own detected chainId wins. A network fanning out to
		// several chains must not have one member answer with the other's id.
		up := newForwardingUpstream(10) // config says 10 (Optimism)
		up.networkId = "evm:999"        // stale label
		handled, resp, err := upstreamPreForward_eth_chainId(
			context.Background(), evmNetwork(1), up, chainIdRequest(t, false))

		require.NoError(t, err)
		require.True(t, handled)
		assert.Equal(t, "0xa", answeredChainId(t, resp))
	})

	t.Run("FallsBackToTheNetworkIdLabel", func(t *testing.T) {
		up := newForwardingUpstream(0) // config carries no chainId
		up.networkId = "evm:8453"
		handled, resp, err := upstreamPreForward_eth_chainId(
			context.Background(), evmNetwork(1), up, chainIdRequest(t, false))

		require.NoError(t, err)
		require.True(t, handled)
		assert.Equal(t, "0x2105", answeredChainId(t, resp),
			"the evm:<id> label must be preferred over the network config")
	})

	t.Run("FallsBackToTheNetworkConfigLast", func(t *testing.T) {
		up := newForwardingUpstream(0)
		up.networkId = "svm:mainnet" // not an evm: label, so unusable
		handled, resp, err := upstreamPreForward_eth_chainId(
			context.Background(), evmNetwork(56), up, chainIdRequest(t, false))

		require.NoError(t, err)
		require.True(t, handled)
		assert.Equal(t, "0x38", answeredChainId(t, resp))
	})

	t.Run("UnparsableNetworkIdFallsBackToTheNetworkConfig", func(t *testing.T) {
		// "evm:" with a non-numeric suffix must not be forced into a chainId.
		up := newForwardingUpstream(0)
		up.networkId = "evm:mainnet"
		handled, resp, err := upstreamPreForward_eth_chainId(
			context.Background(), evmNetwork(56), up, chainIdRequest(t, false))

		require.NoError(t, err)
		require.True(t, handled)
		assert.Equal(t, "0x38", answeredChainId(t, resp))
	})

	t.Run("NoChainIdAnywhereFallsThrough", func(t *testing.T) {
		up := newForwardingUpstream(0)
		up.networkId = "evm:"
		handled, resp, err := upstreamPreForward_eth_chainId(
			context.Background(), evmNetwork(0), up, chainIdRequest(t, false))

		require.NoError(t, err)
		assert.False(t, handled, "with nothing configured the request must reach the node")
		assert.Nil(t, resp)
	})

	t.Run("SkipCacheReadFallsThroughToTheUpstream", func(t *testing.T) {
		up := newForwardingUpstream(10)
		handled, resp, err := upstreamPreForward_eth_chainId(
			context.Background(), evmNetwork(1), up, chainIdRequest(t, true))
		require.NoError(t, err)
		assert.False(t, handled)
		assert.Nil(t, resp)
	})

	t.Run("NilArgumentsFallThrough", func(t *testing.T) {
		up := newForwardingUpstream(10)
		for _, tc := range []struct {
			name string
			n    common.Network
			u    common.Upstream
			r    *common.NormalizedRequest
		}{
			{"nilNetwork", nil, up, chainIdRequest(t, false)},
			{"nilUpstream", evmNetwork(1), nil, chainIdRequest(t, false)},
			{"nilRequest", evmNetwork(1), up, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				handled, resp, err := upstreamPreForward_eth_chainId(context.Background(), tc.n, tc.u, tc.r)
				require.NoError(t, err)
				assert.False(t, handled)
				assert.Nil(t, resp)
			})
		}
	})
}
