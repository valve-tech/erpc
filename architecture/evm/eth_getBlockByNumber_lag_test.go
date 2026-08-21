package evm

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// networkPostForward_eth_getBlockByNumber publishes how far behind the wall
// clock the block an upstream just served is. Operators alarm on that gauge, so
// it has to carry the real distance and it has to stay silent for the requests
// it cannot attribute — a block fetched by number says nothing about the tip.

// latestBlockResponse builds a well-formed eth_getBlockByNumber result for the
// given block number and timestamp.
func latestBlockResponse(t *testing.T, blockNumber int64, timestamp int64) *common.NormalizedResponse {
	t.Helper()
	body := fmt.Sprintf(`{"number":"0x%x","hash":"0xaaa","timestamp":"0x%x"}`, blockNumber, timestamp)
	return common.NewNormalizedResponse().WithJsonRpcResponse(
		common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(body), nil),
	)
}

// lagNetwork reports `project` as its project id and `head` as the highest
// latest block it knows about.
func lagNetwork(project string, head int64) *mockNetwork {
	n := new(mockNetwork)
	n.On("ProjectId").Return(project).Maybe()
	n.On("Id").Return("evm:1").Maybe()
	n.On("EvmHighestLatestBlockNumber", mock.Anything).Return(head).Maybe()
	return n
}

func TestNetworkPostForward_GetBlockByNumber_PublishesTheLatestBlockAge(t *testing.T) {
	ctx := context.Background()
	const project = "gbn-lag-latest"

	// Detailed tracing adds the block-number and timestamp span attributes.
	// Turn it on so the whole lag block runs, and put it back afterwards.
	prev := common.IsTracingDetailed
	common.IsTracingDetailed = true
	defer func() { common.IsTracingDetailed = prev }()

	blockTs := time.Now().Add(-90 * time.Second).Unix()
	n := lagNetwork(project, 110)
	nq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
	nr := latestBlockResponse(t, 100, blockTs)

	out, err := networkPostForward_eth_getBlockByNumber(ctx, n, nq, nr, nil)
	require.NoError(t, err)
	assert.Same(t, nr, out, "the hook observes the response, it does not replace it")

	got := promUtil.ToFloat64(telemetry.MetricNetworkLatestBlockTimestampDistance.
		WithLabelValues(project, "evm:1", "network_response"))
	assert.InDelta(t, 90, got, 5,
		"the gauge must carry the real distance between the block and the wall clock")
}

func TestNetworkPostForward_GetBlockByNumber_StaysSilentOffTheTip(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		project string
		params  string
		resp    func(t *testing.T) *common.NormalizedResponse
	}{
		{
			// A block fetched by number is not the tip, so its age says nothing
			// about how far behind the network is.
			name:    "ExplicitBlockNumber",
			project: "gbn-lag-bynumber",
			params:  `["0x64",false]`,
			resp: func(t *testing.T) *common.NormalizedResponse {
				// Clearly old, so a hook that measured it anyway would move the
				// gauge well away from zero.
				return latestBlockResponse(t, 100, time.Now().Add(-90*time.Second).Unix())
			},
		},
		{
			name:    "NoTimestampInTheBlock",
			project: "gbn-lag-nots",
			params:  `["latest",false]`,
			resp: func(t *testing.T) *common.NormalizedResponse {
				return common.NewNormalizedResponse().WithJsonRpcResponse(
					common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`{"number":"0x64","hash":"0xaaa"}`), nil),
				)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := lagNetwork(tc.project, 110)
			nq := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":` + tc.params + `}`))

			_, err := networkPostForward_eth_getBlockByNumber(ctx, n, nq, tc.resp(t), nil)
			require.NoError(t, err)

			got := promUtil.ToFloat64(telemetry.MetricNetworkLatestBlockTimestampDistance.
				WithLabelValues(tc.project, "evm:1", "network_response"))
			assert.Equal(t, float64(0), got,
				"a request that cannot be attributed to the tip must leave the gauge untouched")
		})
	}
}

// A forward that already failed carries no block to measure. The hook must hand
// the upstream error straight back rather than reporting a distance of "now".
func TestNetworkPostForward_GetBlockByNumber_PassesTheUpstreamErrorThrough(t *testing.T) {
	ctx := context.Background()
	const project = "gbn-lag-error"

	n := lagNetwork(project, 110)
	nq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`))
	upstreamErr := fmt.Errorf("upstream refused")

	_, err := networkPostForward_eth_getBlockByNumber(ctx, n, nq, latestBlockResponse(t, 100, time.Now().Unix()), upstreamErr)
	require.ErrorIs(t, err, upstreamErr)

	got := promUtil.ToFloat64(telemetry.MetricNetworkLatestBlockTimestampDistance.
		WithLabelValues(project, "evm:1", "network_response"))
	assert.Equal(t, float64(0), got, "a failed forward must not publish a block age")
}
