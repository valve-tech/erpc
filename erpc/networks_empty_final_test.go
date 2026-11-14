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
	return mockCountedMethodDelayed(host, method, marker, body, 0)
}

// mockCountedMethodDelayed is mockCountedMethod with a reply delay.
func mockCountedMethodDelayed(host, method, marker, body string, delay time.Duration) *atomic.Int32 {
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
		Delay(delay).
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
	// defaultAccept keeps eRPC's default emptyResultAccept list instead of
	// emptying it.
	defaultAccept bool
	// primary1Mocks registers mocks for rpc1 ahead of every other mock, for
	// example to control its head.
	primary1Mocks func()
	// primary1Configure adjusts primary-1's config.
	primary1Configure func(cfg *common.UpstreamConfig)
	// headCheckMethods sets evm.emptyResultHeadCheckMethods when non-nil.
	headCheckMethods []string
	// beforeForward runs after the network is ready, just before the request.
	beforeForward func()
	// hedge adds a hedge policy to the network's failsafe.
	hedge *common.HedgePolicyConfig
	// primaryDelay and primary2Delay delay primary-1's and primary-2's
	// answers to the method.
	primaryDelay, primary2Delay time.Duration
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
	// Cleanups run last-in first-out: cancel the network (and so its
	// pollers) first, then reset gock. Resetting gock while a poller is
	// still inside a mocked call is a data race in gock.
	t.Cleanup(util.ResetGock)
	t.Cleanup(cancel)

	var p1, p2, f1, f2 *atomic.Int32
	acceptList := []string{}
	if c.defaultAccept {
		acceptList = nil
	}
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
				EmptyResultAccept: acceptList,
			},
			Hedge: c.hedge,
		}},
		mocks: func() {
			if c.primary1Mocks != nil {
				c.primary1Mocks()
			}
			if c.primary2Latest != "" {
				mockJsonRpcUpstream("rpc2.localhost", "0x3e7", c.primary2Latest, "0x3e0")
			}
			p2Body := c.primaryBody
			if c.primary2Body != "" {
				p2Body = c.primary2Body
			}
			p1 = mockCountedMethodDelayed("rpc1.localhost", c.method, c.marker, c.primaryBody, c.primaryDelay)
			p2 = mockCountedMethodDelayed("rpc2.localhost", c.method, c.marker, p2Body, c.primary2Delay)
			f1 = mockCountedMethod("rpc3.localhost", c.method, c.marker, c.fallbackBody)
			f2 = mockCountedMethod("rpc4.localhost", c.method, c.marker, c.fallbackBody)
		},
		configure: func(cfgs []*common.UpstreamConfig) {
			for _, cfg := range cfgs {
				// Heads come from each poller's bootstrap poll. No ticker
				// poll runs during the test, so no poller is left inside a
				// mocked call when the next subtest resets gock, and no
				// tick can move a head the test controls.
				cfg.Evm.StatePollerInterval = common.Duration(time.Hour)
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
				if cfg.Id == "primary-1" && c.primary1Configure != nil {
					c.primary1Configure(cfg)
				}
			}
		},
		network: func(cfg *common.NetworkConfig) {
			cfg.DirectiveDefaults = &common.DirectiveDefaultsConfig{RetryEmpty: util.BoolPtr(true)}
			// One voter is enough for the served tip, so primary-2 alone at
			// 1002 makes the network confident about block 1002.
			cfg.Evm.ServedTip = &common.EvmServedTipConfig{Quorum: 1}
			if c.headCheckMethods != nil {
				cfg.Evm.EmptyResultHeadCheckMethods = c.headCheckMethods
			}
		},
	})

	// Pin the routed order in the policy only. The registry keeps all four
	// upstreams, so the escape can still find the fallbacks.
	policy.OverrideOrderForTest(network.policyEngine, network.networkId, "primary-1", "primary-2")

	if c.beforeForward != nil {
		c.beforeForward()
	}

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

const (
	traceFilterAt1002 = `[{"fromBlock":"0x3ea","toBlock":"0x3ea","toAddress":["0x00000000000000000000000000000000000000aa"]}]`
	oneTrace          = `{"jsonrpc":"2.0","id":1,"result":[{"action":{"to":"0x00000000000000000000000000000000000000aa"},"blockNumber":1002,"type":"call"}]}`
	callTarget        = "00000000000000000000000000000000000000cc"
	emptyCall         = `{"jsonrpc":"2.0","id":1,"result":"0x"}`
	someCall          = `{"jsonrpc":"2.0","id":1,"result":"0x2a"}`
)

func ethCallParams(block string) string {
	return `[{"to":"0x` + callTarget + `","data":"0x"},"` + block + `"]`
}

// The default emptyResultAccept list gets the same head check for the range
// methods (eth_getLogs, trace_filter). An empty answer from an upstream whose
// head is below the range end is not final: the request moves on to an
// upstream that has the block. No upstream lists the method in
// emptyResultAuthoritativeMethods here.
func TestEmptyFinal_AcceptListAboveHeadIsNotFinal(t *testing.T) {
	t.Run("EthGetLogs", func(t *testing.T) {
		r := runEmptyFinalCase(t, emptyFinalCase{
			method: "eth_getLogs", params: logsAt1002, marker: logAddress,
			primaryBody: emptyLogs, fallbackBody: oneLog, defaultAccept: true,
			primaryLatest: "0x3e8", primary2Latest: "0x3ea", primary2Body: oneLog,
		})
		require.NoError(t, r.err)
		assert.Contains(t, resultString(t, r.resp), `"logIndex"`)
		assert.Equal(t, "primary-2", r.resp.UpstreamId())
		assert.GreaterOrEqual(t, r.primary1Calls, int32(1), "primary-1 must be asked first")
	})

	t.Run("TraceFilter", func(t *testing.T) {
		r := runEmptyFinalCase(t, emptyFinalCase{
			method: "trace_filter", params: traceFilterAt1002, marker: logAddress,
			primaryBody: emptyLogs, fallbackBody: oneTrace, defaultAccept: true,
			primaryLatest: "0x3e8", primary2Latest: "0x3ea", primary2Body: oneTrace,
		})
		require.NoError(t, r.err)
		assert.Contains(t, resultString(t, r.resp), `"action"`)
		assert.Equal(t, "primary-2", r.resp.UpstreamId())
		assert.GreaterOrEqual(t, r.primary1Calls, int32(1))
	})
}

// At or below the answering upstream's head, an accepted empty answer stays
// final: one call, no sweep, no escape.
func TestEmptyFinal_AcceptListAtOrBelowHeadIsFinal(t *testing.T) {
	cases := map[string]emptyFinalCase{
		"EthGetLogs": {method: "eth_getLogs", params: logsAt1001, marker: logAddress,
			primaryBody: emptyLogs, fallbackBody: oneLog},
		"EthCall": {method: "eth_call", params: ethCallParams("0x3ea"), marker: callTarget,
			primaryBody: emptyCall, fallbackBody: someCall},
		"EthCallLatestTag": {method: "eth_call", params: ethCallParams("latest"), marker: callTarget,
			primaryBody: emptyCall, fallbackBody: someCall},
	}
	for name, c := range cases {
		c.defaultAccept = true
		c.primaryLatest = "0x3ea" // 1002
		c.primary2Body = someCall
		if c.method == "eth_getLogs" {
			c.primary2Body = oneLog
		}
		t.Run(name, func(t *testing.T) {
			r := runEmptyFinalCase(t, c)
			require.NoError(t, r.err)
			assert.Equal(t, "primary-1", r.resp.UpstreamId())
			assert.Equal(t, int32(1), r.primaryCalls, "no sweep to primary-2")
			assert.Equal(t, int32(0), r.fallbackCalls)
			assert.Equal(t, float64(0), r.escapes)
		})
	}
}

// Value reads are not head-checked: their empty answer stays final, by tag
// or by number, from the accept list or the authoritative list, even when
// the answering upstream's known head is below the block. Each case passes
// on the merge base and on this code alike.
func TestEmptyFinal_ValueReadsUnchanged(t *testing.T) {
	const addr = `"0x00000000000000000000000000000000000000cc"`
	cases := map[string]emptyFinalCase{
		"EthCallNumber": {method: "eth_call", params: ethCallParams("0x3ea"),
			primaryBody: emptyCall, primary2Body: someCall},
		"EthCallLatest": {method: "eth_call", params: ethCallParams("latest"),
			primaryBody: emptyCall, primary2Body: someCall},
		"EthCallPending": {method: "eth_call", params: ethCallParams("pending"),
			primaryBody: emptyCall, primary2Body: someCall},
		"EthGetBalance": {method: "eth_getBalance", params: `[` + addr + `,"0x3ea"]`,
			primaryBody: zeroBalance, primary2Body: someBalance},
		"EthGetBalanceAuthoritative": {method: "eth_getBalance", params: `[` + addr + `,"0x3ea"]`,
			primaryBody: zeroBalance, primary2Body: someBalance, authoritative: true},
		"EthGetCode": {method: "eth_getCode", params: `[` + addr + `,"0x3ea"]`,
			primaryBody: emptyCall, primary2Body: someCall},
		"EthGetStorageAt": {method: "eth_getStorageAt", params: `[` + addr + `,"0x0","0x3ea"]`,
			primaryBody:  `{"jsonrpc":"2.0","id":1,"result":"0x0000000000000000000000000000000000000000000000000000000000000000"}`,
			primary2Body: someCall},
		"EthGetTransactionCount": {method: "eth_getTransactionCount", params: `[` + addr + `,"0x3ea"]`,
			primaryBody: zeroBalance, primary2Body: someBalance},
	}
	for name, c := range cases {
		c.marker = callTarget
		c.fallbackBody = c.primary2Body
		c.defaultAccept = !c.authoritative
		c.primaryLatest = "0x3e8" // 1000 < 1002
		c.primary2Latest = "0x3ea"
		t.Run(name, func(t *testing.T) {
			r := runEmptyFinalCase(t, c)
			require.NoError(t, r.err)
			assert.Equal(t, "primary-1", r.resp.UpstreamId())
			assert.Equal(t, int32(1), r.primaryCalls, "no sweep to primary-2")
			assert.Equal(t, int32(0), r.fallbackCalls)
			assert.Equal(t, float64(0), r.escapes)
		})
	}
}

// primary-1's head is unknown: its state poller never got a latest block.
// The refresh cannot find one either, so its [] is not final and primary-2,
// which has the block, answers.
func TestEmptyFinal_UnknownHeadIsNotFinal(t *testing.T) {
	r := runEmptyFinalCase(t, emptyFinalCase{
		method: "eth_getLogs", params: logsAt1002, marker: logAddress,
		primaryBody: emptyLogs, fallbackBody: oneLog, defaultAccept: true,
		primaryLatest: "0x3ea", primary2Latest: "0x3ea", primary2Body: oneLog,
		primary1Mocks: func() {
			gock.New("http://rpc1.localhost").Post("").Persist().
				Filter(func(r *http.Request) bool {
					b := util.SafeReadBody(r)
					return r.URL.Host == "rpc1.localhost" &&
						strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, `"latest"`)
				}).
				Reply(200).
				JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"internal error"}}`))
		},
	})
	require.NoError(t, r.err)
	assert.Contains(t, resultString(t, r.resp), `"logIndex"`)
	assert.Equal(t, "primary-2", r.resp.UpstreamId())
	assert.GreaterOrEqual(t, r.primary1Calls, int32(1), "primary-1 must be asked first")
	assert.Equal(t, int32(0), r.fallbackCalls)
	assert.Equal(t, float64(0), r.escapes)
}

// primary-1 has block 1002, but its poller last saw 1000 and polls only
// once an hour. The refresh before judging asks the poller for a fresh head,
// finds 1002, and primary-1's true [] is final: primary-2 is never asked.
func TestEmptyFinal_LaggingPollerRefreshMakesFinal(t *testing.T) {
	var nodeAt1002 atomic.Bool
	var headPolls atomic.Int32
	latestFrom := func(want bool) func(*http.Request) bool {
		return func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			if r.URL.Host != "rpc1.localhost" || !strings.Contains(b, "eth_getBlockByNumber") ||
				!strings.Contains(b, `"latest"`) || nodeAt1002.Load() != want {
				return false
			}
			if want {
				headPolls.Add(1)
			}
			return true
		}
	}
	r := runEmptyFinalCase(t, emptyFinalCase{
		method: "eth_getLogs", params: logsAt1002, marker: logAddress,
		primaryBody: emptyLogs, fallbackBody: oneLog, defaultAccept: true,
		primaryLatest: "0x3e8", primary2Latest: "0x3ea", primary2Body: oneLog,
		primary1Mocks: func() {
			gock.New("http://rpc1.localhost").Post("").Persist().Filter(latestFrom(false)).
				Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x3e8","timestamp":"0x6702a8f0"}}`))
			gock.New("http://rpc1.localhost").Post("").Persist().Filter(latestFrom(true)).
				Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x3ea","timestamp":"0x6702a8f0"}}`))
		},
		primary1Configure: func(cfg *common.UpstreamConfig) {
			cfg.Evm.StatePollerInterval = common.Duration(time.Hour)
		},
		// The node imports 1002 after the poller's last poll.
		beforeForward: func() { nodeAt1002.Store(true) },
	})
	require.NoError(t, r.err)
	assert.Equal(t, "[]", resultString(t, r.resp))
	assert.Equal(t, "primary-1", r.resp.UpstreamId())
	assert.Equal(t, int32(1), r.primaryCalls, "the refreshed head makes primary-1's answer final")
	assert.Equal(t, int32(0), r.fallbackCalls)
	assert.GreaterOrEqual(t, headPolls.Load(), int32(1), "the refresh must poll primary-1's head")
}

// An explicit empty evm.emptyResultHeadCheckMethods turns the check off:
// the above-head [] is final again.
func TestEmptyFinal_HeadCheckOffByConfig(t *testing.T) {
	r := runEmptyFinalCase(t, emptyFinalCase{
		method: "eth_getLogs", params: logsAt1002, marker: logAddress,
		primaryBody: emptyLogs, fallbackBody: oneLog, defaultAccept: true,
		primaryLatest: "0x3e8", primary2Latest: "0x3ea", primary2Body: oneLog,
		headCheckMethods: []string{},
	})
	require.NoError(t, r.err)
	assert.Equal(t, "[]", resultString(t, r.resp))
	assert.Equal(t, "primary-1", r.resp.UpstreamId())
	assert.Equal(t, int32(1), r.primaryCalls)
}

// eth_getLogs with no toBlock runs to latest. primary-1's head (1000)
// reaches fromBlock but not the served tip (1002), so its [] is not final:
// the range end is the tip, not fromBlock.
func TestEmptyFinal_GetLogsWithoutToBlockChecksTheTip(t *testing.T) {
	r := runEmptyFinalCase(t, emptyFinalCase{
		method: "eth_getLogs", marker: logAddress,
		params:      `[{"fromBlock":"0x3e8","address":"0x00000000000000000000000000000000000000aa"}]`,
		primaryBody: emptyLogs, fallbackBody: oneLog, defaultAccept: true,
		primaryLatest: "0x3e8", primary2Latest: "0x3ea", primary2Body: oneLog,
	})
	require.NoError(t, r.err)
	assert.Contains(t, resultString(t, r.resp), `"logIndex"`)
	assert.Equal(t, "primary-2", r.resp.UpstreamId())
	assert.GreaterOrEqual(t, r.primary1Calls, int32(1), "primary-1 must be asked first")
}

// With no toBlock and primary-1 at the tip, its [] stays final.
func TestEmptyFinal_GetLogsWithoutToBlockAtTipIsFinal(t *testing.T) {
	r := runEmptyFinalCase(t, emptyFinalCase{
		method: "eth_getLogs", marker: logAddress,
		params:      `[{"fromBlock":"0x3e8","address":"0x00000000000000000000000000000000000000aa"}]`,
		primaryBody: emptyLogs, fallbackBody: oneLog, defaultAccept: true,
		primaryLatest: "0x3ea", primary2Latest: "0x3ea", primary2Body: oneLog,
	})
	require.NoError(t, r.err)
	assert.Equal(t, "[]", resultString(t, r.resp))
	assert.Equal(t, "primary-1", r.resp.UpstreamId())
	assert.Equal(t, int32(1), r.primaryCalls)
}

// With a hedge, an authoritative empty answer wins as soon as it lands.
// primary-1 lists eth_getLogs, has the block, and answers [] after 40 ms.
// The hedge has already asked primary-2, which answers data after 300 ms.
// The hedge must keep primary-1's final [] instead of waiting for
// primary-2.
func TestEmptyFinal_HedgeKeepsAnAuthoritativeEmpty(t *testing.T) {
	r := runEmptyFinalCase(t, emptyFinalCase{
		method: "eth_getLogs", params: logsAt1001, marker: logAddress,
		primaryBody: emptyLogs, fallbackBody: oneLog, primary2Body: oneLog,
		authoritative: true, primaryLatest: "0x3ea",
		hedge: &common.HedgePolicyConfig{
			Delay:    common.NewStaticDuration(5 * time.Millisecond),
			MaxCount: 1,
		},
		primaryDelay:  40 * time.Millisecond,
		primary2Delay: 300 * time.Millisecond,
	})
	require.NoError(t, r.err)
	assert.Equal(t, "[]", resultString(t, r.resp))
	assert.Equal(t, "primary-1", r.resp.UpstreamId())
}
