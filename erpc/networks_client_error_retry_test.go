package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A generic -32602 from every upstream is each node's verdict on the request
// bytes. The first sweep already asked every upstream, so a timed second
// sweep asks the same nodes the same question and only adds the retry delay.
// reth answers eth_callBundle with no transactions this way.
func TestNetwork_ClientErrorFromEveryUpstreamDoesNotResweep(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()

	var calls atomic.Int32
	var upstreamConfigs []*common.UpstreamConfig
	for _, id := range []string{"rpc1", "rpc2"} {
		endpoint := "http://" + id + ".localhost"
		upstreamConfigs = append(upstreamConfigs, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       id,
			Endpoint: endpoint,
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		})
		gock.New(endpoint).
			Post("").
			Persist().
			Filter(func(r *http.Request) bool {
				return strings.Contains(util.SafeReadBody(r), "eth_callBundle")
			}).
			ReplyFunc(func(res *gock.Response) {
				calls.Add(1)
				res.Status(200).JSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      1,
					"error":   map[string]interface{}{"code": -32602, "message": "bundle missing txs"},
				})
			})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupSendRawTxNetwork(t, ctx, upstreamConfigs, &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm:          &common.EvmNetworkConfig{ChainId: 123},
		Failsafe: []*common.FailsafeConfig{{
			MatchMethod: "*",
			Retry: &common.RetryPolicyConfig{
				MaxAttempts: 3,
				Delay:       common.Duration(500 * time.Millisecond),
			},
		}},
	})

	start := time.Now()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_callBundle","params":[{"txs":[],"blockNumber":"0x1","stateBlockNumber":"latest"}]}`))
	_, err := network.Forward(ctx, req)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
		"client must get the node's -32602, got: %v", err)
	assert.Equal(t, int32(2), calls.Load(), "each upstream must be asked once")
	// One retry delay is 500 ms; the answer must not wait for it.
	assert.Less(t, elapsed, 400*time.Millisecond,
		"the error must return once every upstream has answered, not after a retry delay")
}
