package erpc

import (
	"context"
	"math"
	"testing"

	"github.com/erpc/erpc/common"
	upstreampkg "github.com/erpc/erpc/upstream"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// TestEligibleLane covers the block-availability "lane" decision that feeds
// the selection policy's per-boundary axis. The interesting cases are the two
// "no scoping → nil" collapses (no bounds anywhere; every upstream eligible),
// which keep the engine from spawning a lane slot identical to the wildcard.
func TestEligibleLane(t *testing.T) {
	const (
		lo = math.MinInt64
		hi = math.MaxInt64
	)
	// archive: fully unbounded. recent: only blocks >= 100 (lower.exactBlock).
	// capped: only blocks <= 200 (an upper bound). window: [100,200].
	archive := upstreamBlockBounds{id: "archive", min: lo, max: hi}
	recent := upstreamBlockBounds{id: "recent", min: 100, max: hi}
	capped := upstreamBlockBounds{id: "capped", min: lo, max: 200}
	window := upstreamBlockBounds{id: "window", min: 100, max: 200}

	cases := []struct {
		name   string
		bounds []upstreamBlockBounds
		bn     int64
		want   []string // nil means "no lane scoping"
	}{
		{
			name:   "no bounds anywhere → nil",
			bounds: []upstreamBlockBounds{{id: "a", min: lo, max: hi}, {id: "b", min: lo, max: hi}},
			bn:     5,
			want:   nil,
		},
		{
			name:   "historical block excludes recent-only node → proper subset",
			bounds: []upstreamBlockBounds{archive, recent},
			bn:     1,
			want:   []string{"archive"},
		},
		{
			name:   "in-range block: all eligible → nil",
			bounds: []upstreamBlockBounds{archive, recent},
			bn:     100,
			want:   nil,
		},
		{
			name:   "above an upper bound excludes the capped node",
			bounds: []upstreamBlockBounds{archive, capped},
			bn:     250,
			want:   []string{"archive"},
		},
		{
			name:   "windowed node included only inside its window",
			bounds: []upstreamBlockBounds{archive, window},
			bn:     150,
			want:   nil, // both eligible inside the window
		},
		{
			name:   "windowed node excluded below its window",
			bounds: []upstreamBlockBounds{archive, window},
			bn:     50,
			want:   []string{"archive"},
		},
		{
			name:   "no upstream can serve → empty (non-nil) lane",
			bounds: []upstreamBlockBounds{recent},
			bn:     1,
			want:   []string{}, // recent is bounded and excluded → empty, NOT the full-pool nil
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := eligibleLane(tc.bounds, tc.bn)
			if tc.want == nil {
				require.Nil(t, got)
				return
			}
			require.Equal(t, tc.want, got)
		})
	}
}

// eligibleLane decides the lane from bounds a caller already resolved.
// eligibleUpstreamIDsForBoundary is what resolves them, and it refuses to
// answer in five situations. Each refusal returns nil, which the policy engine
// reads as "do not scope this request" — so a refusal that stopped firing would
// not error, it would quietly narrow the pool for requests that must see all of
// it.

// laneNetwork builds a Network with the given upstreams and architecture, using
// the same light fixture the query-executor tests use.
func laneNetwork(t *testing.T, architecture common.NetworkArchitecture, ups ...*upstreampkg.Upstream) *Network {
	t.Helper()
	logger := zerolog.Nop()
	n := &Network{
		networkId:         "evm:1",
		logger:            &logger,
		upstreamsRegistry: newTestUpstreamsRegistry(t, "evm:1", "eth_getBalance", ups...),
	}
	if architecture != "" {
		n.cfg = &common.NetworkConfig{
			Architecture: architecture,
			Evm:          &common.EvmNetworkConfig{ChainId: 1},
		}
	}
	return n
}

// laneUpstream builds an upstream whose availability bounds come straight from
// config, so no state poller is needed to resolve them.
func laneUpstream(t *testing.T, id string, lowerBound *int64) *upstreampkg.Upstream {
	t.Helper()
	logger := zerolog.Nop()
	ups := &upstreampkg.Upstream{}
	evmCfg := &common.EvmUpstreamConfig{ChainId: 1}
	if lowerBound != nil {
		evmCfg.BlockAvailability = &common.EvmBlockAvailabilityConfig{
			Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: lowerBound},
		}
	}
	setUnexportedField(t, ups, "config", &common.UpstreamConfig{
		Id:       id,
		Type:     common.UpstreamTypeEvm,
		Endpoint: "http://lane.example",
		Evm:      evmCfg,
	})
	setUnexportedField(t, ups, "logger", &logger)
	return ups
}

func laneRequest(t *testing.T, body string) *common.NormalizedRequest {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(body))
	require.NoError(t, req.Validate())
	return req
}

// TestEligibleUpstreamIDsForBoundary_ScopesToTheUpstreamsThatHoldTheBlock is
// the case the axis exists for: an archive node and a recent-only node, and a
// block only the archive node still has.
func TestEligibleUpstreamIDsForBoundary_ScopesToTheUpstreamsThatHoldTheBlock(t *testing.T) {
	lower := int64(100)
	n := laneNetwork(t, common.ArchitectureEvm,
		laneUpstream(t, "archive", nil),
		laneUpstream(t, "recent", &lower),
	)

	req := laneRequest(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","0x5"]}`)
	got := n.eligibleUpstreamIDsForBoundary(context.Background(), "eth_getBalance", req)

	require.Equal(t, []string{"archive"}, got,
		"block 5 is below the recent node's lower bound, so it must be out of the lane")
}

// TestEligibleUpstreamIDsForBoundary_RefusesToScope collects the refusals. Each
// returns nil, and nil means the caller uses the whole pool.
func TestEligibleUpstreamIDsForBoundary_RefusesToScope(t *testing.T) {
	lower := int64(100)
	withUpstreams := func() *Network {
		return laneNetwork(t, common.ArchitectureEvm,
			laneUpstream(t, "archive", nil),
			laneUpstream(t, "recent", &lower),
		)
	}
	numbered := `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","0x5"]}`

	t.Run("the network is not EVM", func(t *testing.T) {
		n := laneNetwork(t, common.ArchitectureSvm,
			laneUpstream(t, "archive", nil), laneUpstream(t, "recent", &lower))
		require.Nil(t, n.eligibleUpstreamIDsForBoundary(
			context.Background(), "eth_getBalance", laneRequest(t, numbered)))
	})

	t.Run("the network has no config at all", func(t *testing.T) {
		n := laneNetwork(t, "", laneUpstream(t, "archive", nil), laneUpstream(t, "recent", &lower))
		require.Nil(t, n.eligibleUpstreamIDsForBoundary(
			context.Background(), "eth_getBalance", laneRequest(t, numbered)))
	})

	t.Run("the method owns its own range check", func(t *testing.T) {
		// eth_getLogs spans a range, not a block, and has its own availability
		// hook. Scoping it by a single block number would be wrong.
		req := laneRequest(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x5"}]}`)
		require.Nil(t, withUpstreams().eligibleUpstreamIDsForBoundary(
			context.Background(), "eth_getLogs", req))
	})

	t.Run("the request names no block number", func(t *testing.T) {
		req := laneRequest(t, `{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["0xdead"]}`)
		require.Nil(t, withUpstreams().eligibleUpstreamIDsForBoundary(
			context.Background(), "eth_sendRawTransaction", req))
	})

	t.Run("the network has no upstreams", func(t *testing.T) {
		n := laneNetwork(t, common.ArchitectureEvm)
		require.Nil(t, n.eligibleUpstreamIDsForBoundary(
			context.Background(), "eth_getBalance", laneRequest(t, numbered)))
	})
}

// Bug 72 pin. eRPC marks block-agnostic methods — eth_chainId, net_version —
// as finalized and gives them block number 1, "a signal to indicate data is
// finalized on first ever block" (architecture/evm/block_ref.go). That 1 is a
// CACHE sentinel, not a block the caller asked for.
//
// The availability gate used to read it as a real block. 1 is below any
// configured lower bound, so every static method was refused on every pruned
// node, and the boundary lane came back empty. An operator saw eth_chainId
// fail across the whole pool while eth_getBalance on the same nodes succeeded.
// The bound arrives the same way from maxAvailableRecentBlocks, which resolves
// to latest-N, so this was not confined to explicit bounds.
//
// Both gates now ask evm.MethodHasNoBlockDependency — the same config that
// produced the sentinel — instead of decoding the number. This test pins that:
// a static method is served, and a real historical read is still refused.
func TestBlockAvailability_StaticMethodIsNotGatedByTheCacheSentinel(t *testing.T) {
	lower := int64(100)
	n := laneNetwork(t, common.ArchitectureEvm, laneUpstream(t, "recent", &lower))
	n.projectId = "boundary-test"
	ups := n.upstreamsRegistry.GetNetworkUpstreams(context.Background(), "evm:1")
	require.Len(t, ups, 1)

	for _, method := range []string{"eth_chainId", "net_version"} {
		t.Run(method, func(t *testing.T) {
			req := laneRequest(t, `{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":[]}`)

			err, retryable := n.checkUpstreamBlockAvailability(context.Background(), ups[0], req, method)
			require.NoError(t, err,
				"bug 72: a chain-id probe needs no block, so no upstream can be missing one")
			require.False(t, retryable)

			require.Nil(t, n.eligibleUpstreamIDsForBoundary(context.Background(), method, req),
				"bug 72: no boundary applies to a block-agnostic method, so every upstream is eligible")
		})
	}

	// The contrast that keeps the fix honest. A tip-bound read is allowed for a
	// different reason (it resolves to block 0), and a real historical read below
	// the bound is still refused — the fix must not have disabled the gate.
	req := laneRequest(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","latest"]}`)
	err, _ := n.checkUpstreamBlockAvailability(context.Background(), ups[0], req, "eth_getBalance")
	require.NoError(t, err, "a tip-bound read on the same upstream is allowed through")

	req = laneRequest(t, `{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaaaa","0x5"]}`)
	err, retryable := n.checkUpstreamBlockAvailability(context.Background(), ups[0], req, "eth_getBalance")
	require.Error(t, err, "block 5 is below the lower bound of 100 and must still be refused")
	require.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamBlockUnavailable))
	require.False(t, retryable)
}
