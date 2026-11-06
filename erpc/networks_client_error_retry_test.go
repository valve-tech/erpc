package erpc

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minusThirtyTwoSixOhTwoNetwork builds two upstreams on the fleet's network
// retry (3 attempts, 500 ms delay). Each upstream answers method once with
// first, then with then on every later call (nil: no later answer). It
// returns the first-answer mocks, so a test can count how many were used.
// gock's ReplyFunc runs once at setup, not per request, so one-shot mocks in
// order are the way to change an answer over time.
func minusThirtyTwoSixOhTwoNetwork(t *testing.T, ctx context.Context, method string, first, then map[string]interface{}) (*Network, []gock.Mock) {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)
	util.SetupMocksForEvmStatePoller()

	isMethod := func(r *http.Request) bool { return strings.Contains(util.SafeReadBody(r), method) }
	var firsts []gock.Mock
	var upstreamConfigs []*common.UpstreamConfig
	for _, id := range []string{"rpc1", "rpc2"} {
		endpoint := "http://" + id + ".localhost"
		upstreamConfigs = append(upstreamConfigs, &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       id,
			Endpoint: endpoint,
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		})
		req := gock.New(endpoint).Post("").Times(1).Filter(isMethod)
		req.Reply(200).JSON(first)
		firsts = append(firsts, req.Mock)
		if then != nil {
			gock.New(endpoint).Post("").Persist().Filter(isMethod).Reply(200).JSON(then)
		}
	}

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
	return network, firsts
}

func used(mocks []gock.Mock) int {
	n := 0
	for _, m := range mocks {
		if m.Done() {
			n++
		}
	}
	return n
}

func rpcError(code int, msg string) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"error":   map[string]interface{}{"code": code, "message": msg},
	}
}

// reth answers eth_callBundle with no transactions with -32602 "bundle
// missing txs". No node accepts a bundle with no transactions, now or later,
// so the first answer is final: no other upstream, no retry delay.
func TestNetwork_EmptyBundleRejectionReturnsAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network, firsts := minusThirtyTwoSixOhTwoNetwork(t, ctx, "eth_callBundle",
		rpcError(-32602, "bundle missing txs"), rpcError(-32602, "bundle missing txs"))

	start := time.Now()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_callBundle","params":[{"txs":[],"blockNumber":"0x1","stateBlockNumber":"latest"}]}`))
	_, err := network.Forward(ctx, req)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeEndpointClientSideException),
		"client must get the node's -32602, got: %v", err)
	assert.Equal(t, 1, used(firsts), "a permanent rejection needs no second opinion")
	assert.Less(t, elapsed, 400*time.Millisecond,
		"the error must return at once, not after a retry delay")
}

// reth answers eth_feeHistory for the block one past its head with -32602
// "request beyond head block". The block arrives a moment later, so this
// -32602 clears with time: the network retry must keep running and return the
// answer. 0.3.0.7-valve failed 8.5% of these on chain 1 by treating every
// -32602 as final (bug log 198).
func TestNetwork_MinusThirtyTwoSixOhTwoThatClearsWithTimeIsRetried(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Both upstreams are one block behind for their first call.
	network, firsts := minusThirtyTwoSixOhTwoNetwork(t, ctx, "eth_feeHistory",
		rpcError(-32602, "request beyond head block"),
		map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]interface{}{
				"oldestBlock":   "0x10",
				"baseFeePerGas": []interface{}{"0x1", "0x1"},
				"gasUsedRatio":  []interface{}{0.5},
			},
		})

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_feeHistory","params":["0x1","0x10",[]]}`))
	resp, err := network.Forward(ctx, req)

	require.NoError(t, err, "the retry must reach the answer once the block exists")
	require.NotNil(t, resp)
	assert.GreaterOrEqual(t, used(firsts), 1, "the first answer was the -32602, so a retry must have run")
}
