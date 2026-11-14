package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/internal/policy"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockCountedMethod answers method on host with body and counts the calls.
// It matches only bodies that hold marker, so the state poller's own
// availability probes (which can use the same methods) are not counted. It
// must be registered before the fixture's catch-all mocks. gock runs a
// filter before it matches the host, so the filter checks the host itself.
func mockCountedMethod(host, method, marker, body string) *atomic.Int32 {
	var calls atomic.Int32
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			if r.URL.Host != host {
				return false
			}
			b := util.SafeReadBody(r)
			if strings.Contains(b, `"`+method+`"`) && strings.Contains(b, marker) {
				calls.Add(1)
				return true
			}
			return false
		}).
		Reply(200).
		JSON([]byte(body))
	return &calls
}

type emptyFinalCase struct {
	method string
	// params is the JSON params array of the request. It must hold marker.
	params string
	// marker is a string only this test's request body holds.
	marker string
	// primaryBody is the JSON-RPC body each primary returns.
	primaryBody string
	// fallbackBody is the JSON-RPC body each fallback returns.
	fallbackBody string
	// authoritative lists the method on the primaries when true.
	authoritative bool
	primaryLatest string
	// primary2Latest and primary2Body override primary-2's head and answer
	// when set. primary-1 is always asked first.
	primary2Latest string
	primary2Body   string
}

type emptyFinalResult struct {
	resp          *common.NormalizedResponse
	err           error
	primary1Calls int32
	primaryCalls  int32
	fallbackCalls int32
	escapes       float64
}

// runEmptyFinalCase builds two primaries and two tier:fallback upstreams.
// The fallbacks are at block 1002. The primaries have no availability gate,
// so only the head check decides whether their empty answer is final. The
// network's emptyResultAccept is empty, so the accept list cannot mask the
// authoritative setting, and RetryEmpty is on, so a miss can retry.
func runEmptyFinalCase(t *testing.T, c emptyFinalCase) emptyFinalResult {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(util.ResetGock)

	var p1, p2, f1, f2 *atomic.Int32
	network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
		primaryLatest:  c.primaryLatest,
		fallbackLatest: "0x3ea", // 1002
		enableFailover: true,
		failsafe: []*common.FailsafeConfig{{
			MatchMethod: "*",
			Retry: &common.RetryPolicyConfig{
				MaxAttempts:       3,
				Delay:             common.Duration(5 * time.Millisecond),
				EmptyResultDelay:  common.Duration(5 * time.Millisecond),
				EmptyResultAccept: []string{},
			},
		}},
		mocks: func() {
			if c.primary2Latest != "" {
				mockJsonRpcUpstream("rpc2.localhost", "0x3e7", c.primary2Latest, "0x3e0")
			}
			p2Body := c.primaryBody
			if c.primary2Body != "" {
				p2Body = c.primary2Body
			}
			p1 = mockCountedMethod("rpc1.localhost", c.method, c.marker, c.primaryBody)
			p2 = mockCountedMethod("rpc2.localhost", c.method, c.marker, p2Body)
			f1 = mockCountedMethod("rpc3.localhost", c.method, c.marker, c.fallbackBody)
			f2 = mockCountedMethod("rpc4.localhost", c.method, c.marker, c.fallbackBody)
		},
		configure: func(cfgs []*common.UpstreamConfig) {
			for _, cfg := range cfgs {
				if strings.HasPrefix(cfg.Id, "fallback") {
					// The selection policy's prober mirrors sampled
					// requests to cordoned upstreams. Those mirrors are not
					// the request path, so turn them off to keep the
					// fallback call counts clean.
					cfg.Routing = &common.UpstreamRoutingConfig{Probe: common.ProbeModeOff}
				}
				if strings.HasPrefix(cfg.Id, "primary") {
					cfg.Evm.BlockAvailability = nil
					if c.authoritative {
						cfg.Evm.EmptyResultAuthoritativeMethods = []string{c.method}
					}
				}
			}
		},
		network: func(cfg *common.NetworkConfig) {
			cfg.DirectiveDefaults = &common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)}
			// One voter is enough for the served tip, so primary-2 alone at
			// 1002 makes the network confident about block 1002.
			cfg.Evm.ServedTip = &common.EvmServedTipConfig{Quorum: 1}
		},
	})

	// Pin the routed order in the policy only. The registry keeps all four
	// upstreams, so the escape can still find the fallbacks.
	policy.OverrideOrderForTest(network.policyEngine, network.networkId, "primary-1", "primary-2")

	counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", c.method)
	before := promUtil.ToFloat64(counter)

	req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"%s","params":%s}`, c.method, c.params,
	)))
	req.SetNetwork(network)
	req.ApplyDirectiveDefaults(network.cfg.DirectiveDefaults)
	resp, err := network.Forward(ctx, req)
	if resp != nil {
		t.Cleanup(resp.Release)
	}
	return emptyFinalResult{
		resp:          resp,
		err:           err,
		primary1Calls: p1.Load(),
		primaryCalls:  p1.Load() + p2.Load(),
		fallbackCalls: f1.Load() + f2.Load(),
		escapes:       promUtil.ToFloat64(counter) - before,
	}
}

func resultString(t *testing.T, resp *common.NormalizedResponse) string {
	t.Helper()
	require.NotNil(t, resp)
	jrr, err := resp.JsonRpcResponse()
	require.NoError(t, err)
	return jrr.GetResultString()
}

const (
	logAddress     = "00000000000000000000000000000000000000aa"
	balanceAddress = "00000000000000000000000000000000000000bb"
	logsAt1001     = `[{"fromBlock":"0x3e9","toBlock":"0x3e9","address":"0x00000000000000000000000000000000000000aa"}]`
	logsAt1002     = `[{"fromBlock":"0x3ea","toBlock":"0x3ea","address":"0x00000000000000000000000000000000000000aa"}]`
	emptyLogs      = `{"jsonrpc":"2.0","id":1,"result":[]}`
	oneLog         = `{"jsonrpc":"2.0","id":1,"result":[{"address":"0x00000000000000000000000000000000000000aa","blockNumber":"0x3ea","logIndex":"0x0","transactionHash":"0x01"}]}`
	balanceAt1001  = `["0x00000000000000000000000000000000000000bb","0x3e9"]`
	zeroBalance    = `{"jsonrpc":"2.0","id":1,"result":"0x0"}`
	someBalance    = `{"jsonrpc":"2.0","id":1,"result":"0x64"}`
)

// An empty answer from an upstream that lists the method, for a block at or
// below that upstream's head, is final: no retry and no fallback escape.
func TestEmptyFinal_AuthoritativeAtOrBelowHeadIsFinal(t *testing.T) {
	t.Run("EthGetLogsEmptyArray", func(t *testing.T) {
		r := runEmptyFinalCase(t, emptyFinalCase{
			method: "eth_getLogs", params: logsAt1001, marker: logAddress,
			primaryBody: emptyLogs, fallbackBody: oneLog,
			authoritative: true, primaryLatest: "0x3ea", // 1002 >= 1001
		})
		require.NoError(t, r.err)
		assert.Equal(t, "[]", resultString(t, r.resp))
		assert.Equal(t, int32(1), r.primary1Calls, "primary-1's answer is final: no retry")
		assert.Equal(t, int32(1), r.primaryCalls, "and no sweep to primary-2")
		assert.Equal(t, int32(0), r.fallbackCalls, "the paid fallback must never be called")
		assert.Equal(t, float64(0), r.escapes, "the escape counter must not move")
	})

	t.Run("EthGetBalanceZero", func(t *testing.T) {
		r := runEmptyFinalCase(t, emptyFinalCase{
			method: "eth_getBalance", params: balanceAt1001, marker: balanceAddress,
			primaryBody: zeroBalance, fallbackBody: someBalance,
			authoritative: true, primaryLatest: "0x3ea",
		})
		require.NoError(t, r.err)
		assert.Equal(t, `"0x0"`, resultString(t, r.resp))
		assert.Equal(t, int32(1), r.primary1Calls)
		assert.Equal(t, int32(1), r.primaryCalls)
		assert.Equal(t, int32(0), r.fallbackCalls)
		assert.Equal(t, float64(0), r.escapes)
	})
}

// The tip case. primary-1 lists the method, but its head (1000) is below
// the requested block (1002). primary-2 is at 1002, so the network is
// confident about the block. primary-1's empty answer may only mean it has
// not imported the block yet, so it is not final.
func TestEmptyFinal_AuthoritativeAboveHeadEscalates(t *testing.T) {
	// The loop moves on to primary-2, which has the data.
	t.Run("EthGetLogsNextPrimaryAnswers", func(t *testing.T) {
		r := runEmptyFinalCase(t, emptyFinalCase{
			method: "eth_getLogs", params: logsAt1002, marker: logAddress,
			primaryBody: emptyLogs, fallbackBody: oneLog,
			authoritative: true, primaryLatest: "0x3e8", // 1000 < 1002
			primary2Latest: "0x3ea", primary2Body: oneLog,
		})
		require.NoError(t, r.err)
		assert.Contains(t, resultString(t, r.resp), `"logIndex"`)
		assert.Equal(t, "primary-2", r.resp.UpstreamId())
		assert.GreaterOrEqual(t, r.primary1Calls, int32(1), "primary-1 must be asked first")
		assert.Equal(t, int32(0), r.fallbackCalls)
	})

	// primary-2 fails, so the request escapes to the fallback tier.
	t.Run("EthGetLogsEscapesToFallback", func(t *testing.T) {
		r := runEmptyFinalCase(t, emptyFinalCase{
			method: "eth_getLogs", params: logsAt1002, marker: logAddress,
			primaryBody: emptyLogs, fallbackBody: oneLog,
			authoritative: true, primaryLatest: "0x3e8",
			primary2Latest: "0x3ea",
			primary2Body:   `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"unavailable"}}`,
		})
		require.NoError(t, r.err)
		assert.Contains(t, resultString(t, r.resp), `"logIndex"`)
		assert.Contains(t, []string{"fallback-1", "fallback-2"}, r.resp.UpstreamId())
		assert.GreaterOrEqual(t, r.primary1Calls, int32(1))
		assert.Equal(t, float64(1), r.escapes)
	})

	t.Run("EthGetBalanceNextPrimaryAnswers", func(t *testing.T) {
		r := runEmptyFinalCase(t, emptyFinalCase{
			method: "eth_getBalance", params: `["0x00000000000000000000000000000000000000bb","0x3ea"]`, marker: balanceAddress,
			primaryBody: zeroBalance, fallbackBody: someBalance,
			authoritative: true, primaryLatest: "0x3e8",
			primary2Latest: "0x3ea", primary2Body: someBalance,
		})
		require.NoError(t, r.err)
		assert.Equal(t, `"0x64"`, resultString(t, r.resp))
		assert.Equal(t, "primary-2", r.resp.UpstreamId())
		assert.GreaterOrEqual(t, r.primary1Calls, int32(1))
	})
}

// null is never authoritative: a node says null when it lacks the block.
func TestEmptyFinal_NullStillEscalates(t *testing.T) {
	r := runEmptyFinalCase(t, emptyFinalCase{
		method: "eth_getBlockByNumber", params: `["0x3e9",false]`, marker: `"0x3e9"`,
		primaryBody:   `{"jsonrpc":"2.0","id":1,"result":null}`,
		fallbackBody:  `{"jsonrpc":"2.0","id":1,"result":{"number":"0x3e9","hash":"0xabc","parentHash":"0xdef","timestamp":"0x6702a8f0"}}`,
		authoritative: true, primaryLatest: "0x3ea",
	})
	require.NoError(t, r.err)
	assert.Contains(t, []string{"fallback-1", "fallback-2"}, r.resp.UpstreamId())
	assert.Equal(t, float64(1), r.escapes)
}

// The control: a primary that does not list the method still escalates.
func TestEmptyFinal_NotListedStillEscalates(t *testing.T) {
	r := runEmptyFinalCase(t, emptyFinalCase{
		method: "eth_getLogs", params: logsAt1001, marker: logAddress,
		primaryBody: emptyLogs, fallbackBody: oneLog,
		authoritative: false, primaryLatest: "0x3ea",
	})
	require.NoError(t, r.err)
	assert.Contains(t, resultString(t, r.resp), `"logIndex"`)
	assert.Equal(t, float64(1), r.escapes)
}

// An error from a primary that lists the method is not an empty answer, so
// it still escalates.
func TestEmptyFinal_ErrorStillEscalates(t *testing.T) {
	r := runEmptyFinalCase(t, emptyFinalCase{
		method: "eth_getBalance", params: balanceAt1001, marker: balanceAddress,
		primaryBody:   `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"header not found"}}`,
		fallbackBody:  someBalance,
		authoritative: true, primaryLatest: "0x3ea",
	})
	require.NoError(t, r.err)
	assert.Equal(t, `"0x64"`, resultString(t, r.resp))
	assert.Equal(t, float64(1), r.escapes)
}
