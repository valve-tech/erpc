package erpc

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A transaction sequencer (Base, Arbitrum) accepts eth_sendRawTransaction and
// answers every other method, eth_chainId included, with -32601. With
// evm.chainId configured, such an upstream must start and serve the
// transaction. Every state-poller method also gets -32601 here, so the test
// proves that nothing else at init blocks the upstream.
func TestNetwork_SequencerUpstream_ServesSendRawTransaction(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	var sendCalls atomic.Int32
	gock.New("http://sequencer.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_sendRawTransaction")
		}).
		Reply(200).
		Map(func(res *http.Response) *http.Response {
			sendCalls.Add(1)
			return res
		}).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"result":  expectedTxHash,
		})
	gock.New("http://sequencer.localhost").
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return !strings.Contains(util.SafeReadBody(r), "eth_sendRawTransaction")
		}).
		Reply(200).
		JSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      1,
			"error": map[string]interface{}{
				"code":    -32601,
				"message": "rpc method is not whitelisted",
			},
		})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network := setupSendRawTxNetwork(t, ctx,
		[]*common.UpstreamConfig{{
			Type:          common.UpstreamTypeEvm,
			Id:            "sequencer",
			Endpoint:      "http://sequencer.localhost",
			Evm:           &common.EvmUpstreamConfig{ChainId: 123},
			IgnoreMethods: []string{"*"},
			AllowMethods:  []string{"eth_sendRawTransaction", "eth_chainId"},
		}},
		&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 123},
		},
	)

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_sendRawTransaction","params":["` + sampleSignedTx + `"]}`))
	resp, err := network.Forward(ctx, req)
	require.NoError(t, err, "the sequencer must serve eth_sendRawTransaction")
	require.NotNil(t, resp)
	defer resp.Release()

	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	assert.Equal(t, `"`+expectedTxHash+`"`, jrr.GetResultString())
	assert.Equal(t, int32(1), sendCalls.Load(), "the transaction reached the sequencer once")
}
