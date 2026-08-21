package upstream

import (
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/failsafe"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/telemetry"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The per-upstream lifecycle reads below decide routing and are what an
// operator sees on the admin surface. A wrong answer either sends traffic to a
// node that is out, or hides a node that is out.

// bareUpstream builds an Upstream with only the fields these tests read. It
// avoids NewUpstream where a client is not needed, so a test failure points at
// the method under test rather than at client construction.
func bareUpstream(t *testing.T, cfg *common.UpstreamConfig) *Upstream {
	t.Helper()
	lg := zerolog.Nop()
	u := &Upstream{
		ProjectId:      "test",
		appCtx:         t.Context(),
		logger:         &lg,
		config:         cfg,
		metricsTracker: health.NewTracker(&lg, "test", 2*time.Second),
	}
	u.networkLabel.Store("n/a")
	return u
}

// executorsWithBreakers wires an upstream with a method-scoped breaker and a
// catch-all breaker, which is the shape that separates IsDown from
// CircuitBreakerState.
func executorsWithBreakers(t *testing.T, u *Upstream) (methodScoped, catchAll *failsafe.Breaker) {
	t.Helper()
	brk := func() *common.CircuitBreakerPolicyConfig {
		return &common.CircuitBreakerPolicyConfig{
			FailureThresholdCount:    1,
			FailureThresholdCapacity: 1,
			HalfOpenAfter:            common.Duration(time.Hour),
			SuccessThresholdCount:    1,
			SuccessThresholdCapacity: 1,
		}
	}
	scoped := newTestExecutor(t, &common.UpstreamFailsafeConfig{MatchMethod: "eth_call", CircuitBreaker: brk()})
	wildcard := newTestExecutor(t, &common.UpstreamFailsafeConfig{MatchMethod: "*", CircuitBreaker: brk()})
	u.failsafeExecutors = []*upstreamExecutor{scoped, wildcard}
	return scoped.Breaker(), wildcard.Breaker()
}

func TestUpstream_IsDownAndCircuitBreakerStateAnswerDifferentQuestions(t *testing.T) {
	u := bareUpstream(t, &common.UpstreamConfig{Id: "u1"})
	scoped, catchAll := executorsWithBreakers(t, u)

	require.False(t, u.IsDown())
	require.Equal(t, failsafe.StateClosed, u.CircuitBreakerState())

	// One method is out. IsDown answers "is anything about this upstream
	// broken", so it must flip; the per-upstream pill reads the catch-all
	// breaker, which still serves every other method and must stay closed.
	scoped.Record(failsafe.OutcomeFailure)
	require.Equal(t, failsafe.StateOpen, scoped.State(), "sanity: the scoped breaker opened")
	assert.True(t, u.IsDown(), "an open method-scoped breaker means part of this upstream is down")
	assert.Equal(t, failsafe.StateClosed, u.CircuitBreakerState(),
		"CircuitBreakerState reports the catch-all breaker, not the first one it finds")

	// Now the catch-all opens: the whole upstream is out.
	catchAll.Record(failsafe.OutcomeFailure)
	assert.Equal(t, failsafe.StateOpen, u.CircuitBreakerState())
}

func TestUpstream_NilUpstreamReadsAsDownAndClosed(t *testing.T) {
	// Callers hold *Upstream from maps that may miss. Reporting a nil upstream
	// as usable would route a request into a nil dereference.
	var u *Upstream
	assert.True(t, u.IsDown())
	assert.Equal(t, failsafe.StateClosed, u.CircuitBreakerState())
	assert.Nil(t, u.MetricsTracker())
	assert.Nil(t, u.Tracker())
	assert.Nil(t, u.Vendor())
	assert.Nil(t, u.Config())
	assert.Equal(t, "", u.Id())
}

func TestUpstream_TrackerAccessorsReturnTheLiveTracker(t *testing.T) {
	u := bareUpstream(t, &common.UpstreamConfig{Id: "u1"})
	assert.Same(t, u.metricsTracker, u.MetricsTracker())
	// Tracker() is the interface-typed view the policy engine consumes; it has
	// to be the SAME object, or scoring reads an empty second tracker.
	require.NotNil(t, u.Tracker())
	assert.Equal(t, u.metricsTracker, u.Tracker())
}

func TestUpstream_BreakerTransitionsAreCounted(t *testing.T) {
	// The `upstream_breaker_state_change_total` series is how an operator sees
	// a breaker trip after the fact. It only exists because NewUpstream wires
	// the hook onto every configured breaker.
	label := "closed_to_open"
	before := testutil.ToFloat64(
		telemetry.MetricUpstreamBreakerStateChange.WithLabelValues("test", "brk-ups", label))

	hook := makeBreakerTransitionHook("test", "brk-ups")
	hook(failsafe.StateClosed, failsafe.StateOpen, "threshold reached")

	after := testutil.ToFloat64(
		telemetry.MetricUpstreamBreakerStateChange.WithLabelValues("test", "brk-ups", label))
	assert.Equal(t, before+1, after, "a breaker transition must increment its labelled counter")
}

func TestUpstream_NewUpstreamWiresTheTransitionHookOntoEveryBreaker(t *testing.T) {
	reg, _ := newBootstrapTestRegistry(t)
	ups, err := reg.NewUpstream(&common.UpstreamConfig{
		Id:       "wired",
		Type:     common.UpstreamTypeEvm,
		Endpoint: bootstrapTestEndpoint,
		Evm:      &common.EvmUpstreamConfig{ChainId: 123, StatePollerInterval: common.Duration(time.Hour)},
		Failsafe: []*common.UpstreamFailsafeConfig{{
			MatchMethod: "*",
			CircuitBreaker: &common.CircuitBreakerPolicyConfig{
				FailureThresholdCount: 1, FailureThresholdCapacity: 1,
				HalfOpenAfter:         common.Duration(time.Hour),
				SuccessThresholdCount: 1, SuccessThresholdCapacity: 1,
			},
		}},
	})
	require.NoError(t, err)

	require.Equal(t, failsafe.StateClosed, ups.CircuitBreakerState())

	// Every configured breaker must carry the hook. This is the synchronous
	// half of the check — the breaker fires OnTransition on a goroutine, so a
	// missing hook is only observable here without racing.
	var breakers int
	for _, fe := range ups.failsafeExecutors {
		if b := fe.Breaker(); b != nil {
			breakers++
			assert.NotNil(t, b.OnTransition,
				"NewUpstream left a breaker without a transition hook; its trips would be invisible")
		}
	}
	require.Equal(t, 1, breakers, "the configured failsafe block must produce exactly one breaker")

	before := testutil.ToFloat64(
		telemetry.MetricUpstreamBreakerStateChange.WithLabelValues("test", "wired", "closed_to_open"))

	// Drive the real breaker through the real executor list.
	for _, fe := range ups.failsafeExecutors {
		if b := fe.Breaker(); b != nil {
			b.Record(failsafe.OutcomeFailure)
		}
	}
	require.Equal(t, failsafe.StateOpen, ups.CircuitBreakerState())

	// The hook runs on its own goroutine (failsafe/breaker.go transitionLocked),
	// so the counter lands shortly after the transition, not with it.
	assert.Eventually(t, func() bool {
		return testutil.ToFloat64(
			telemetry.MetricUpstreamBreakerStateChange.WithLabelValues("test", "wired", "closed_to_open")) == before+1
	}, 5*time.Second, 10*time.Millisecond,
		"the breaker trip never reached upstream_breaker_state_change_total")
}

// recordingEvmPoller records the network config SetNetworkConfig pushes down.
type recordingEvmPoller struct {
	*mockEvmStatePoller
	got *common.NetworkConfig
}

func (r *recordingEvmPoller) SetNetworkConfig(cfg *common.NetworkConfig) { r.got = cfg }

func TestUpstream_SetNetworkConfigBindsTheUpstreamToItsNetwork(t *testing.T) {
	t.Run("networkId and alias", func(t *testing.T) {
		u := bareUpstream(t, &common.UpstreamConfig{Id: "u1"})
		poller := &recordingEvmPoller{mockEvmStatePoller: &mockEvmStatePoller{}}
		u.evmStatePoller = poller

		cfg := &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 42161},
			Alias:        "arbitrum",
		}
		u.SetNetworkConfig(cfg)

		// networkId is the only thing binding an upstream to a network.
		assert.Equal(t, "evm:42161", u.NetworkId())
		// The alias is what an operator named the network; it must win as the
		// metric label or every dashboard reads raw chain ids.
		assert.Equal(t, "arbitrum", u.NetworkLabel())
		// The poller has no network reference of its own.
		assert.Same(t, cfg, poller.got, "the evm poller must receive the network config")
	})

	t.Run("no alias falls back to the network id", func(t *testing.T) {
		u := bareUpstream(t, &common.UpstreamConfig{Id: "u1"})
		u.SetNetworkConfig(&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 1},
		})
		assert.Equal(t, "evm:1", u.NetworkId())
		assert.Equal(t, "evm:1", u.NetworkLabel(), "an unlabelled network must not stay 'n/a'")
	})

	t.Run("nil config changes nothing", func(t *testing.T) {
		u := bareUpstream(t, &common.UpstreamConfig{Id: "u1"})
		u.SetNetworkConfig(&common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: 1},
		})
		u.SetNetworkConfig(nil)
		assert.Equal(t, "evm:1", u.NetworkId(), "a nil config must not unbind a bound upstream")
	})
}

func TestUpstream_MarshalJSONCarriesTheIdNetworkAndMetrics(t *testing.T) {
	u := bareUpstream(t, &common.UpstreamConfig{Id: "u1"})
	u.networkId.Store("evm:123")
	u.metricsTracker.RecordUpstreamRequest(u, "eth_call", common.DataFinalityStateUnknown)

	raw, err := u.MarshalJSON()
	require.NoError(t, err)
	// The admin health surface renders this. Losing the network id makes every
	// upstream look unassigned.
	assert.Contains(t, string(raw), `"id":"u1"`)
	assert.Contains(t, string(raw), `"networkId":"evm:123"`)
	assert.Contains(t, string(raw), "requestsTotal")
}

func TestUpstream_IgnoreMethodNeedsAutoIgnoreEnabled(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		u := bareUpstream(t, &common.UpstreamConfig{Id: "u1"})
		u.IgnoreMethod("eth_getLogs")

		allowed, err := u.ShouldHandleMethod("eth_getLogs")
		require.NoError(t, err)
		assert.True(t, allowed,
			"an upstream must not silently stop serving a method the operator never excluded")
		assert.Empty(t, u.config.IgnoreMethods)
	})

	t.Run("enabled", func(t *testing.T) {
		on := true
		u := bareUpstream(t, &common.UpstreamConfig{Id: "u1", AutoIgnoreUnsupportedMethods: &on})

		allowed, err := u.ShouldHandleMethod("eth_getLogs")
		require.NoError(t, err)
		require.True(t, allowed, "sanity: the method starts allowed")

		u.IgnoreMethod("eth_getLogs")

		// The decision cache must be overwritten, not merely appended to the
		// config — a stale cached `true` keeps re-sending an unsupported call.
		allowed, err = u.ShouldHandleMethod("eth_getLogs")
		require.NoError(t, err)
		assert.False(t, allowed)
		assert.Contains(t, u.config.IgnoreMethods, "eth_getLogs")

		// Other methods are untouched.
		allowed, err = u.ShouldHandleMethod("eth_call")
		require.NoError(t, err)
		assert.True(t, allowed)
	})
}

func TestUpstream_ShouldHandleMethodLetsAllowOverrideIgnore(t *testing.T) {
	// "ignore everything except eth_getLogs" is the documented pattern, and it
	// only works because allowMethods is evaluated after ignoreMethods.
	u := bareUpstream(t, &common.UpstreamConfig{
		Id:            "u1",
		IgnoreMethods: []string{"*"},
		AllowMethods:  []string{"eth_getLogs"},
	})

	allowed, err := u.ShouldHandleMethod("eth_getLogs")
	require.NoError(t, err)
	assert.True(t, allowed, "an explicit allow must override a wildcard ignore")

	allowed, err = u.ShouldHandleMethod("eth_call")
	require.NoError(t, err)
	assert.False(t, allowed)

	// The second read comes from the decision cache and must agree.
	allowed, err = u.ShouldHandleMethod("eth_call")
	require.NoError(t, err)
	assert.False(t, allowed)
}

func TestUpstream_EvmAccessorsRefuseBeforeThePollerExists(t *testing.T) {
	u := bareUpstream(t, &common.UpstreamConfig{Id: "u1"})

	// Before the poller exists nothing is known. Returning 0 with no error
	// would read as "this node is at block 0" and make it look furthest behind.
	assert.Equal(t, common.EvmSyncingStateUnknown, u.EvmSyncingState())

	_, err := u.EvmLatestBlock()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state poller")

	_, err = u.EvmFinalizedBlock()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state poller")

	_, err = u.EvmIsBlockFinalized(t.Context(), 100, false)
	require.Error(t, err)

	assert.Nil(t, u.EvmStatePoller())
	assert.Nil(t, u.SvmStatePoller())
}

func TestUpstream_EvmAccessorsReadThroughToThePoller(t *testing.T) {
	u := bareUpstream(t, &common.UpstreamConfig{Id: "u1"})
	u.evmStatePoller = &mockEvmStatePoller{latestBlock: 900, finalizedBlock: 800}

	latest, err := u.EvmLatestBlock()
	require.NoError(t, err)
	assert.Equal(t, int64(900), latest)

	finalized, err := u.EvmFinalizedBlock()
	require.NoError(t, err)
	assert.Equal(t, int64(800), finalized)

	assert.Equal(t, common.EvmSyncingStateNotSyncing, u.EvmSyncingState())
	assert.Same(t, u.evmStatePoller, u.EvmStatePoller())
}

func TestUpstream_EvmBlockAvailabilityBoundsReportsTheConfiguredWindow(t *testing.T) {
	// Post-forward, this pair decides whether an empty answer is expected
	// (block outside the node's range) or a misbehaviour worth cordoning for.
	lower := int64(1000)
	u := bareUpstream(t, &common.UpstreamConfig{
		Id: "u1",
		Evm: &common.EvmUpstreamConfig{
			BlockAvailability: &common.EvmBlockAvailabilityConfig{
				Lower: &common.EvmAvailabilityBoundConfig{ExactBlock: &lower},
			},
		},
	})

	minB, maxB := u.EvmBlockAvailabilityBounds()
	assert.Equal(t, int64(1000), minB)
	assert.Greater(t, maxB, int64(1_000_000_000), "an unset upper bound must stay unbounded")
}

func TestUpstream_GuessVendorNameFromTheEndpoint(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		want     string
	}{
		{"https://eth-mainnet.g.alchemy.com/v2/key", "unknown-alchemy.com"},
		// BUG (upstream candidate, upstream/upstream.go:1432): the multi-level
		// TLD guard is `len(rootDomain) < 5`, and "co.uk" is exactly 5, so the
		// escape hatch never fires for the very case its comment names. Every
		// *.co.uk vendor collapses onto one `vendor` label. Pinned as-is —
		// this test asserts today's behaviour, not the intended behaviour.
		{"https://rpc.example.co.uk:8545/", "unknown-co.uk"},
		{"https://rpc.example.io:8545/", "unknown-example.io"},
		{"http://192.168.1.10:8545", "unknown-192.168.1.10"},
		{"http://localhost:8545", "unknown-localhost"},
		{"", ""},
		{"://not a url", ""},
	} {
		u := bareUpstream(t, &common.UpstreamConfig{Id: "u1", Endpoint: tc.endpoint})
		assert.Equal(t, tc.want, u.guessVendorName(), "endpoint %q", tc.endpoint)
	}
}

func TestUpstream_CordonAndUncordonAreVisibleThroughTheUpstream(t *testing.T) {
	u := bareUpstream(t, &common.UpstreamConfig{Id: "u1"})
	u.networkId.Store("evm:123")

	_, cordoned := u.CordonedReason("eth_call")
	require.False(t, cordoned)

	u.Cordon("eth_call", "probe says it is behind")
	reason, cordoned := u.CordonedReason("eth_call")
	require.True(t, cordoned)
	assert.Equal(t, "probe says it is behind", reason)

	// Uncordon is the ONLY way back into rotation — no clock tick clears it.
	u.Uncordon("eth_call", "caught up")
	_, cordoned = u.CordonedReason("eth_call")
	assert.False(t, cordoned, "an uncordoned upstream must take traffic again")
}
