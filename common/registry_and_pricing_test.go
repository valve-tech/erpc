package common

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// --- chain-family registry helpers ---

// probeFamily is a family that can build its own probe transport. The optional
// ProbeTransportFactory surface is what decides whether an upstream of this
// family can be health-checked at all.
type probeFamily struct {
	fakeFamily
	built  int
	caller ProbeCaller
}

func (p *probeFamily) NewProbeCaller(endpoint string, client *http.Client) ProbeCaller {
	p.built++
	return p.caller
}

// schemeGateFamily refuses one URL scheme and explains why.
type schemeGateFamily struct {
	fakeFamily
}

func (s *schemeGateFamily) SupportsEndpointScheme(scheme string) (bool, string) {
	if scheme == "ws" || scheme == "wss" {
		return false, "the svm path has never been run against the websocket client"
	}
	return true, ""
}

// stubProbeCaller is a ProbeCaller that records nothing; it only needs to be a
// distinct object the test can compare against.
type stubProbeCaller struct{}

func (stubProbeCaller) CallJsonRpc(context.Context, string, []interface{}) ([]byte, error) {
	return nil, nil
}
func (stubProbeCaller) CallREST(context.Context, string, string) (int, []byte, error) {
	return 0, nil, nil
}

// An upstream's `type:` names its architecture. Resolving it through the
// registry is what keeps a new chain from needing a switch statement here.
func TestLookupChainFamilyForUpstreamType_ResolvesThroughTheRegistry(t *testing.T) {
	f := &fakeFamily{name: "testfam", transport: TransportJsonRpc}
	registerForTest(t, f)

	got, ok := LookupChainFamilyForUpstreamType(UpstreamType("testfam"))
	require.True(t, ok)
	require.Same(t, f, got)

	// A type nobody registered must report "not found", not a zero family the
	// caller would then use to route traffic.
	_, ok = LookupChainFamilyForUpstreamType(UpstreamType("nosuchfam"))
	require.False(t, ok)

	_, ok = LookupChainFamilyForUpstreamType("")
	require.False(t, ok)
}

// Without a probe transport there is no tip and no way to tell a synced node
// from a stalled one, so bootstrap must be able to detect the absence.
func TestNewFamilyProbeCaller_ReportsWhetherTheFamilyCanBeProbed(t *testing.T) {
	plain := &fakeFamily{name: "plainfam", transport: TransportJsonRpc}
	caller, ok := NewFamilyProbeCaller(plain, "https://node.example.com", http.DefaultClient)
	require.False(t, ok, "a family with no probe transport must say so")
	require.Nil(t, caller)

	stub := stubProbeCaller{}
	withProbe := &probeFamily{
		fakeFamily: fakeFamily{name: "probefam", transport: TransportJsonRpc},
		caller:     stub,
	}
	caller, ok = NewFamilyProbeCaller(withProbe, "https://node.example.com", http.DefaultClient)
	require.True(t, ok)
	require.Equal(t, stub, caller, "the family's own caller must be returned, not a generic one")
	require.Equal(t, 1, withProbe.built, "the factory must actually be called")
}

// A family that implements no scheme gate allows everything the client factory
// knows. That is the unknown-case default, so it is asserted first.
func TestEndpointSchemeSupported_DefaultsToAllowingEveryScheme(t *testing.T) {
	plain := &fakeFamily{name: "plainfam", transport: TransportJsonRpc}

	for _, scheme := range []string{"http", "https", "ws", "wss", "grpc", "somethingnew"} {
		ok, reason := EndpointSchemeSupported(plain, scheme)
		require.True(t, ok, "scheme %q must be allowed by default", scheme)
		require.Equal(t, "", reason)
	}
}

// When a family does gate schemes, the refusal must carry an operator-facing
// reason: a bare "false" leaves the operator guessing why their upstream was
// rejected at boot.
func TestEndpointSchemeSupported_RefusalCarriesAReason(t *testing.T) {
	gated := &schemeGateFamily{fakeFamily{name: "gatedfam", transport: TransportJsonRpc}}

	ok, reason := EndpointSchemeSupported(gated, "wss")
	require.False(t, ok)
	require.NotEmpty(t, reason, "a refusal must explain itself")
	require.Contains(t, reason, "websocket")

	ok, reason = EndpointSchemeSupported(gated, "https")
	require.True(t, ok)
	require.Equal(t, "", reason)
}

// --- network id validation ---

// IsValidNetwork is the gate the URL router and the provider filters run. A
// shape it wrongly accepts becomes a network eRPC tries to build and 404s on
// with nothing to explain it.
func TestIsValidNetwork_EvmRequiresAPositiveChainId(t *testing.T) {
	require.True(t, IsValidNetwork("evm:1"))
	require.True(t, IsValidNetwork("evm:42161"))

	require.False(t, IsValidNetwork("evm:0"), "chain id 0 is not a chain")
	require.False(t, IsValidNetwork("evm:-1"))
	require.False(t, IsValidNetwork("evm:mainnet"))
	require.False(t, IsValidNetwork("evm:"))
	require.False(t, IsValidNetwork("evm:1.5"))
	require.False(t, IsValidNetwork("evm:0x1"))
}

func TestIsValidNetwork_SvmAcceptsOneOrTwoSegments(t *testing.T) {
	// Back-compat single-segment shape.
	require.True(t, IsValidNetwork("svm:mainnet-beta"))
	require.True(t, IsValidNetwork("svm:devnet"))
	// Two-segment chain:cluster shape.
	require.True(t, IsValidNetwork("svm:solana:mainnet-beta"))
	// An unknown private chain must still pass — operators run their own.
	require.True(t, IsValidNetwork("svm:my.chain:my_cluster-1"))

	require.False(t, IsValidNetwork("svm:"), "an empty body names no cluster")
	require.False(t, IsValidNetwork("svm:solana:"), "an empty cluster names nothing")
	require.False(t, IsValidNetwork("svm:a:b:c"), "three segments are not a shape eRPC serves")
	require.False(t, IsValidNetwork("svm:main net"), "a space cannot appear in a URL path segment")
	require.False(t, IsValidNetwork("svm:main/net"))
}

func TestIsValidNetwork_UnknownPrefixIsRefused(t *testing.T) {
	require.False(t, IsValidNetwork(""))
	require.False(t, IsValidNetwork("evm"))
	require.False(t, IsValidNetwork("btc:mainnet"), "a family that is not registered here must not validate")
	require.False(t, IsValidNetwork("1"))
}

// --- credit-unit pricing ---

// Credit units drive the X-ERPC-Credits header an operator bills against. The
// precedence below is the whole contract: an operator override must beat the
// vendor's published table, and an unpriced method must cost something rather
// than silently free.
func TestResolveCreditUnits_OverrideBeatsDefaultsBeatsWildcard(t *testing.T) {
	defaults := map[string]int64{"eth_call": 26, "eth_getLogs": 75, "*": 10}
	override := map[string]int64{"eth_call": 5, "*": 2}

	// 1. An exact override wins over everything.
	require.Equal(t, int64(5), ResolveCreditUnits(defaults, override, "eth_call"))

	// 2. With no exact override, the vendor's exact default wins over both
	//    wildcards — an override wildcard must not silently reprice a method
	//    the vendor prices explicitly.
	require.Equal(t, int64(75), ResolveCreditUnits(defaults, override, "eth_getLogs"))

	// 3. With neither exact entry, the override wildcard wins.
	require.Equal(t, int64(2), ResolveCreditUnits(defaults, override, "eth_blockNumber"))

	// 4. With no override at all, the defaults wildcard applies.
	require.Equal(t, int64(10), ResolveCreditUnits(defaults, nil, "eth_blockNumber"))

	// 5. With no table at all, a call still costs one unit. Returning 0 would
	//    make an unpriced vendor look free.
	require.Equal(t, int64(1), ResolveCreditUnits(nil, nil, "eth_blockNumber"))
	require.Equal(t, int64(1), ResolveCreditUnits(map[string]int64{"eth_call": 26}, nil, "eth_blockNumber"))
}

// `creditUnits: {"*": 0}` is the documented way to opt a vendor out. A zero
// must therefore be honoured as a real price, not read as "unset".
func TestResolveCreditUnits_ExplicitZeroOptsOut(t *testing.T) {
	require.Equal(t, int64(0), ResolveCreditUnits(nil, map[string]int64{"*": 0}, "eth_call"))
	require.Equal(t, int64(0), ResolveCreditUnits(map[string]int64{"*": 10}, map[string]int64{"*": 0}, "eth_call"))
	require.Equal(t, int64(0), ResolveCreditUnits(map[string]int64{"eth_call": 0}, nil, "eth_call"))
}

// --- timeout function ---

// timeoutTestNetwork is the smallest Network a TimeoutFunc needs.
type timeoutTestNetwork struct {
	metrics TrackedMetrics
}

func (n *timeoutTestNetwork) Id() string                        { return "evm:1" }
func (n *timeoutTestNetwork) Label() string                     { return "evm:1" }
func (n *timeoutTestNetwork) ProjectId() string                 { return "p" }
func (n *timeoutTestNetwork) Architecture() NetworkArchitecture { return ArchitectureEvm }
func (n *timeoutTestNetwork) Config() *NetworkConfig            { return nil }
func (n *timeoutTestNetwork) Logger() *zerolog.Logger           { lg := zerolog.Nop(); return &lg }
func (n *timeoutTestNetwork) GetMethodMetrics(string) TrackedMetrics {
	return n.metrics
}
func (n *timeoutTestNetwork) Forward(context.Context, *NormalizedRequest) (*NormalizedResponse, error) {
	return nil, nil
}
func (n *timeoutTestNetwork) GetFinality(context.Context, *NormalizedRequest, *NormalizedResponse) DataFinalityState {
	return DataFinalityStateRealtime
}

// fixedQuantiles answers one duration for every quantile, so the test controls
// the adaptive result exactly instead of depending on collected latency.
type fixedQuantiles struct{ d time.Duration }

func (f *fixedQuantiles) Add(float64)                       {}
func (f *fixedQuantiles) GetQuantile(float64) time.Duration { return f.d }
func (f *fixedQuantiles) Reset()                            {}

type fixedMetrics struct{ q QuantileTracker }

func (m *fixedMetrics) ErrorRate() float64                    { return 0 }
func (m *fixedMetrics) GetResponseQuantiles() QuantileTracker { return m.q }

func timeoutTestRequest(t *testing.T, ntw Network) *NormalizedRequest {
	t.Helper()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	if ntw != nil {
		req.SetNetwork(ntw)
	}
	return req
}

// No timeout configured must mean no timeout function — the caller then skips
// context.WithTimeout entirely rather than applying a zero deadline that would
// fail every request instantly.
func TestNewTimeoutFunc_NilWhenNothingIsConfigured(t *testing.T) {
	lg := zerolog.Nop()

	require.Nil(t, NewTimeoutFunc(&lg, nil))
	require.Nil(t, NewTimeoutFunc(&lg, &TimeoutPolicyConfig{}))
	require.Nil(t, NewTimeoutFunc(&lg, &TimeoutPolicyConfig{Duration: &AdaptiveDuration{}}))
}

func TestNewTimeoutFunc_StaticDurationIsReturnedUnchanged(t *testing.T) {
	lg := zerolog.Nop()
	fn := NewTimeoutFunc(&lg, &TimeoutPolicyConfig{
		Duration: &AdaptiveDuration{Base: Duration(3 * time.Second)},
	})
	require.NotNil(t, fn)

	// A static timeout must not depend on the request at all, including a nil
	// network — that is the cold-start path on the very first request.
	got := fn(context.Background(), timeoutTestRequest(t, nil))
	require.NotNil(t, got)
	require.Equal(t, 3*time.Second, *got)
}

// With a quantile configured, the timeout is the observed latency PLUS Base as
// headroom, clamped by Max. Dropping the headroom would time out roughly half
// the requests that are merely at the quantile.
func TestNewTimeoutFunc_QuantileAddsHeadroomAndRespectsMax(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lg := zerolog.Nop()

	fn := NewTimeoutFunc(&lg, &TimeoutPolicyConfig{
		Duration: &AdaptiveDuration{
			Base:     Duration(1 * time.Second),
			Quantile: 0.99,
			Max:      Duration(5 * time.Second),
		},
	})
	require.NotNil(t, fn)

	ntw := &timeoutTestNetwork{metrics: &fixedMetrics{q: &fixedQuantiles{d: 2 * time.Second}}}
	got := fn(ctx, timeoutTestRequest(t, ntw))
	require.NotNil(t, got)
	require.Equal(t, 3*time.Second, *got, "observed latency plus the Base headroom")

	// One pathological upstream must not push the timeout past the operator's
	// ceiling — that is what Max exists for.
	ntw = &timeoutTestNetwork{metrics: &fixedMetrics{q: &fixedQuantiles{d: 30 * time.Second}}}
	got = fn(ctx, timeoutTestRequest(t, ntw))
	require.NotNil(t, got)
	require.Equal(t, 5*time.Second, *got, "Max must cap the adaptive timeout")
}

// When the tracker has no data yet, the adaptive component falls back to the
// auto-floor (Base/2) instead of zero. Without the floor a short timeout
// produces short success latencies, which produce an even shorter timeout — the
// feedback loop that collapsed a policy to 50ms.
func TestNewTimeoutFunc_AutoFloorStopsTheTimeoutCollapsing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lg := zerolog.Nop()

	fn := NewTimeoutFunc(&lg, &TimeoutPolicyConfig{
		Duration: &AdaptiveDuration{Base: Duration(4 * time.Second), Quantile: 0.99},
	})
	require.NotNil(t, fn)

	// GetQuantile returns 0 — no latency collected for this method yet.
	ntw := &timeoutTestNetwork{metrics: &fixedMetrics{q: &fixedQuantiles{d: 0}}}
	got := fn(ctx, timeoutTestRequest(t, ntw))
	require.NotNil(t, got)
	require.Equal(t, 6*time.Second, *got, "Base plus the auto-floor of Base/2")

	// An explicit Min overrides the auto-floor, so an operator keeps control.
	fnMin := NewTimeoutFunc(&lg, &TimeoutPolicyConfig{
		Duration: &AdaptiveDuration{
			Base:     Duration(4 * time.Second),
			Quantile: 0.99,
			Min:      Duration(1 * time.Second),
		},
	})
	got = fnMin(ctx, timeoutTestRequest(t, ntw))
	require.NotNil(t, got)
	require.Equal(t, 5*time.Second, *got, "Base plus the operator's own Min")
}

// Before any latency has been collected the function must fall back to the
// static value rather than return nothing, which would leave the request with
// no deadline at all.
func TestNewTimeoutFunc_ColdStartFallsBackToTheStaticValue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lg := zerolog.Nop()

	fn := NewTimeoutFunc(&lg, &TimeoutPolicyConfig{
		Duration: &AdaptiveDuration{Base: Duration(4 * time.Second), Quantile: 0.99},
	})

	t.Run("no network on the request", func(t *testing.T) {
		got := fn(ctx, timeoutTestRequest(t, nil))
		require.NotNil(t, got)
		require.Equal(t, 4*time.Second, *got)
	})

	t.Run("network reports no metrics tracker", func(t *testing.T) {
		got := fn(ctx, timeoutTestRequest(t, &timeoutTestNetwork{metrics: nil}))
		require.NotNil(t, got)
		require.Equal(t, 4*time.Second, *got)
	})

	t.Run("request carries no method", func(t *testing.T) {
		// An unparseable body leaves Method() empty, so there is no per-method
		// tracker to consult.
		req := NewNormalizedRequest([]byte(`not json`))
		req.SetNetwork(&timeoutTestNetwork{metrics: &fixedMetrics{q: &fixedQuantiles{d: time.Second}}})
		got := fn(ctx, req)
		require.NotNil(t, got)
		require.Equal(t, 4*time.Second, *got)
	})
}

// With a quantile but no Base, Max is the fallback and the auto-floor is a
// fixed 500ms. A nil result here would mean "no deadline", which is the one
// outcome a timeout policy must never produce.
func TestNewTimeoutFunc_QuantileWithoutBaseUsesMaxAndTheFixedFloor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	lg := zerolog.Nop()

	fn := NewTimeoutFunc(&lg, &TimeoutPolicyConfig{
		Duration: &AdaptiveDuration{Quantile: 0.99, Max: Duration(8 * time.Second)},
	})
	require.NotNil(t, fn)

	// Cold start: no network, so the fallback is Max.
	got := fn(ctx, timeoutTestRequest(t, nil))
	require.NotNil(t, got)
	require.Equal(t, 8*time.Second, *got)

	// A tiny observed latency is floored at 500ms.
	ntw := &timeoutTestNetwork{metrics: &fixedMetrics{q: &fixedQuantiles{d: 10 * time.Millisecond}}}
	got = fn(ctx, timeoutTestRequest(t, ntw))
	require.NotNil(t, got)
	require.Equal(t, 500*time.Millisecond, *got)
}

// --- misc helpers ---

func TestRemoveDuplicates_KeepsFirstOccurrenceOrder(t *testing.T) {
	require.Equal(t, []string{"a", "b", "c"},
		RemoveDuplicates([]string{"a", "b", "a", "c", "b", "a"}))
	require.Equal(t, []string{}, RemoveDuplicates(nil))
	require.Equal(t, []string{""}, RemoveDuplicates([]string{"", ""}),
		"an empty string is a value, not an absence")
}
