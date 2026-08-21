package evm

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eth_getLogs must always hand the caller a JSON array. Nodes disagree about
// how to say "no logs": some send [], some send null, some send an empty
// string. A client that does `for (const log of result)` crashes on anything
// but the array, so the upstream post-forward hook rewrites the emptyish
// shapes into [].

func getLogsRequest(t *testing.T) *common.NormalizedRequest {
	t.Helper()
	return common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":9,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`))
}

func getLogsResponse(t *testing.T, rq *common.NormalizedRequest, raw string) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponseFromBytes([]byte(`9`), []byte(raw), nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithRequest(rq).WithJsonRpcResponse(jrr)
}

func TestUpstreamPostForward_ethGetLogs(t *testing.T) {
	ctx := context.Background()

	t.Run("NullResultBecomesAnEmptyArray", func(t *testing.T) {
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)
		rq := getLogsRequest(t)
		rs := getLogsResponse(t, rq, `null`)

		got, err := upstreamPostForward_eth_getLogs(ctx, n, up, rq, rs, nil)

		require.NoError(t, err)
		require.NotNil(t, got)
		jrr, err := got.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, "[]", jrr.GetResultString(),
			"a caller iterating the result must not receive null")
		// Discriminating: the hook must build a NEW response, and stamp the
		// answering upstream on it, so routing and metrics still attribute it.
		assert.NotSame(t, rs, got)
		assert.Equal(t, "fwd-ups", got.UpstreamId())
	})

	t.Run("EmptyStringResultBecomesAnEmptyArray", func(t *testing.T) {
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)
		rq := getLogsRequest(t)
		rs := getLogsResponse(t, rq, `""`)

		got, err := upstreamPostForward_eth_getLogs(ctx, n, up, rq, rs, nil)

		require.NoError(t, err)
		jrr, err := got.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, "[]", jrr.GetResultString())
	})

	t.Run("PopulatedResultIsLeftAlone", func(t *testing.T) {
		// Rewriting a real result would silently drop every log.
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)
		rq := getLogsRequest(t)
		rs := getLogsResponse(t, rq, `[{"address":"0xdead","data":"0x1"}]`)

		got, err := upstreamPostForward_eth_getLogs(ctx, n, up, rq, rs, nil)

		require.NoError(t, err)
		assert.Same(t, rs, got)
		jrr, err := got.JsonRpcResponse()
		require.NoError(t, err)
		assert.Contains(t, jrr.GetResultString(), "0xdead")
	})

	t.Run("AnErrorIsPassedThroughWithoutRewriting", func(t *testing.T) {
		// A failed request has no result to normalize. Turning it into [] would
		// report "no logs" for a range that was never read.
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)
		rq := getLogsRequest(t)
		rs := getLogsResponse(t, rq, `null`)
		boom := errors.New("upstream 503")

		got, err := upstreamPostForward_eth_getLogs(ctx, n, up, rq, rs, boom)

		assert.Same(t, boom, err)
		assert.Same(t, rs, got)
	})

	t.Run("NilResponseIsPassedThrough", func(t *testing.T) {
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)

		got, err := upstreamPostForward_eth_getLogs(ctx, n, up, getLogsRequest(t), nil, nil)

		require.NoError(t, err)
		assert.Nil(t, got)
	})

	t.Run("TheDispatcherRoutesGetLogsToThisHook", func(t *testing.T) {
		// The normalization only reaches production through the method switch
		// in HandleUpstreamPostForward, so pin the routing as well as the hook.
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)
		rq := getLogsRequest(t)
		rs := getLogsResponse(t, rq, `null`)

		got, err := HandleUpstreamPostForward(ctx, n, up, rq, rs, nil, false)

		require.NoError(t, err)
		require.NotNil(t, got)
		jrr, err := got.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, "[]", jrr.GetResultString())
	})

	t.Run("TheArchitectureHandlerDelegatesToTheDispatcher", func(t *testing.T) {
		// The registry hands erpc an EvmArchitectureHandler, not the package
		// functions, so the wrapper is the real production entry point.
		h := &EvmArchitectureHandler{}
		up := newForwardingUpstream(1)
		n := newForwardingNetwork(1)
		rq := getLogsRequest(t)
		rs := getLogsResponse(t, rq, `null`)

		got, err := h.HandleUpstreamPostForward(ctx, n, up, rq, rs, nil, false)

		require.NoError(t, err)
		jrr, err := got.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, "[]", jrr.GetResultString())
	})
}
