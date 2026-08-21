package policy_test

import (
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// probeRequest builds a read-only JSON-RPC request the prober is allowed
// to mirror.
func probeRequest(method string) *common.NormalizedRequest {
	return common.NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`))
}

// TestEngine_PublishRequest_MirrorsTrafficToAnExcludedUpstream — an
// excluded upstream sees no user traffic, so its metrics freeze and the
// predicate that dropped it can never clear. `probeExcluded` breaks that
// deadlock by shadow-mirroring sampled real requests, and PublishRequest
// is the request path's only entry into it.
func TestEngine_PublishRequest_MirrorsTrafficToAnExcludedUpstream(t *testing.T) {
	f := newEngineFixture(t)
	good := f.upstream("aaa")
	bad := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(), good, bad)

	f.seed(good, seedSpec{requests: 20, latencyMs: 10})
	f.seed(bad, seedSpec{failed: 20})
	f.tick("evm:1", "*")
	require.Equal(t, []string{"bbb"}, f.excludedIDs("evm:1", "*"))

	f.Engine.PublishRequest("evm:1", probeRequest("eth_getBalance"))

	require.True(t, waitUntil(t, 5*time.Second, func() bool {
		return bad.forwards.Load() >= 1
	}), "the excluded upstream must receive the shadow probe")
	require.Zero(t, good.forwards.Load(),
		"an in-rotation upstream is never a probe target")
}

// TestEngine_PublishRequest_NeverMirrorsAWriteMethod — mirroring a
// transaction broadcast would send it twice. The prober refuses any
// method with a write signature.
func TestEngine_PublishRequest_NeverMirrorsAWriteMethod(t *testing.T) {
	f := newEngineFixture(t)
	good := f.upstream("aaa")
	bad := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(), good, bad)

	f.seed(good, seedSpec{requests: 20, latencyMs: 10})
	f.seed(bad, seedSpec{failed: 20})
	f.tick("evm:1", "*")
	require.Equal(t, []string{"bbb"}, f.excludedIDs("evm:1", "*"))

	f.Engine.PublishRequest("evm:1", probeRequest("eth_sendRawTransaction"))
	// A read request published afterwards proves the bus drained, so the
	// zero below is a refusal rather than a race with the dispatcher.
	f.Engine.PublishRequest("evm:1", probeRequest("eth_getBalance"))

	require.True(t, waitUntil(t, 5*time.Second, func() bool {
		return bad.forwards.Load() >= 1
	}), "the read request must still be mirrored")
	require.Equal(t, int64(1), bad.forwards.Load(),
		"only the read request was mirrored; the write was refused")
}

// TestEngine_PublishRequest_AFailedProbeCountsAgainstTheUpstream — the
// probe exists to re-admit an upstream that has healed. A probe that
// fails must therefore be recorded as a failure, or an upstream that is
// still broken would look healthier on every probe and re-admit itself.
func TestEngine_PublishRequest_AFailedProbeCountsAgainstTheUpstream(t *testing.T) {
	f := newEngineFixture(t)
	good := f.upstream("aaa")
	bad := f.upstream("bbb")
	bad.forwardErr = errors.New("probe: upstream still down")
	f.register("evm:1", defaultPolicyConfig(), good, bad)

	f.seed(good, seedSpec{requests: 20, latencyMs: 10})
	f.seed(bad, seedSpec{failed: 20})
	f.tick("evm:1", "*")
	require.Equal(t, []string{"bbb"}, f.excludedIDs("evm:1", "*"))

	before := f.Tracker.GetUpstreamMethodMetrics(bad, "*", common.DataFinalityStateAll).ErrorsTotal.Load()
	f.Engine.PublishRequest("evm:1", probeRequest("eth_getBalance"))

	require.True(t, waitUntil(t, 5*time.Second, func() bool {
		m := f.Tracker.GetUpstreamMethodMetrics(bad, "*", common.DataFinalityStateAll)
		return m != nil && m.ErrorsTotal.Load() > before
	}), "the failed probe must be recorded against the upstream")
}

// TestEngine_PublishRequest_IsANoOpWithoutAProbeStep — a policy that omits
// `probeExcluded` has opted out of shadow traffic. The request path must
// then send nothing at all, even though upstreams are excluded.
func TestEngine_PublishRequest_IsANoOpWithoutAProbeStep(t *testing.T) {
	f := newEngineFixture(t)
	cfg := defaultPolicyConfig()
	cfg.EvalFunc = "(ups, _ctx) => ups.excludeIf(errorRateAbove(0.7))"
	good := f.upstream("aaa")
	bad := f.upstream("bbb")
	f.register("evm:1", cfg, good, bad)

	f.seed(good, seedSpec{requests: 20, latencyMs: 10})
	f.seed(bad, seedSpec{failed: 20})
	f.tick("evm:1", "*")
	require.Equal(t, []string{"bbb"}, f.excludedIDs("evm:1", "*"),
		"the upstream is excluded — it just must not be probed")

	f.Engine.PublishRequest("evm:1", probeRequest("eth_getBalance"))
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, bad.forwards.Load(),
		"no probeExcluded step means no prober, so no mirrored traffic")
}

// TestEngine_PublishRequest_MirrorsOnlyWithinTheNetworkThatServedIt — the
// probers are per network. A request served by one network must never
// reach another network's excluded upstreams: they are on a different
// chain and the response would be meaningless.
//
// The nil request in here is a contract statement, not a claim of
// coverage. `NormalizedRequest.Method` is nil-safe, so removing the
// engine's own nil guard changes nothing observable.
func TestEngine_PublishRequest_MirrorsOnlyWithinTheNetworkThatServedIt(t *testing.T) {
	f := newEngineFixture(t)
	bad := f.upstream("bbb")
	f.register("evm:1", defaultPolicyConfig(), f.upstream("aaa"), bad)
	f.seed(bad, seedSpec{failed: 20})
	f.tick("evm:1", "*")
	require.Equal(t, []string{"bbb"}, f.excludedIDs("evm:1", "*"))

	require.NotPanics(t, func() {
		f.Engine.PublishRequest("evm:1", nil)
		f.Engine.PublishRequest("evm:999", probeRequest("eth_getBalance"))
	})
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, bad.forwards.Load(),
		"a request published against another network must not be mirrored here")

	// Control: the same request on the right network IS mirrored, so the
	// zero above is a refusal rather than a broken fixture.
	f.Engine.PublishRequest("evm:1", probeRequest("eth_getBalance"))
	require.True(t, waitUntil(t, 5*time.Second, func() bool {
		return bad.forwards.Load() >= 1
	}), "the control request must be mirrored")
}
