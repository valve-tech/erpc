package erpc

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	policystdlib "github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/thirdparty"
	"github.com/erpc/erpc/upstream"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockJsonRpcUpstream wires the state-poller mocks for one upstream at a
// fixed block height.
func mockJsonRpcUpstream(host string, chainIdHex, latestHex, finalizedHex string) {
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_chainId")
		}).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"result":"%s"}`, chainIdHex)))

	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, `"latest"`)
		}).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"result":{"number":"%s","timestamp":"0x6702a8f0"}}`, latestHex)))

	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			b := util.SafeReadBody(r)
			return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, `"finalized"`)
		}).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"result":{"number":"%s","timestamp":"0x6702a8e0"}}`, finalizedHex)))

	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_syncing")
		}).
		Reply(200).
		JSON([]byte(`{"result":false}`))
}

// mockEthCallReturning wires an eth_call mock whose result identifies the
// upstream that served it.
func mockEthCallReturning(host string, resultHex string) {
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_call")
		}).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":"%s"}`, resultHex)))
}

// headBoundedAvailability makes an upstream serve only blocks up to its
// polled head; the availability gate has no such implicit bound.
func headBoundedAvailability() *common.EvmBlockAvailabilityConfig {
	return &common.EvmBlockAvailabilityConfig{
		Upper: &common.EvmAvailabilityBoundConfig{LatestBlockMinus: i64(0)},
	}
}

// failoverUpstreamConfigs builds 2 primaries and 2 fallback-tier upstreams on
// rpc1..rpc4.localhost.
func failoverUpstreamConfigs() []*common.UpstreamConfig {
	var cfgs []*common.UpstreamConfig
	for i, id := range []string{"primary-1", "primary-2", "fallback-1", "fallback-2"} {
		cfg := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       id,
			Endpoint: fmt.Sprintf("http://rpc%d.localhost", i+1),
			Evm: &common.EvmUpstreamConfig{
				ChainId:             999,
				StatePollerInterval: common.Duration(100 * time.Millisecond),
				StatePollerDebounce: common.Duration(20 * time.Millisecond),
				BlockAvailability:   headBoundedAvailability(),
			},
		}
		if strings.HasPrefix(id, "fallback") {
			cfg.Tags = []string{common.TagTierFallback}
		}
		cfgs = append(cfgs, cfg)
	}
	return cfgs
}

// buildFailoverNetwork wires a Network with the default selection policy
// (fallback tier cordoned while a primary is healthy) on a frozen ticker;
// callers drive ticks via policy.TickForTest.
func buildFailoverNetwork(
	t *testing.T, ctx context.Context,
	upstreamConfigs []*common.UpstreamConfig,
	opts failoverFixtureOpts,
) (*Network, *upstream.UpstreamsRegistry, *health.Tracker) {
	t.Helper()

	rlCfg := opts.rateLimiters
	if rlCfg == nil {
		rlCfg = &common.RateLimiterConfig{}
	}
	rlr, err := upstream.NewRateLimitersRegistry(context.Background(), rlCfg, &log.Logger)
	require.NoError(t, err)
	// 5s metrics window so accumulated request/failure samples don't get
	// reset mid-test.
	mt := health.NewTracker(&log.Logger, "main", 5*time.Second)
	vr := thirdparty.NewVendorsRegistry()
	pr, err := thirdparty.NewProvidersRegistry(&log.Logger, vr, nil, nil)
	require.NoError(t, err)

	sharedStateCfg := &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: common.DriverMemory,
			Memory: &common.MemoryConnectorConfig{
				MaxItems:     100_000,
				MaxTotalSize: "1GB",
			},
		},
		LockMaxWait:     common.Duration(200 * time.Millisecond),
		UpdateMaxWait:   common.Duration(200 * time.Millisecond),
		FallbackTimeout: common.Duration(3 * time.Second),
		LockTtl:         common.Duration(4 * time.Second),
	}
	require.NoError(t, sharedStateCfg.SetDefaults("test"))
	ssr, err := data.NewSharedStateRegistry(ctx, &log.Logger, sharedStateCfg)
	require.NoError(t, err)

	upr := upstream.NewUpstreamsRegistry(
		ctx, &log.Logger, "main", upstreamConfigs,
		ssr, rlr, vr, pr, nil, mt, nil,
	)

	networkConfig := &common.NetworkConfig{
		Architecture: common.ArchitectureEvm,
		Evm: &common.EvmNetworkConfig{
			ChainId:                  999,
			EnforceBlockAvailability: util.BoolPtr(true),
		},
		// Default selection policy, frozen ticker; tests drive ticks.
		SelectionPolicy: &common.SelectionPolicyConfig{
			EvalInterval: 0,
		},
	}
	if opts.enableFailover {
		networkConfig.Failover = &common.FailoverConfig{OnDefaultsExhausted: util.BoolPtr(true)}
	}
	networkConfig.Failsafe = opts.failsafe
	if opts.network != nil {
		opts.network(networkConfig)
	}

	var policyEngine *policy.Engine
	if !opts.noPolicy {
		policyEngine = policy.NewEngine(ctx, &log.Logger, "main", mt, policystdlib.Install, nil)
	}

	network, err := NewNetwork(ctx, &log.Logger, "main", networkConfig, rlr, upr, mt, policyEngine)
	stopAtEnd(t, network)
	require.NoError(t, err)

	upr.Bootstrap(ctx)
	time.Sleep(200 * time.Millisecond)
	require.NoError(t, upr.GetInitializer().WaitForTasks(ctx))
	require.NoError(t, upr.PrepareUpstreamsForNetwork(ctx, util.EvmNetworkId(999)))
	require.NoError(t, network.Bootstrap(ctx))

	upsList := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(999))
	require.Len(t, upsList, len(upstreamConfigs))
	for _, ups := range upsList {
		require.NoError(t, ups.Bootstrap(ctx))
	}

	// Let the pollers run a couple of cycles.
	time.Sleep(500 * time.Millisecond)

	return network, upr, mt
}

// failoverFixtureOpts configures the failoverUpstreamConfigs layout.
type failoverFixtureOpts struct {
	primaryLatest  string
	fallbackLatest string
	enableFailover bool
	failsafe       []*common.FailsafeConfig
	// noPolicy leaves the network without a policy engine, so every
	// upstream (fallbacks included) is in the routed list.
	noPolicy bool
	// mocks registers test-specific mocks ahead of the standard ones, before
	// any poller starts.
	mocks func()
	// configure adjusts the upstream configs before the network is built.
	configure func(cfgs []*common.UpstreamConfig)
	// network adjusts the network config before the network is built.
	network func(cfg *common.NetworkConfig)
	// rateLimiters configures the rate limiters registry; nil means none.
	rateLimiters *common.RateLimiterConfig
}

func setupFailoverFixture(
	t *testing.T, ctx context.Context, opts failoverFixtureOpts,
) (*Network, []*upstream.Upstream, *health.Tracker) {
	t.Helper()
	util.ResetGock()

	const chainIdHex = "0x3e7" // 999
	const finalizedHex = "0x3e0"

	if opts.mocks != nil {
		opts.mocks()
	}
	mockJsonRpcUpstream("rpc1.localhost", chainIdHex, opts.primaryLatest, finalizedHex)
	mockJsonRpcUpstream("rpc2.localhost", chainIdHex, opts.primaryLatest, finalizedHex)
	mockJsonRpcUpstream("rpc3.localhost", chainIdHex, opts.fallbackLatest, finalizedHex)
	mockJsonRpcUpstream("rpc4.localhost", chainIdHex, opts.fallbackLatest, finalizedHex)

	mockEthCallReturning("rpc1.localhost", "0x1111")
	mockEthCallReturning("rpc2.localhost", "0x2222")
	mockEthCallReturning("rpc3.localhost", "0x3333")
	mockEthCallReturning("rpc4.localhost", "0x4444")

	cfgs := failoverUpstreamConfigs()
	if opts.configure != nil {
		opts.configure(cfgs)
	}
	network, upr, mt := buildFailoverNetwork(t, ctx, cfgs, opts)

	upsList := upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(999))
	require.Len(t, upsList, 4)

	// With healthy primaries the default policy cordons the fallbacks, so
	// only the per-request escape brings them in.
	if network.policyEngine != nil {
		policy.ResetSlotStateForTest(network.policyEngine, network.networkId, "*")
		policy.TickForTest(network.policyEngine, network.networkId, "*")
	}
	require.NoError(t, upr.RefreshUpstreamNetworkMethodScores())
	time.Sleep(50 * time.Millisecond)

	return network, upsList, mt
}

// ethCallRequest constructs an eth_call request targeting a specific block.
func ethCallRequest(id int, blockHex string) *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"eth_call","params":[{"to":"0xdead","data":"0x"},"%s"]}`,
		id, blockHex,
	)))
}

// Once the primaries are exhausted with retryable errors, the same request
// escalates to the fallback tier.
func TestFailover_EscapeHatch(t *testing.T) {
	t.Run("EscapesToFallbackOnFirstFailingRequest", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Primaries at 1000 skip block 1002; the fallbacks at 1002 serve it.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3ea", // 1002
			enableFailover: true,
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		req := ethCallRequest(1, "0x3ea")
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err, "client request must succeed on first try via escape hatch")
		require.NotNil(t, resp)
		defer resp.Release()

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		result := strings.Trim(jrr.GetResultString(), `"`)
		assert.Contains(t, []string{"0x3333", "0x4444"}, result,
			"escape hatch must route to a fallback; got %q", result)

		after := promUtil.ToFloat64(counter)
		assert.Equal(t, before+1, after,
			"MetricNetworkFallbackEscapeTotal must increment by exactly 1 for the escape firing")
	})

	t.Run("NoEscapeWhenPrimariesHealthy", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3ea", // 1002
			fallbackLatest: "0x3ea", // 1002
			enableFailover: true,
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		for i := 0; i < 30; i++ {
			req := ethCallRequest(i, "0x3ea")
			req.SetNetwork(network)
			resp, err := network.Forward(ctx, req)
			require.NoError(t, err, "healthy primary request must succeed (iter %d)", i)
			require.NotNil(t, resp)

			jrr, _ := resp.JsonRpcResponse()
			result := strings.Trim(jrr.GetResultString(), `"`)
			assert.Contains(t, []string{"0x1111", "0x2222"}, result,
				"healthy request must be served by a primary (0x1111/0x2222), got %q (iter %d)", result, i)
			resp.Release()
		}

		after := promUtil.ToFloat64(counter)
		assert.Equal(t, before, after,
			"escape hatch must NOT fire when primaries are healthy; counter must be unchanged")
	})

	t.Run("NoEscapeWhenFailoverDisabled", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// The default selection policy may still route to the fallback tier
		// on its own; only the escape counter is asserted.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3ea", // 1002
			enableFailover: false,   // <-- disabled
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)
		req := ethCallRequest(1, "0x3ea")
		req.SetNetwork(network)
		resp, _ := network.Forward(ctx, req)
		if resp != nil {
			resp.Release()
		}

		after := promUtil.ToFloat64(counter)
		assert.Equal(t, before, after,
			"with failover disabled the per-request escape hatch must NOT fire; counter must be unchanged")
	})

	t.Run("OnlyEscalatesOncePerRequest", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Every upstream skips block 1002 (retryable); the escape fires once
		// and does not re-escalate after the fallbacks also skip.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3e8", // also 1000
			enableFailover: true,
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		req := ethCallRequest(1, "0x3ea")
		req.SetNetwork(network)
		resp, _ := network.Forward(ctx, req)
		if resp != nil {
			resp.Release()
		}

		after := promUtil.ToFloat64(counter)
		assert.Equal(t, before+1, after,
			"escape must fire EXACTLY once per request (no re-escalation loop); got %v→%v",
			before, after)
	})

	t.Run("EscapesOnNonRetryableGateSkip", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Block 9000 is beyond MaxRetryableBlockDistance of the primaries at
		// 1000, so their skips are non-retryable; the escape must still fire.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8",  // 1000
			fallbackLatest: "0x2710", // 10000
			enableFailover: true,
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		req := ethCallRequest(1, "0x2328") // 9000
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err,
			"request must succeed via fallback even when primary gate-skip is non-retryable "+
				"(distance > MaxRetryableBlockDistance)")
		require.NotNil(t, resp)
		defer resp.Release()

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		result := strings.Trim(jrr.GetResultString(), `"`)
		assert.Contains(t, []string{"0x3333", "0x4444"}, result,
			"non-retryable-gate-skip escape must route to a fallback; got %q", result)

		// The selection policy may already exclude primaries this far behind,
		// leaving the escape nothing to do.
		after := promUtil.ToFloat64(counter)
		assert.LessOrEqual(t, after-before, float64(1),
			"escape hatch must fire at most once for the non-retryable gate-skip case")
	})

	t.Run("EscapesOnEmptyishGetBlockByNumber", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Every upstream reports 1002, but the primaries return null for it.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3ea", // 1002
			fallbackLatest: "0x3ea", // 1002
			enableFailover: true,
			mocks: func() {
				nullBlock := `{"jsonrpc":"2.0","id":1,"result":null}`
				okBlock := `{"jsonrpc":"2.0","id":1,"result":{"number":"0x3ea","hash":"0xabc","parentHash":"0xdef","timestamp":"0x6702a8f0"}}`
				for _, host := range []string{"rpc1.localhost", "rpc2.localhost"} {
					gock.New("http://" + host).
						Post("").
						Persist().
						Filter(func(r *http.Request) bool {
							b := util.SafeReadBody(r)
							return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, `"0x3ea"`)
						}).
						Reply(200).
						JSON([]byte(nullBlock))
				}
				for _, host := range []string{"rpc3.localhost", "rpc4.localhost"} {
					gock.New("http://" + host).
						Post("").
						Persist().
						Filter(func(r *http.Request) bool {
							b := util.SafeReadBody(r)
							return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, `"0x3ea"`)
						}).
						Reply(200).
						JSON([]byte(okBlock))
				}
			},
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_getBlockByNumber")
		before := promUtil.ToFloat64(counter)

		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x3ea",false]}`,
		))
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err, "null from primaries must escalate to fallbacks on the same request")
		require.NotNil(t, resp)
		defer resp.Release()

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		require.False(t, jrr.IsResultEmptyish(), "fallback must return a non-null block header")
		num, err := jrr.PeekStringByPath(ctx, "number")
		require.NoError(t, err)
		assert.Equal(t, "0x3ea", num)

		assert.Contains(t, []string{"fallback-1", "fallback-2"}, resp.UpstreamId(),
			"emptyish primary miss must be served by a fallback")

		after := promUtil.ToFloat64(counter)
		assert.Equal(t, before+1, after,
			"escape hatch must fire for emptyish eth_getBlockByNumber primary misses")
	})

	t.Run("NoEscapeOnErrorNonRetryableTowardNetwork", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Every provider returns the same verdict for this error, so sweeping
		// the fallbacks cannot help.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8",
			fallbackLatest: "0x3e8",
			enableFailover: true,
			mocks: func() {
				mockEthCallError("rpc1.localhost", -32000, "sender is over rate limit")
				mockEthCallError("rpc2.localhost", -32000, "sender is over rate limit")
			},
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)
		req := ethCallRequest(1, "0x3e8")
		req.SetNetwork(network)
		resp, _ := network.Forward(ctx, req)
		if resp != nil {
			resp.Release()
		}
		assert.Equal(t, before, promUtil.ToFloat64(counter))
	})

	t.Run("NoEscapeOnNullReceipt", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// A pending tx's receipt is null on every upstream; that is an
		// answer, not an outage.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8",
			fallbackLatest: "0x3e8",
			enableFailover: true,
			mocks: func() {
				for _, host := range []string{"rpc1.localhost", "rpc2.localhost", "rpc3.localhost", "rpc4.localhost"} {
					gock.New("http://" + host).
						Post("").
						Persist().
						Filter(func(r *http.Request) bool {
							return strings.Contains(util.SafeReadBody(r), "eth_getTransactionReceipt")
						}).
						Reply(200).
						JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
				}
			},
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_getTransactionReceipt")
		before := promUtil.ToFloat64(counter)
		for i := 0; i < 5; i++ {
			req := common.NewNormalizedRequest([]byte(fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%d,"method":"eth_getTransactionReceipt","params":["0x%064x"]}`, i, i+1,
			)))
			req.SetNetwork(network)
			resp, _ := network.Forward(ctx, req)
			if resp != nil {
				resp.Release()
			}
		}
		assert.Equal(t, before, promUtil.ToFloat64(counter))
	})

	t.Run("UseUpstreamPinLimitsTheEscape", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Primaries at 1000 skip block 1002; the fallbacks at 1002 could serve
		// it, but a pin that excludes them must keep the request off them.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8", // 1000
			fallbackLatest: "0x3ea", // 1002
			enableFailover: true,
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		req := ethCallRequest(1, "0x3ea")
		req.SetNetwork(network)
		req.SetDirectives(&common.RequestDirectives{UseUpstream: "primary-1"})
		resp, err := network.Forward(ctx, req)
		if resp != nil {
			assert.NotContains(t, []string{"fallback-1", "fallback-2"}, resp.UpstreamId(),
				"a request pinned to primary-1 must not be served by a fallback")
			resp.Release()
		}
		assert.Error(t, err, "the pinned upstream cannot serve the block, so the request fails")
		assert.Equal(t, before, promUtil.ToFloat64(counter), "a pin that excludes the fallbacks must not escape")

		// A pin that still admits the fallbacks keeps the escape.
		req = ethCallRequest(2, "0x3ea")
		req.SetNetwork(network)
		req.SetDirectives(&common.RequestDirectives{UseUpstream: "!primary-2"})
		resp, err = network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		defer resp.Release()
		assert.Contains(t, []string{"fallback-1", "fallback-2"}, resp.UpstreamId())
		assert.Equal(t, before+1, promUtil.ToFloat64(counter))
	})

	t.Run("KeepsTheRoutedEmptyAnswerWhenFallbacksFail", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// The primaries answer null for block 1002 and the fallbacks fail
		// retryably, with no retry left: the client must still get the null.
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3ea", // 1002
			fallbackLatest: "0x3ea", // 1002
			enableFailover: true,
			failsafe: []*common.FailsafeConfig{{
				MatchMethod: "*",
				Retry:       &common.RetryPolicyConfig{MaxAttempts: 1},
			}},
			mocks: func() {
				isBlock := func(r *http.Request) bool {
					b := util.SafeReadBody(r)
					return strings.Contains(b, "eth_getBlockByNumber") && strings.Contains(b, `"0x3ea"`)
				}
				for _, host := range []string{"rpc1.localhost", "rpc2.localhost"} {
					gock.New("http://" + host).Post("").Persist().Filter(isBlock).
						Reply(200).JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
				}
				for _, host := range []string{"rpc3.localhost", "rpc4.localhost"} {
					gock.New("http://" + host).Post("").Persist().Filter(isBlock).
						Reply(503).JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"unavailable"}}`))
				}
			},
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_getBlockByNumber")
		before := promUtil.ToFloat64(counter)

		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x3ea",false]}`,
		))
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err, "failed fallbacks must not replace the primaries' answer with an error")
		require.NotNil(t, resp)
		defer resp.Release()

		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.True(t, jrr.IsResultEmptyish())
		assert.Contains(t, []string{"primary-1", "primary-2"}, resp.UpstreamId())
		assert.Equal(t, before+1, promUtil.ToFloat64(counter), "the escape must still have been tried")
	})

	t.Run("DoesNotResweepFallbacksAlreadyTried", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Without a policy engine the fallbacks are already in the routed
		// list, so the sweep has tried them before the escape is considered.
		var fallbackHits atomic.Int64
		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8",
			fallbackLatest: "0x3e8",
			enableFailover: true,
			noPolicy:       true,
			mocks: func() {
				for _, host := range []string{"rpc1.localhost", "rpc2.localhost", "rpc3.localhost", "rpc4.localhost"} {
					host := host
					fallback := host == "rpc3.localhost" || host == "rpc4.localhost"
					gock.New("http://" + host).
						Post("").
						Persist().
						Filter(func(r *http.Request) bool {
							// Filters run before host matching.
							if r.URL.Host != host || !strings.Contains(util.SafeReadBody(r), "eth_call") {
								return false
							}
							if fallback {
								fallbackHits.Add(1)
							}
							return true
						}).
						Reply(200).
						JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"internal error"}}`))
				}
			},
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)
		req := ethCallRequest(1, "0x3e8")
		req.SetNetwork(network)
		resp, _ := network.Forward(ctx, req)
		if resp != nil {
			resp.Release()
		}
		assert.Equal(t, int64(2), fallbackHits.Load(), "each fallback is tried once")
		assert.Equal(t, before, promUtil.ToFloat64(counter), "nothing is left to escape to")
	})

	t.Run("ConcurrentHedgesEscalateAtMostOnce", func(t *testing.T) {
		t.Cleanup(util.ResetGock)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
			primaryLatest:  "0x3e8",
			fallbackLatest: "0x3e8",
			enableFailover: true,
			failsafe: []*common.FailsafeConfig{{
				MatchMethod: "*",
				Hedge:       &common.HedgePolicyConfig{Delay: common.NewStaticDuration(10 * time.Millisecond), MaxCount: 2},
				Retry:       &common.RetryPolicyConfig{MaxAttempts: 2},
			}},
			mocks: func() {
				for _, host := range []string{"rpc1.localhost", "rpc2.localhost"} {
					gock.New("http://" + host).
						Post("").
						Persist().
						Filter(func(r *http.Request) bool {
							return strings.Contains(util.SafeReadBody(r), "eth_call")
						}).
						Reply(200).
						Delay(30 * time.Millisecond).
						JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"internal error"}}`))
				}
			},
		})

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)
		const requests = 10
		var wg sync.WaitGroup
		for i := 0; i < requests; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				req := ethCallRequest(100+i, "0x3e8")
				req.SetNetwork(network)
				resp, _ := network.Forward(ctx, req)
				if resp != nil {
					resp.Release()
				}
			}(i)
		}
		wg.Wait()
		assert.LessOrEqual(t, promUtil.ToFloat64(counter)-before, float64(requests))
	})
}

// mockEthCallError wires an eth_call mock that returns a JSON-RPC error.
func mockEthCallError(host string, code int, message string) {
	gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			return strings.Contains(util.SafeReadBody(r), "eth_call")
		}).
		Reply(200).
		JSON([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"error":{"code":%d,"message":"%s"}}`, code, message)))
}

// A consensus slot whose upstream is skipped by block availability reports
// ErrUpstreamsExhausted, which consensus counts as "no attempt", rather than
// the raw skip error.
func TestNetwork_ConsensusSlotSkippedByBlockAvailability(t *testing.T) {
	t.Cleanup(util.ResetGock)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	network, _, _ := setupFailoverFixture(t, ctx, failoverFixtureOpts{
		primaryLatest:  "0x3e8", // 1000
		fallbackLatest: "0x3e8",
		failsafe: []*common.FailsafeConfig{{
			MatchMethod: "*",
			Consensus:   &common.ConsensusPolicyConfig{MaxParticipants: 2, AgreementThreshold: 2},
		}},
	})

	req := ethCallRequest(1, "0x3ea") // 1002: beyond every primary's served range
	req.SetNetwork(network)
	resp, err := network.Forward(ctx, req)
	if resp != nil {
		resp.Release()
	}
	assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted), "got %v", err)
}

// paidFallbackIds lists the two paid fallbacks in their configured escape
// order: QuickNode first, then Alchemy.
var paidFallbackIds = []string{"quicknode-evm-1", "alchemy-evm-1"}

// paidUpstreamHosts maps each upstream of the two-paid-fallbacks layout to
// its mock host.
var paidUpstreamHosts = map[string]string{
	"primary-1":       "rpc1.localhost",
	"primary-2":       "rpc2.localhost",
	"primary-3":       "rpc3.localhost",
	"quicknode-evm-1": "rpc4.localhost",
	"alchemy-evm-1":   "rpc5.localhost",
}

// paidUpstreamOrder is the configured order of the layout.
var paidUpstreamOrder = []string{"primary-1", "primary-2", "primary-3", "quicknode-evm-1", "alchemy-evm-1"}

// callLog records, in order, each upstream that received an HTTP call for
// one method.
type callLog struct {
	mu    sync.Mutex
	calls []string
}

func (c *callLog) add(id string) {
	c.mu.Lock()
	c.calls = append(c.calls, id)
	c.mu.Unlock()
}

func (c *callLog) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *callLog) reset() {
	c.mu.Lock()
	c.calls = nil
	c.mu.Unlock()
}

// mockReply is one upstream's canned answer for a method.
type mockReply struct {
	status int
	body   string
	delay  time.Duration
}

func okReply(resultJson string) mockReply {
	return mockReply{status: 200, body: fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%s}`, resultJson)}
}

// retryableFailure is a server error every policy treats as retryable.
var retryableFailure = mockReply{status: 503, body: `{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"unavailable"}}`}

// mockMethodLogged wires a method mock for one upstream that records each
// call in log before it replies.
func mockMethodLogged(id, method string, reply mockReply, log *callLog) {
	host := paidUpstreamHosts[id]
	m := gock.New("http://" + host).
		Post("").
		Persist().
		Filter(func(r *http.Request) bool {
			// Filters run before host matching.
			if r.URL.Host != host || !strings.Contains(util.SafeReadBody(r), method) {
				return false
			}
			log.add(id)
			return true
		}).
		Reply(reply.status)
	if reply.delay > 0 {
		m = m.Delay(reply.delay)
	}
	m.JSON([]byte(reply.body))
}

// denyBudget is a spent rate-limit budget: its "*" rule allows no call. The
// bootstrap's eth_chainId must not count against it, or the upstream never
// registers and the fixture's upstream count fails.
func denyBudget(id string) *common.RateLimitBudgetConfig {
	return &common.RateLimitBudgetConfig{
		Id: id,
		Rules: []*common.RateLimitRuleConfig{{
			Method:   "*",
			MaxCount: 0,
			Period:   common.RateLimitPeriodSecond,
		}},
	}
}

type paidFallbacksOpts struct {
	// replies sets each upstream's eth_call answer; the primaries default to
	// a slow retryable failure.
	replies map[string]mockReply
	// deny lists the paid fallbacks whose rate-limit budget denies every call.
	deny []string
	// extraMocks registers more mocks ahead of the standard ones.
	extraMocks func()
	// allowPinToFallback sets failover.allowPinToFallback on the network.
	allowPinToFallback bool
	// fallbackBreaker gives each paid fallback a breaker that one failure
	// opens for an hour.
	fallbackBreaker bool
	// latest overrides an upstream's latest block (hex); the default is
	// 0x3e8.
	latest map[string]string
}

// setupPaidFallbacksFixture builds 3 primaries and 2 paid tier:fallback
// upstreams (QuickNode, then Alchemy), with network retry 2 and hedge 1 and
// the default selection policy, which cordons the fallbacks while the
// primaries look healthy. It returns the eth_call log.
func setupPaidFallbacksFixture(t *testing.T, ctx context.Context, opts paidFallbacksOpts) (*Network, *callLog) {
	t.Helper()
	util.ResetGock()

	const chainIdHex = "0x3e7" // 999
	const latestHex = "0x3e8"  // 1000
	const finalizedHex = "0x3e0"

	calls := &callLog{}
	if opts.extraMocks != nil {
		opts.extraMocks()
	}
	for _, id := range paidUpstreamOrder {
		reply, ok := opts.replies[id]
		if !ok {
			if strings.HasPrefix(id, "primary") {
				reply = retryableFailure
				// Slow enough for the hedge to fire.
				reply.delay = 30 * time.Millisecond
			} else {
				reply = okReply(`"0xfa11"`)
			}
		}
		mockMethodLogged(id, "eth_call", reply, calls)
	}
	for _, id := range paidUpstreamOrder {
		latest := latestHex
		if v, ok := opts.latest[id]; ok {
			latest = v
		}
		mockJsonRpcUpstream(paidUpstreamHosts[id], chainIdHex, latest, finalizedHex)
	}

	rl := &common.RateLimiterConfig{Store: &common.RateLimitStoreConfig{Driver: "memory"}}
	denied := map[string]bool{}
	for _, id := range opts.deny {
		denied[id] = true
		rl.Budgets = append(rl.Budgets, denyBudget(id+"-budget"))
	}

	var cfgs []*common.UpstreamConfig
	for _, id := range paidUpstreamOrder {
		cfg := &common.UpstreamConfig{
			Type:     common.UpstreamTypeEvm,
			Id:       id,
			Endpoint: "http://" + paidUpstreamHosts[id],
			Evm: &common.EvmUpstreamConfig{
				ChainId:             999,
				StatePollerInterval: common.Duration(100 * time.Millisecond),
				StatePollerDebounce: common.Duration(20 * time.Millisecond),
				BlockAvailability:   headBoundedAvailability(),
			},
		}
		if !strings.HasPrefix(id, "primary") {
			// The valve paid-backstop shape: no probe mirrors, one attempt,
			// no hedge, and no auto-tuner rewriting the budget.
			cfg.Tags = []string{common.TagTierFallback}
			cfg.Routing = &common.UpstreamRoutingConfig{Probe: common.ProbeModeOff}
			cfg.Failsafe = []*common.FailsafeConfig{{
				MatchMethod: "*",
				Retry:       &common.RetryPolicyConfig{MaxAttempts: 1},
			}}
			if opts.fallbackBreaker {
				cfg.Failsafe[0].CircuitBreaker = &common.CircuitBreakerPolicyConfig{
					FailureThresholdCount:    1,
					FailureThresholdCapacity: 1,
					HalfOpenAfter:            common.Duration(time.Hour),
					SuccessThresholdCount:    1,
					SuccessThresholdCapacity: 1,
				}
			}
			cfg.RateLimitAutoTune = &common.RateLimitAutoTuneConfig{Enabled: util.BoolPtr(false)}
		}
		if denied[id] {
			cfg.RateLimitBudget = id + "-budget"
		}
		cfgs = append(cfgs, cfg)
	}

	network, upr, _ := buildFailoverNetwork(t, ctx, cfgs, failoverFixtureOpts{
		enableFailover: true,
		rateLimiters:   rl,
		failsafe: []*common.FailsafeConfig{{
			MatchMethod: "*",
			Retry:       &common.RetryPolicyConfig{MaxAttempts: 2},
			Hedge:       &common.HedgePolicyConfig{Delay: common.NewStaticDuration(10 * time.Millisecond), MaxCount: 1},
		}},
		network: func(cfg *common.NetworkConfig) {
			if opts.allowPinToFallback {
				cfg.Failover.AllowPinToFallback = util.BoolPtr(true)
			}
		},
	})

	// Healthy primaries at the tick: the default policy cordons the fallbacks.
	policy.ResetSlotStateForTest(network.policyEngine, network.networkId, "*")
	policy.TickForTest(network.policyEngine, network.networkId, "*")
	require.NoError(t, upr.RefreshUpstreamNetworkMethodScores())
	time.Sleep(50 * time.Millisecond)

	// The escape walks the fallbacks in this order.
	var order []string
	for _, up := range upr.GetNetworkUpstreams(ctx, util.EvmNetworkId(999)) {
		order = append(order, up.Id())
	}
	t.Logf("registry order: %v", order)

	return network, calls
}

// fallbackCalls keeps only the calls that reached a paid fallback.
func fallbackCalls(calls []string) []string {
	var out []string
	for _, id := range calls {
		if !strings.HasPrefix(id, "primary") {
			out = append(out, id)
		}
	}
	return out
}

// TestFailover_TwoPaidFallbacks pins the escape order valve's relay depends
// on: when every primary fails retryably, the escape tries QuickNode, then
// Alchemy, each at most once per request, and a rate-limit budget that
// denies one paid fallback moves the escape to the next.
func TestFailover_TwoPaidFallbacks(t *testing.T) {
	t.Run("QuickNodeServesAndAlchemyIsNotCalled", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			replies: map[string]mockReply{
				"quicknode-evm-1": okReply(`"0x9999"`),
				"alchemy-evm-1":   okReply(`"0xa1c0"`),
			},
		})
		calls.reset()

		req := ethCallRequest(1, "0x3e8")
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		defer resp.Release()

		assert.Equal(t, "quicknode-evm-1", resp.UpstreamId())
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, "0x9999", strings.Trim(jrr.GetResultString(), `"`))
		assert.Equal(t, []string{"quicknode-evm-1"}, fallbackCalls(calls.snapshot()),
			"QuickNode answered, so Alchemy must not be called; calls: %v", calls.snapshot())
	})

	t.Run("AlchemyOnlyAfterQuickNodeFails", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			replies: map[string]mockReply{
				"quicknode-evm-1": retryableFailure,
				"alchemy-evm-1":   okReply(`"0xa1c0"`),
			},
		})
		calls.reset()

		req := ethCallRequest(1, "0x3e8")
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		defer resp.Release()

		assert.Equal(t, "alchemy-evm-1", resp.UpstreamId())
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, "0xa1c0", strings.Trim(jrr.GetResultString(), `"`))
		assert.Equal(t, paidFallbackIds, fallbackCalls(calls.snapshot()),
			"QuickNode first, then Alchemy, each once; calls: %v", calls.snapshot())
	})

	t.Run("QuickNodeBudgetDeniesSoAlchemyServes", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			replies: map[string]mockReply{
				"quicknode-evm-1": okReply(`"0x9999"`),
				"alchemy-evm-1":   okReply(`"0xa1c0"`),
			},
			deny: []string{"quicknode-evm-1"},
		})
		calls.reset()

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		req := ethCallRequest(1, "0x3e8")
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		defer resp.Release()

		assert.Equal(t, "alchemy-evm-1", resp.UpstreamId())
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, "0xa1c0", strings.Trim(jrr.GetResultString(), `"`))
		// The budget denies before any HTTP call, so QuickNode sends nothing.
		assert.Equal(t, []string{"alchemy-evm-1"}, fallbackCalls(calls.snapshot()),
			"calls: %v", calls.snapshot())
		assert.Equal(t, before+1, promUtil.ToFloat64(counter), "one escape for the request")

		qnErr, ok := upstreamError(req, "quicknode-evm-1")
		require.True(t, ok, "the escape must have tried QuickNode")
		assert.True(t, common.HasErrorCode(qnErr, common.ErrCodeUpstreamRateLimitRuleExceeded),
			"QuickNode's error must be its budget denial; got %v", qnErr)
	})

	t.Run("BothBudgetsDenyGivesExhaustedAndNoThirdCall", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			replies: map[string]mockReply{
				"quicknode-evm-1": okReply(`"0x9999"`),
				"alchemy-evm-1":   okReply(`"0xa1c0"`),
			},
			deny: []string{"quicknode-evm-1", "alchemy-evm-1"},
		})
		calls.reset()

		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_call")
		before := promUtil.ToFloat64(counter)

		req := ethCallRequest(1, "0x3e8")
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		if resp != nil {
			resp.Release()
		}
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted),
			"want the aggregated exhausted error; got %v", err)
		for _, id := range paidFallbackIds {
			fbErr, ok := upstreamError(req, id)
			require.True(t, ok, "the escape must have tried %s", id)
			assert.True(t, common.HasErrorCode(fbErr, common.ErrCodeUpstreamRateLimitRuleExceeded),
				"%s's error must be its budget denial; got %v", id, fbErr)
		}

		// Both budgets deny before any HTTP call. No third escape target
		// exists, and the request must not go round again.
		assert.Empty(t, fallbackCalls(calls.snapshot()), "calls: %v", calls.snapshot())
		assert.Equal(t, before+1, promUtil.ToFloat64(counter), "exactly one escape for the request")
		// 2 network attempts, each an original and 1 hedge, over 3 primaries.
		assert.LessOrEqual(t, len(calls.snapshot()), 2*2*3,
			"at most one call per primary per sweep; calls: %v", calls.snapshot())
	})

	t.Run("UseUpstreamReachesAlchemyAloneForGetFilterChanges", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		filterCalls := &callLog{}
		network, _ := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			allowPinToFallback: true,
			replies: map[string]mockReply{
				"primary-1": okReply(`"0x1111"`),
				"primary-2": okReply(`"0x2222"`),
				"primary-3": okReply(`"0x3333"`),
			},
			extraMocks: func() {
				for _, id := range paidUpstreamOrder {
					mockMethodLogged(id, "eth_getFilterChanges",
						okReply(fmt.Sprintf(`["%s"]`, id)), filterCalls)
				}
			},
		})

		// The policy cordons tier:fallback while the primaries are healthy.
		counter := telemetry.MetricNetworkFallbackEscapeTotal.WithLabelValues("main", "evm:999", "eth_getFilterChanges")
		before := promUtil.ToFloat64(counter)

		req := common.NewNormalizedRequest([]byte(
			`{"jsonrpc":"2.0","id":1,"method":"eth_getFilterChanges","params":["0x1"]}`,
		))
		h := http.Header{}
		h.Set("X-ERPC-Use-Upstream", "alchemy-evm-1")
		req.EnrichFromHttp(h, nil, common.UserAgentTrackingModeSimplified)
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		require.NoError(t, err)
		require.NotNil(t, resp)
		defer resp.Release()

		assert.Equal(t, "alchemy-evm-1", resp.UpstreamId())
		jrr, err := resp.JsonRpcResponse()
		require.NoError(t, err)
		assert.Equal(t, `["alchemy-evm-1"]`, jrr.GetResultString())
		assert.Equal(t, []string{"alchemy-evm-1"}, filterCalls.snapshot(),
			"the pin must reach Alchemy and nothing else")
		t.Logf("escape counter delta for the pinned request: %v", promUtil.ToFloat64(counter)-before)
	})

	// pinnedEthCall sends an eth_call at blockHex pinned to selector.
	pinnedEthCall := func(ctx context.Context, network *Network, selector, blockHex string) (*common.NormalizedRequest, *common.NormalizedResponse, error) {
		req := ethCallRequest(1, blockHex)
		h := http.Header{}
		h.Set("X-ERPC-Use-Upstream", selector)
		req.EnrichFromHttp(h, nil, common.UserAgentTrackingModeSimplified)
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		return req, resp, err
	}

	t.Run("UseUpstreamReachesAlchemyAtLatest", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			allowPinToFallback: true,
			replies: map[string]mockReply{
				"primary-1":     okReply(`"0x1111"`),
				"primary-2":     okReply(`"0x2222"`),
				"primary-3":     okReply(`"0x3333"`),
				"alchemy-evm-1": okReply(`"0xa1c0"`),
			},
		})
		calls.reset()

		_, resp, err := pinnedEthCall(ctx, network, "alchemy-evm-1", "latest")
		require.NoError(t, err)
		require.NotNil(t, resp)
		defer resp.Release()
		assert.Equal(t, "alchemy-evm-1", resp.UpstreamId())
		assert.Equal(t, []string{"alchemy-evm-1"}, calls.snapshot())
	})

	t.Run("UseUpstreamToFailingAlchemyCallsOnlyAlchemy", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			allowPinToFallback: true,
			replies: map[string]mockReply{
				"primary-1":     okReply(`"0x1111"`),
				"primary-2":     okReply(`"0x2222"`),
				"primary-3":     okReply(`"0x3333"`),
				"alchemy-evm-1": retryableFailure,
			},
		})
		calls.reset()

		_, resp, err := pinnedEthCall(ctx, network, "alchemy-evm-1", "0x3e8")
		if resp != nil {
			resp.Release()
		}
		require.Error(t, err)
		// Network retry 2 and hedge 1 must not pay the fallback again.
		assert.Equal(t, []string{"alchemy-evm-1"}, calls.snapshot(),
			"the pin must reach Alchemy alone, once")
	})

	t.Run("UseUpstreamToDeniedAlchemySendsNothing", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			allowPinToFallback: true,
			replies: map[string]mockReply{
				"primary-1": okReply(`"0x1111"`),
				"primary-2": okReply(`"0x2222"`),
				"primary-3": okReply(`"0x3333"`),
			},
			deny: []string{"alchemy-evm-1"},
		})
		calls.reset()

		req, resp, err := pinnedEthCall(ctx, network, "alchemy-evm-1", "0x3e8")
		if resp != nil {
			resp.Release()
		}
		require.Error(t, err)
		assert.Empty(t, calls.snapshot(), "the budget denies before any HTTP call")
		alErr, ok := upstreamError(req, "alchemy-evm-1")
		require.True(t, ok, "the pin must have tried Alchemy")
		assert.True(t, common.HasErrorCode(alErr, common.ErrCodeUpstreamRateLimitRuleExceeded),
			"Alchemy's error must be its budget denial; got %v", alErr)
	})

	healthyPrimaries := map[string]mockReply{
		"primary-1":     okReply(`"0x1111"`),
		"primary-2":     okReply(`"0x2222"`),
		"primary-3":     okReply(`"0x3333"`),
		"alchemy-evm-1": okReply(`"0xa1c0"`),
	}

	// pinRefused asserts the pin to Alchemy fails as it did in 0.3.0.18:
	// ErrUpstreamsExhausted, no eth_call to anyone, and Alchemy never
	// selected. An open breaker also refuses inside Upstream.Forward, so only
	// the missing attempt shows that the pin did not select Alchemy.
	pinRefused := func(t *testing.T, ctx context.Context, network *Network, calls *callLog) {
		t.Helper()
		calls.reset()
		req, resp, err := pinnedEthCall(ctx, network, "alchemy-evm-1", "0x3e8")
		if resp != nil {
			resp.Release()
		}
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted), "got %v", err)
		assert.Empty(t, calls.snapshot())
		alErr, tried := upstreamError(req, "alchemy-evm-1")
		assert.False(t, tried, "the pin must not select Alchemy; its error: %v", alErr)
	}

	t.Run("UseUpstreamToCordonedFallbackFailsWhenNotAllowed", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// allowPinToFallback is off by default.
		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{replies: healthyPrimaries})
		require.False(t, network.cfg.Failover.PinToFallbackAllowed())
		pinRefused(t, ctx, network, calls)
	})

	t.Run("UseUpstreamSkipsFallbackWithOpenBreaker", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		replies := map[string]mockReply{}
		for k, v := range healthyPrimaries {
			replies[k] = v
		}
		replies["alchemy-evm-1"] = retryableFailure
		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			replies:            replies,
			allowPinToFallback: true,
			fallbackBreaker:    true,
		})

		// One direct failure opens Alchemy's breaker.
		var alchemy *upstream.Upstream
		for _, up := range network.upstreamsRegistry.GetNetworkUpstreams(ctx, network.networkId) {
			if up.Id() == "alchemy-evm-1" {
				alchemy = up
			}
		}
		require.NotNil(t, alchemy)
		trip := ethCallRequest(2, "0x3e8")
		trip.SetNetwork(network)
		if resp, _ := alchemy.Forward(ctx, trip, true, false); resp != nil {
			resp.Release()
		}
		require.True(t, alchemy.IsDown("eth_call", common.DataFinalityStateFinalized), "the breaker must be open")

		pinRefused(t, ctx, network, calls)
	})

	t.Run("UseUpstreamSkipsFallbackWithHealthCordon", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			replies:            healthyPrimaries,
			allowPinToFallback: true,
		})
		for _, up := range network.upstreamsRegistry.GetNetworkUpstreams(ctx, network.networkId) {
			if up.Id() == "alchemy-evm-1" {
				network.metricsTracker.Cordon(up, "*", "test: unhealthy")
			}
		}
		pinRefused(t, ctx, network, calls)
	})

	t.Run("UseUpstreamSkipsFallbackThePolicyDropsForBlockLag", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Alchemy trails the tip by 40 blocks, past the default policy's
		// blockNumberLagAbove(16). That writes no tracker cordon.
		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			replies:            healthyPrimaries,
			allowPinToFallback: true,
			latest:             map[string]string{"alchemy-evm-1": "0x3c0"},
		})
		for _, up := range network.upstreamsRegistry.GetNetworkUpstreams(ctx, network.networkId) {
			if up.Id() == "alchemy-evm-1" {
				require.False(t, network.metricsTracker.IsCordoned(up, "eth_call"), "the lag must not show as a tracker cordon")
			}
		}
		pinRefused(t, ctx, network, calls)
	})

	t.Run("InternalPinDoesNotReachCordonedFallback", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			replies:            healthyPrimaries,
			allowPinToFallback: true,
		})
		calls.reset()

		// The integrity state prober pins each
		// upstream like this, on every pass.
		req := ethCallRequest(1, "0x3e8")
		req.SetDirectives(&common.RequestDirectives{UseUpstream: "alchemy-evm-1", UpstreamPolicies: common.UpstreamPoliciesTimeoutOnly})
		req.SetNetwork(network)
		resp, err := network.Forward(ctx, req)
		if resp != nil {
			resp.Release()
		}
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted), "got %v", err)
		assert.Empty(t, calls.snapshot(), "an internal pin must not spend the fallback's budget")
	})

	t.Run("UseUpstreamToUnknownIdStillExhausts", func(t *testing.T) {
		defer util.ResetGock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		network, calls := setupPaidFallbacksFixture(t, ctx, paidFallbacksOpts{
			allowPinToFallback: true,
			replies: map[string]mockReply{
				"primary-1": okReply(`"0x1111"`),
				"primary-2": okReply(`"0x2222"`),
				"primary-3": okReply(`"0x3333"`),
			},
		})
		calls.reset()

		_, resp, err := pinnedEthCall(ctx, network, "no-such-upstream", "0x3e8")
		if resp != nil {
			resp.Release()
		}
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeUpstreamsExhausted), "got %v", err)
		assert.Empty(t, calls.snapshot())
	})
}

// upstreamError returns the error the request recorded for upstream id.
func upstreamError(req *common.NormalizedRequest, id string) (error, bool) {
	var found error
	ok := false
	req.ErrorsByUpstream.Range(func(k, v any) bool {
		if up, isUp := k.(common.Upstream); isUp && up.Id() == id {
			found, _ = v.(error)
			ok = true
			return false
		}
		return true
	})
	return found, ok
}
