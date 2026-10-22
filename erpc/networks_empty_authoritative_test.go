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
	"github.com/stretchr/testify/require"
)

// mockTraceBlock answers trace_block on host with result, or with a
// JSON-RPC error when errMsg is set, and counts the calls.
func mockTraceBlock(host string, result interface{}, errMsg string) *atomic.Int32 {
	var calls atomic.Int32
	body := map[string]interface{}{"jsonrpc": "2.0", "id": 1}
	if errMsg != "" {
		body["error"] = map[string]interface{}{"code": -32000, "message": errMsg}
	} else {
		body["result"] = result
	}
	gock.New(host).
		Post("").
		Filter(func(r *http.Request) bool {
			if strings.Contains(util.SafeReadBody(r), "trace_block") {
				calls.Add(1)
				return true
			}
			return false
		}).
		Persist().
		Reply(200).
		JSON(body)
	return &calls
}

func setupEmptyAuthoritativeNetwork(t *testing.T, ctx context.Context, authoritative ...string) *Network {
	t.Helper()
	ups := make([]*common.UpstreamConfig, 0, 2)
	for _, id := range []string{"rpc1", "rpc2"} {
		cfg := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       id,
			Endpoint: "http://" + id + ".localhost",
			Evm:      &common.EvmUpstreamConfig{ChainId: 123},
		}
		for _, a := range authoritative {
			if a == id {
				cfg.Evm.EmptyResultAuthoritativeMethods = []string{"trace_block"}
			}
		}
		ups = append(ups, cfg)
	}
	network := setupTestNetworkWithCustomUpstreams(t, ctx, ups,
		&common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)},
		&common.RetryPolicyConfig{
			MaxAttempts:      3,
			Delay:            common.Duration(10 * time.Millisecond),
			EmptyResultDelay: common.Duration(300 * time.Millisecond),
		},
	)
	network.cfg.Evm.MarkEmptyAsErrorMethods = []string{"trace_block"}
	network.PinUpstreamOrderForTest("rpc1", "rpc2")
	return network
}

func forwardTraceBlock(t *testing.T, ctx context.Context, network *Network) (*common.NormalizedResponse, time.Duration, error) {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"trace_block","params":["0x1"]}`))
	req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)
	start := time.Now()
	resp, err := network.Forward(ctx, req)
	return resp, time.Since(start), err
}

func resultOf(t *testing.T, ctx context.Context, resp *common.NormalizedResponse) string {
	t.Helper()
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	return jrr.GetResultString()
}

// Our own node's [] is the truth: it returns at once, with no empty-result
// delay and no second upstream.
func TestEmptyResultAuthoritative_EmptyReturnsAtOnce(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	rpc1 := mockTraceBlock("http://rpc1.localhost", []interface{}{}, "")
	rpc2 := mockTraceBlock("http://rpc2.localhost", []interface{}{map[string]interface{}{"type": "call"}}, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupEmptyAuthoritativeNetwork(t, ctx, "rpc1")

	resp, elapsed, err := forwardTraceBlock(t, ctx, network)
	require.NoError(t, err)
	require.Equal(t, "[]", resultOf(t, ctx, resp))
	require.Less(t, elapsed, 250*time.Millisecond, "no empty-result delay")
	require.Equal(t, int32(1), rpc1.Load())
	require.Equal(t, int32(0), rpc2.Load(), "an authoritative empty must not try another upstream")
}

// A non-authoritative upstream's [] is still missing data: the request
// retries and gets the real answer elsewhere.
func TestEmptyResultAuthoritative_OtherUpstreamStillRetries(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	rpc1 := mockTraceBlock("http://rpc1.localhost", []interface{}{}, "")
	rpc2 := mockTraceBlock("http://rpc2.localhost", []interface{}{map[string]interface{}{"type": "call"}}, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupEmptyAuthoritativeNetwork(t, ctx, "rpc2") // rpc1, asked first, is the fallback

	resp, _, err := forwardTraceBlock(t, ctx, network)
	require.NoError(t, err)
	require.Contains(t, resultOf(t, ctx, resp), `"call"`)
	require.GreaterOrEqual(t, rpc1.Load(), int32(1))
	require.GreaterOrEqual(t, rpc2.Load(), int32(1))
}

// An authoritative upstream that does not have the block says so with an
// error. That error is missing data, not an empty result, so it still
// retries on another upstream.
func TestEmptyResultAuthoritative_BlockUnavailableStillRetries(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	rpc1 := mockTraceBlock("http://rpc1.localhost", nil, "header not found")
	rpc2 := mockTraceBlock("http://rpc2.localhost", []interface{}{map[string]interface{}{"type": "call"}}, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupEmptyAuthoritativeNetwork(t, ctx, "rpc1", "rpc2")

	resp, _, err := forwardTraceBlock(t, ctx, network)
	require.NoError(t, err)
	require.Contains(t, resultOf(t, ctx, resp), `"call"`)
	require.GreaterOrEqual(t, rpc1.Load(), int32(1))
	require.GreaterOrEqual(t, rpc2.Load(), int32(1))
}

// reth answers trace_block with null for a block it has not got yet. A
// null is not an authoritative empty: it fails over and returns the traces.
func TestEmptyResultAuthoritative_NullStillFailsOver(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	rpc1 := mockTraceBlock("http://rpc1.localhost", nil, "")
	rpc2 := mockTraceBlock("http://rpc2.localhost", []interface{}{map[string]interface{}{"type": "call"}}, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	network := setupEmptyAuthoritativeNetwork(t, ctx, "rpc1", "rpc2")

	resp, _, err := forwardTraceBlock(t, ctx, network)
	require.NoError(t, err)
	require.Contains(t, resultOf(t, ctx, resp), `"call"`)
	require.GreaterOrEqual(t, rpc1.Load(), int32(1))
	require.GreaterOrEqual(t, rpc2.Load(), int32(1))
}

func TestEvmUpstreamConfigCopy_EmptyResultAuthoritativeMethods(t *testing.T) {
	orig := &common.EvmUpstreamConfig{EmptyResultAuthoritativeMethods: []string{"trace_block"}}
	cp := orig.Copy()
	cp.EmptyResultAuthoritativeMethods[0] = "changed"
	require.Equal(t, "trace_block", orig.EmptyResultAuthoritativeMethods[0], "Copy must not share the slice")
}
