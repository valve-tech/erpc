package policy_test

import (
	"bytes"
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/health"
	"github.com/erpc/erpc/internal/policy"
	policystdlib "github.com/erpc/erpc/internal/policy/stdlib"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// This file is the ticked multi-slot engine fixture for `internal/policy`.
//
// `erpc/networks_selection_policy_test.go` drives the same engine, but it
// pays for a full Network: an upstream registry, a shared-state store, a
// vendor registry and a gock HTTP mock per upstream. That fixture answers
// "does routing work end to end". This one answers "what does the engine
// decide, and what does it report afterwards", so it drops every layer
// above the engine.
//
// How to reuse it:
//
//	f := newEngineFixture(t)                          // engine + tracker + logs
//	a := f.upstream("aaa")                            // a fake common.Upstream
//	b := f.upstream("bbb", "tier:fallback")
//	f.register("evm:1", defaultPolicyConfig(), a, b)  // registers and ticks once
//	f.seed(a, seedSpec{failed: 20})                   // error rate 1.0 over 20 samples
//	f.tick("evm:1", "*")                              // one synchronous eval
//	f.orderIDs("evm:1", "*")                          // read the verdict
//
// `seedSpec` counts are cumulative and `errorRate` is `failed / (requests
// + failed)`. `seedSpec{requests: 20, failed: 20}` is a 0.5 error rate,
// which the default policy's `errorRateAbove(0.7)` does NOT drop.
//
// Every tick is explicit. `defaultPolicyConfig` sets `DisableTickerForTest`
// so no background goroutine competes with the test, and `EvalInterval` is
// zero so a slot that escapes that flag still never self-ticks. A test that
// needs the real ticker calls `tickingPolicyConfig(interval)` instead and
// bounds its own wait.
//
// The fixture registers a `t.Cleanup` that stops the engine, so a test
// never leaks a slot goroutine or a prober into the next one.

// fixtureUpstream is a `common.Upstream` with no I/O. `Forward` counts
// calls, which is how the probe tests observe that `PublishRequest`
// reached a real upstream.
type fixtureUpstream struct {
	id        string
	networkID string
	tags      []string
	routing   *common.UpstreamRoutingConfig
	forwards  atomic.Int64
	// forwardErr, when set, is what Forward returns. The probe path
	// records it as an upstream failure.
	forwardErr error
}

func (f *fixtureUpstream) Id() string           { return f.id }
func (f *fixtureUpstream) VendorName() string   { return "vendor-" + f.id }
func (f *fixtureUpstream) NetworkId() string    { return f.networkID }
func (f *fixtureUpstream) NetworkLabel() string { return f.networkID }
func (f *fixtureUpstream) Config() *common.UpstreamConfig {
	return &common.UpstreamConfig{Id: f.id, Tags: f.tags, Routing: f.routing}
}
func (f *fixtureUpstream) Logger() *zerolog.Logger { l := zerolog.Nop(); return &l }
func (f *fixtureUpstream) Vendor() common.Vendor   { return nil }
func (f *fixtureUpstream) Tracker() common.HealthTracker {
	return nil
}
func (f *fixtureUpstream) Forward(ctx context.Context, nq *common.NormalizedRequest, byPassMethodExclusion, isHedgeAttempt bool) (*common.NormalizedResponse, error) {
	f.forwards.Add(1)
	return nil, f.forwardErr
}
func (f *fixtureUpstream) Cordon(method, reason string)                   {}
func (f *fixtureUpstream) Uncordon(method, reason string)                 {}
func (f *fixtureUpstream) IgnoreMethod(method string)                     {}
func (f *fixtureUpstream) ShouldHandleMethod(method string) (bool, error) { return true, nil }

// engineFixture owns one engine, its tracker and its log sink.
type engineFixture struct {
	t       *testing.T
	Engine  *policy.Engine
	Tracker *health.Tracker
	Logs    *bytes.Buffer
}

// newEngineFixture builds an engine with the real stdlib installed. The
// stdlib matters: without the primer the bundled default policy cannot
// resolve `excludeIf`, `preferTag` or `stickyPrimary`.
func newEngineFixture(t *testing.T) *engineFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	buf := &bytes.Buffer{}
	logger := zerolog.New(buf).Level(zerolog.DebugLevel)
	tracker := health.NewTracker(&logger, "p1", time.Minute)

	engine := policy.NewEngine(ctx, &logger, "p1", tracker, policystdlib.Install, nil)
	t.Cleanup(engine.Stop)

	return &engineFixture{t: t, Engine: engine, Tracker: tracker, Logs: buf}
}

// upstream builds a fake upstream. Tags land on `UpstreamConfig.Tags`, so
// `tier:fallback` reaches the JS as `u.tags`.
func (f *engineFixture) upstream(id string, tags ...string) *fixtureUpstream {
	return &fixtureUpstream{id: id, tags: tags}
}

// upstreamOn is `upstream` with the network stamped up front. Use it when
// the test has to seed the tracker BEFORE registering the network — the
// register call runs a synchronous first tick, so metrics seeded after it
// are already one tick late.
func (f *engineFixture) upstreamOn(networkID, id string, tags ...string) *fixtureUpstream {
	return &fixtureUpstream{id: id, networkID: networkID, tags: tags}
}

// register registers a network and runs its first synchronous tick. The
// upstreams' `networkID` is stamped here so the tracker's per-network
// bookkeeping agrees with the slot key.
func (f *engineFixture) register(networkID string, cfg *common.SelectionPolicyConfig, ups ...*fixtureUpstream) []common.Upstream {
	f.t.Helper()
	require.NoError(f.t, cfg.SetDefaults())
	require.NoError(f.t, cfg.Validate())
	list := make([]common.Upstream, 0, len(ups))
	for _, u := range ups {
		u.networkID = networkID
		list = append(list, u)
	}
	require.NoError(f.t, f.Engine.RegisterNetwork(networkID, "", func() []common.Upstream { return list }, cfg))
	return list
}

// tick runs exactly one eval on the (network, method) slot.
func (f *engineFixture) tick(networkID, method string) {
	policy.TickForTest(f.Engine, networkID, method)
}

// orderIDs reads the slot's current verdict as upstream ids.
func (f *engineFixture) orderIDs(networkID, method string) []string {
	return idsOf(f.Engine.GetOrdered(networkID, method, "*"))
}

// excludedIDs reads the slot's probe-eligible excluded set as ids.
func (f *engineFixture) excludedIDs(networkID, method string) []string {
	return idsOf(f.Engine.GetExcluded(networkID, method, "*"))
}

// tickAt, orderIDsAt and excludedIDsAt are the finality-aware siblings of
// tick, orderIDs and excludedIDs. Use them when the network runs
// per-finality slots.
func (f *engineFixture) tickAt(networkID, method, finality string) {
	policy.TickForTestAtScope(f.Engine, networkID, method, finality)
}

func (f *engineFixture) orderIDsAt(networkID, method, finality string) []string {
	return idsOf(f.Engine.GetOrdered(networkID, method, finality))
}

func (f *engineFixture) excludedIDsAt(networkID, method, finality string) []string {
	return idsOf(f.Engine.GetExcluded(networkID, method, finality))
}

// lastDecision returns the most recent decision the slot produced.
func (f *engineFixture) lastDecision(networkID, method string) *policy.Decision {
	f.t.Helper()
	all := f.Engine.RecentDecisions(networkID, method, "*", 0)
	require.NotEmpty(f.t, all, "slot %s/%s has produced no decision", networkID, method)
	return all[len(all)-1]
}

func idsOf(ups []common.Upstream) []string {
	out := make([]string, 0, len(ups))
	for _, u := range ups {
		out = append(out, u.Id())
	}
	return out
}

// seedSpec describes the traffic the tracker should believe it observed.
// It mirrors the shape `erpc/networks_selection_policy_test.go` uses, so a
// scenario can move between the two fixtures unchanged.
type seedSpec struct {
	// method is the tracker bucket. Empty means "*", the wildcard the
	// network-scope slot reads.
	method string
	// finality names the tracker's finality bucket, using the same
	// strings the slot key uses: "realtime", "unfinalized", "finalized"
	// or "unknown". Empty means "unknown". Set it only when the network
	// runs per-finality slots.
	finality string
	// requests is the number of successful requests, each recorded at
	// `latencyMs`.
	requests  int
	latencyMs int
	// failed adds requests that also record a failure, which drives
	// `errorRate`.
	failed int
	// throttled adds requests that record a remote rate limit, which
	// drives `throttledRate`.
	throttled int
	// blockHeadLag is written straight onto the tracker bucket, the way
	// the state poller would.
	blockHeadLag int64
}

// seed drives the tracker through its public Record* API — the same path
// real traffic takes.
func (f *engineFixture) seed(u *fixtureUpstream, s seedSpec) {
	f.t.Helper()
	method := s.method
	if method == "" {
		method = "*"
	}
	fin := finalityState(s.finality)
	for i := 0; i < s.requests; i++ {
		f.Tracker.RecordUpstreamRequest(u, method, fin)
		f.Tracker.RecordUpstreamDuration(u, method,
			time.Duration(s.latencyMs)*time.Millisecond,
			true, "none", fin, "n/a")
	}
	for i := 0; i < s.failed; i++ {
		f.Tracker.RecordUpstreamRequest(u, method, fin)
		f.Tracker.RecordUpstreamFailure(u, method, fin, fmt.Errorf("seed: synthetic failure"))
	}
	for i := 0; i < s.throttled; i++ {
		f.Tracker.RecordUpstreamRequest(u, method, fin)
		f.Tracker.RecordUpstreamRemoteRateLimited(context.Background(), u, method, nil)
	}
	if s.blockHeadLag > 0 {
		m := f.Tracker.GetUpstreamMethodMetrics(u, method, common.DataFinalityStateAll)
		require.NotNil(f.t, m, "tracker has no bucket for %s/%s", u.Id(), method)
		m.BlockHeadLag.Store(s.blockHeadLag)
		if method != "*" {
			if agg := f.Tracker.GetUpstreamMethodMetrics(u, "*", common.DataFinalityStateAll); agg != nil {
				agg.BlockHeadLag.Store(s.blockHeadLag)
			}
		}
	}
}

// finalityState maps the slot-key spelling of a finality onto the
// tracker's enum. It mirrors the engine's own `parseFinality`, so a test
// seeds the exact bucket the slot will read.
func finalityState(s string) common.DataFinalityState {
	switch s {
	case "realtime":
		return common.DataFinalityStateRealtime
	case "unfinalized":
		return common.DataFinalityStateUnfinalized
	case "finalized":
		return common.DataFinalityStateFinalized
	default:
		return common.DataFinalityStateUnknown
	}
}

// defaultPolicyConfig is the bundled default policy on a frozen ticker.
// `EvalFunc` is the placeholder the engine upgrades to `default_policy.js`
// at register time, so a test that changes the JS changes what this runs.
func defaultPolicyConfig(opts ...func(*common.SelectionPolicyConfig)) *common.SelectionPolicyConfig {
	cfg := &common.SelectionPolicyConfig{
		EvalInterval: common.Duration(0),
		// Generous: the whole default chain runs per tick and the race
		// detector slows the sobek interpreter down a lot. A real hang
		// still fails, it just takes five seconds to say so.
		EvalTimeout:          common.Duration(5 * time.Second),
		EvalFunc:             common.DefaultSelectionPolicySource,
		DisableTickerForTest: true,
	}
	for _, o := range opts {
		o(cfg)
	}
	return cfg
}

// tickingPolicyConfig runs a trivial pass-through eval on a REAL ticker.
// Only the pause test needs it; everything else drives ticks explicitly.
// The eval is trivial on purpose — the pause gate lives in the slot's
// ticker loop, above the policy.
//
// The timeout takes 80% of the interval, which is the largest value
// `Validate` accepts. Keep the interval well above the ~20 ms the stdlib
// primer costs on a first tick under `-race`: a tick that overruns its
// timeout hits the eval-timeout data race in `slot.go` (see the upstream
// bug log) and turns this test into a flake that is not about pausing.
func tickingPolicyConfig(interval time.Duration) *common.SelectionPolicyConfig {
	return &common.SelectionPolicyConfig{
		EvalInterval: common.Duration(interval),
		EvalTimeout:  common.Duration(interval * 4 / 5),
		EvalFunc:     "(ups, _ctx) => ups",
	}
}

// perMethod switches the network to one slot per method.
func perMethod(cfg *common.SelectionPolicyConfig) {
	cfg.EvalScope = common.EvalScopeNetworkMethod
}

// perFinality switches the network to one slot per finality bucket.
func perFinality(cfg *common.SelectionPolicyConfig) {
	cfg.EvalScope = common.EvalScopeNetworkFinality
}
