package erpc

import (
	"fmt"
	"io"
	"testing"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eRPC answers range queries on two surfaces. The JSON-RPC surface runs the
// shim in architecture/evm and reads `upstreams[].evm.queryShim` — enabled,
// allowedMethods, maxBlockRange, maxLimit, defaultLimit, concurrency. The BDS
// gRPC surface runs its own shim in this package and reads none of them.
//
// The tests below characterise that second surface. They do not endorse it: an
// operator who caps a range to bound what one call costs at the provider has
// capped only half of their traffic, and nothing in the config or the logs says
// so.

// queryShimLimits returns a shim config with the tightest bounds an operator
// can express, so a walk that respects any of them is impossible to miss.
func queryShimLimits(enabled bool) *common.EvmQueryShimConfig {
	return &common.EvmQueryShimConfig{
		Enabled:       util.BoolPtr(enabled),
		Concurrency:   1,
		MaxBlockRange: 2,
		MaxLimit:      1,
		DefaultLimit:  1,
	}
}

// collectQueryBlockNumbers drains a QueryBlocks stream into the block numbers
// it delivered.
func collectQueryBlockNumbers(t *testing.T, stream interface {
	Recv() (*evm.QueryBlocksResponse, error)
}) []uint64 {
	t.Helper()
	var numbers []uint64
	for {
		page, err := stream.Recv()
		if err == io.EOF {
			return numbers
		}
		require.NoError(t, err)
		for _, b := range page.Blocks {
			numbers = append(numbers, b.Number)
		}
	}
}

// TestGrpcQueryBlocks_WalksAWiderRangeThanTheUpstreamsQueryShimAllows is the
// cost defect. maxBlockRange is 2 and maxLimit is 1, yet a 17-block request is
// walked in full: 17 separate eth_getBlockByNumber calls to the upstream, all
// billed, from one client call the operator believed was capped at two blocks.
func TestGrpcQueryBlocks_WalksAWiderRangeThanTheUpstreamsQueryShimAllows(t *testing.T) {
	h := newGrpcHarness(t, func(c *common.Config) {
		c.Projects[0].Upstreams[0].Evm.QueryShim = queryShimLimits(true)
	})
	h.node.replyBlockByNumber()

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	from, to := "0x10", "0x20"
	stream, err := h.query.QueryBlocks(ctx, &evm.QueryBlocksRequest{FromBlock: &from, ToBlock: &to})
	require.NoError(t, err)

	numbers := collectQueryBlockNumbers(t, stream)

	want := make([]uint64, 0, 17)
	for n := uint64(0x10); n <= 0x20; n++ {
		want = append(want, n)
	}
	assert.Equal(t, want, numbers,
		"defect: maxBlockRange=2 and maxLimit=1 bound the JSON-RPC shim only, so "+
			"the gRPC surface walks the whole range")

	// The blocks really were fetched one by one, so the cost is real rather
	// than a page assembled from something already held.
	asked := map[string]bool{}
	for _, p := range h.node.firstParams("eth_getBlockByNumber") {
		asked[p] = true
	}
	for n := 0x10; n <= 0x20; n++ {
		require.True(t, asked[hexBlockRef(uint64(n))],
			"block %#x must have cost its own upstream call", n)
	}
}

// TestGrpcQueryBlocks_ServesTheQueryEvenWhenTheShimIsTurnedOff is the same gap
// seen from the switch an operator would reach for first. `enabled: false`
// takes eth_queryBlocks off the JSON-RPC surface; over gRPC the identical query
// is still decomposed onto the upstream.
func TestGrpcQueryBlocks_ServesTheQueryEvenWhenTheShimIsTurnedOff(t *testing.T) {
	h := newGrpcHarness(t, func(c *common.Config) {
		c.Projects[0].Upstreams[0].Evm.QueryShim = queryShimLimits(false)
	})
	h.node.replyBlockByNumber()

	ctx, cancel := h.callCtx(nil)
	defer cancel()
	from, to := "0x10", "0x12"
	stream, err := h.query.QueryBlocks(ctx, &evm.QueryBlocksRequest{FromBlock: &from, ToBlock: &to})
	require.NoError(t, err)

	numbers := collectQueryBlockNumbers(t, stream)
	assert.Equal(t, []uint64{0x10, 0x11, 0x12}, numbers,
		"defect: the disabled shim still serves the gRPC query")

	asked := map[string]bool{}
	for _, p := range h.node.firstParams("eth_getBlockByNumber") {
		asked[p] = true
	}
	require.True(t, asked[hexBlockRef(0x11)],
		"the upstream really was asked for the range, so the switch bought nothing")
}

// hexBlockRef renders a block number the way the shim puts it on the wire.
func hexBlockRef(n uint64) string {
	return fmt.Sprintf("0x%x", n)
}
