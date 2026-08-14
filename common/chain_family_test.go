package common

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/util"
)

// These tests pin the ChainFamily CONTRACT, not any architecture switch.
// Deliberate: the `case common.Architecture*` sites across the tree are the
// thing the polyglot work will refactor, so asserting on them would guarantee
// churn. A fake family exercises the seam a real one plugs into, and stays
// valid however the call sites are rewired.

// fakeFamily is a ChainFamily whose every answer is settable, so a test can
// produce the exact case it wants to assert on.
type fakeFamily struct {
	name      NetworkArchitecture
	transport ChainTransport
	probe     ChainProbe
	verdict   RotateVerdict
	probeCall func(ProbeCaller) // records that Probe actually used the caller
	// validId decides which network-id bodies this family owns. nil accepts
	// everything, so tests that do not care about ids stay short.
	validId func(body string) bool
}

func (f *fakeFamily) Family() NetworkArchitecture { return f.name }
func (f *fakeFamily) Transport() ChainTransport   { return f.transport }
func (f *fakeFamily) Probe(_ context.Context, c ProbeCaller) ChainProbe {
	if f.probeCall != nil {
		f.probeCall(c)
	}
	return f.probe
}
func (f *fakeFamily) Classify(ClassifyInput) RotateVerdict { return f.verdict }
func (f *fakeFamily) ValidateNetworkId(body string) bool {
	if f.validId == nil {
		return true
	}
	return f.validId(body)
}

// registerForTest registers f and removes it when the test ends. The registry
// is process-global; without the cleanup one test leaks into the next.
func registerForTest(t *testing.T, f ChainFamily) {
	t.Helper()
	if err := RegisterChainFamily(f); err != nil {
		t.Fatalf("RegisterChainFamily(%s): %v", f.Family(), err)
	}
	t.Cleanup(func() { UnregisterChainFamilyForTest(f.Family()) })
}

func TestRegisterChainFamily_RoundTrip(t *testing.T) {
	f := &fakeFamily{name: "testfam", transport: TransportJsonRpc}
	registerForTest(t, f)

	got, ok := LookupChainFamily("testfam")
	if !ok {
		t.Fatal("LookupChainFamily: family not found after registration")
	}
	if got.Family() != "testfam" {
		t.Fatalf("Family() = %q, want %q", got.Family(), "testfam")
	}
}

func TestLookupChainFamily_UnregisteredIsNotFound(t *testing.T) {
	// Negative control. Without this, a Lookup that returned a zero-value
	// family for everything would pass the round-trip test above.
	if _, ok := LookupChainFamily("definitely-not-registered"); ok {
		t.Fatal("LookupChainFamily returned ok for an unregistered architecture")
	}
}

func TestRegisterChainFamily_RejectsDuplicate(t *testing.T) {
	registerForTest(t, &fakeFamily{name: "dupfam", transport: TransportJsonRpc})

	err := RegisterChainFamily(&fakeFamily{name: "dupfam", transport: TransportJsonRpc})
	if err == nil {
		t.Fatal("second registration of the same family succeeded; a silent " +
			"overwrite would let one architecture shadow another at init time")
	}
}

func TestRegisterChainFamily_RejectsNilAndUnnamed(t *testing.T) {
	if err := RegisterChainFamily(nil); err == nil {
		t.Error("registering a nil family succeeded")
	}
	if err := RegisterChainFamily(&fakeFamily{name: "", transport: TransportJsonRpc}); err == nil {
		t.Error("registering a family with an empty name succeeded; it would be " +
			"unreachable by lookup and would collide with the next unnamed family")
	}
}

func TestRegisterChainFamily_RejectsRESTTransport(t *testing.T) {
	// The beacon case. eRPC's NormalizedRequest carries a body and nothing
	// else — no verb, no path — so a REST family cannot be served. Registering
	// one must fail loudly rather than resolve and then drop every request.
	err := RegisterChainFamily(&fakeFamily{name: "beaconish", transport: TransportREST})
	if err == nil {
		UnregisterChainFamilyForTest("beaconish")
		t.Fatal("registering a REST-transport family succeeded; it would resolve " +
			"but silently fail to serve")
	}
	if _, ok := LookupChainFamily("beaconish"); ok {
		t.Fatal("a rejected family was still added to the registry")
	}
}

func TestRegisteredChainFamilies_SortedAndComplete(t *testing.T) {
	registerForTest(t, &fakeFamily{name: "zeta", transport: TransportJsonRpc})
	registerForTest(t, &fakeFamily{name: "alpha", transport: TransportJsonRpc})

	var sawAlpha, sawZeta bool
	var prev NetworkArchitecture
	for _, n := range RegisteredChainFamilies() {
		if n < prev {
			t.Fatalf("RegisteredChainFamilies not sorted: %q came after %q", n, prev)
		}
		prev = n
		switch n {
		case "alpha":
			sawAlpha = true
		case "zeta":
			sawZeta = true
		}
	}
	if !sawAlpha || !sawZeta {
		t.Fatalf("registered families missing from listing (alpha=%v zeta=%v)", sawAlpha, sawZeta)
	}
}

func TestRegisterChainFamily_TeachesUtilTheNetworkIdShape(t *testing.T) {
	// Registration must reach util too. util.IsValidNetworkId is the gate the
	// networks registry runs BEFORE it looks at any config, so a family that
	// registers here but not there is probeable and unroutable at the same
	// time — the request 404s and nothing explains why.
	registerForTest(t, &fakeFamily{
		name:      "shapefam",
		transport: TransportJsonRpc,
		validId:   func(body string) bool { return body == "mainnet" },
	})

	if !util.IsValidNetworkId("shapefam:mainnet") {
		t.Fatal("a registered family's network id is not valid in util; the " +
			"request path rejects the id before any config is read")
	}
	if util.IsValidNetworkId("shapefam:bogus") {
		t.Fatal("util accepted a body the family rejects; the family owns its " +
			"own id shape or it owns nothing")
	}
}

func TestUnregisterChainFamily_AlsoRemovesTheNetworkIdShape(t *testing.T) {
	// The two registries are written together, so they must be cleared
	// together. A leftover shape would keep validating ids for a family that
	// no longer resolves — and in tests it would leak across cases.
	f := &fakeFamily{name: "leakyfam", transport: TransportJsonRpc}
	if err := RegisterChainFamily(f); err != nil {
		t.Fatalf("RegisterChainFamily: %v", err)
	}
	UnregisterChainFamilyForTest("leakyfam")

	if util.IsValidNetworkId("leakyfam:anything") {
		t.Fatal("the id shape outlived the family registration")
	}
}

func TestRegisterChainFamily_RejectedFamilyLeavesNoIdShape(t *testing.T) {
	// A REST family is refused (see below). If the id shape were written
	// first, a family eRPC cannot serve would still make its ids look valid,
	// and requests for it would route to a network with no clients.
	if err := RegisterChainFamily(&fakeFamily{name: "restfam", transport: TransportREST}); err == nil {
		UnregisterChainFamilyForTest("restfam")
		t.Fatal("REST family registration succeeded")
	}
	if util.IsValidNetworkId("restfam:mainnet") {
		t.Fatal("a rejected family still taught util its id shape")
	}
}

func TestIsValidArchitecture_AnswersFromTheRegistry(t *testing.T) {
	// The URL gate. It used to name evm and svm in a switch, so a chain could
	// be registered, probeable and still unreachable at /<project>/<arch>/…
	// with no error that said why.
	if IsValidArchitecture("archfam") {
		t.Fatal("an unregistered architecture is valid before anyone registered it")
	}
	registerForTest(t, &fakeFamily{name: "archfam", transport: TransportJsonRpc})
	if !IsValidArchitecture("archfam") {
		t.Fatal("a registered family is not a valid architecture; its URLs 404 " +
			"while its family answers probes")
	}
	// Negative control: the registry must not turn into "everything is valid".
	if IsValidArchitecture("definitely-not-registered") {
		t.Fatal("an unregistered architecture validated; every typo would then " +
			"resolve to a network with no upstreams")
	}
	if IsValidArchitecture("") {
		t.Fatal("the empty architecture validated")
	}
}

func TestChainLiveness_OnlyHealthyServes(t *testing.T) {
	// The whole point of the type: syncing and unknown must NOT take traffic.
	// A bool would have collapsed these three into two and served a cold start
	// as if it were caught up.
	for _, tc := range []struct {
		state    ChainLiveness
		serving  bool
		rendered string
	}{
		{ChainUnknown, false, "unknown"},
		{ChainDown, false, "down"},
		{ChainSyncing, false, "syncing"},
		{ChainHealthy, true, "healthy"},
	} {
		if got := tc.state.Serving(); got != tc.serving {
			t.Errorf("%s.Serving() = %v, want %v", tc.rendered, got, tc.serving)
		}
		if got := tc.state.String(); got != tc.rendered {
			t.Errorf("String() = %q, want %q", got, tc.rendered)
		}
	}
}

func TestChainLiveness_ZeroValueIsUnknownNotDown(t *testing.T) {
	// A zero-value ChainProbe is what a caller holds before any probe runs.
	// If the zero value were ChainDown, every cold start would be reported as
	// an outage and would page someone.
	var p ChainProbe
	if p.Liveness != ChainUnknown {
		t.Fatalf("zero-value probe liveness = %v, want ChainUnknown", p.Liveness)
	}
	if p.Liveness == ChainDown {
		t.Fatal("zero value must not read as down")
	}
}

func TestChainProbe_CarriesTipAndReasonForOperators(t *testing.T) {
	// Step 2b of the brief: the same value that gates rotation must answer
	// "is this chain serving, and at what height?". Assert the fields survive
	// a round trip through the interface rather than being dropped.
	want := ChainProbe{
		Liveness: ChainSyncing,
		Tip:      812345,
		Detail:   "verificationprogress 0.97",
	}
	f := &fakeFamily{name: "probefam", transport: TransportJsonRpc, probe: want}
	registerForTest(t, f)

	got := f.Probe(context.Background(), nil)
	if got.Liveness != want.Liveness || got.Tip != want.Tip || got.Detail != want.Detail {
		t.Fatalf("probe round trip = %+v, want %+v", got, want)
	}
	if got.Detail == "" {
		t.Error("probe detail empty: an operator needs the reason, not just a verdict")
	}
}

func TestChainProbe_DownCarriesError(t *testing.T) {
	boom := errors.New("dial tcp: connection refused")
	p := ChainProbe{Liveness: ChainDown, Err: boom}
	if !errors.Is(p.Err, boom) {
		t.Fatal("probe error not retrievable with errors.Is; a wrapped-away cause " +
			"is indistinguishable from an unexplained outage")
	}
	if p.Liveness.Serving() {
		t.Fatal("a down probe reported as serving")
	}
}

func TestProbeCaller_IsWhatTheFamilyReceives(t *testing.T) {
	// Pins that Probe is handed the caller rather than reaching for a global
	// or a concrete Upstream — that indirection is what makes a family
	// testable against a fake server.
	var handed ProbeCaller
	sentinel := &recordingCaller{}
	f := &fakeFamily{
		name:      "callerfam",
		transport: TransportJsonRpc,
		probeCall: func(c ProbeCaller) { handed = c },
	}
	registerForTest(t, f)

	f.Probe(context.Background(), sentinel)
	if handed != ProbeCaller(sentinel) {
		t.Fatal("Probe did not receive the ProbeCaller it was given")
	}
}

func TestRotateVerdict_DistinguishesServeFromRotate(t *testing.T) {
	// The emptyResultAccept lesson, generalized: an empty result that IS the
	// answer must be VerdictServe. Collapsing serve/rotate is exactly the bug
	// that drove ~1.75M redundant upstream calls on evm:369.
	if VerdictServe == VerdictRotate {
		t.Fatal("serve and rotate are the same value")
	}
	for _, tc := range []struct {
		v    RotateVerdict
		want string
	}{
		{VerdictServe, "serve"},
		{VerdictRotate, "rotate"},
		{VerdictClientError, "clientError"},
	} {
		if got := tc.v.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestRotateVerdict_ZeroValueIsServe(t *testing.T) {
	// A family that forgets to set a verdict must fall back to serving what it
	// got, not to rotating. Defaulting to rotate would turn any unhandled case
	// into an upstream-amplification storm.
	var v RotateVerdict
	if v != VerdictServe {
		t.Fatalf("zero-value verdict = %v, want VerdictServe", v)
	}
}

func TestChainTransport_String(t *testing.T) {
	if got := TransportJsonRpc.String(); got != "jsonrpc" {
		t.Errorf("TransportJsonRpc.String() = %q", got)
	}
	if got := TransportREST.String(); got != "rest" {
		t.Errorf("TransportREST.String() = %q", got)
	}
}

// recordingCaller is an inert ProbeCaller used to assert wiring.
type recordingCaller struct{}

func (r *recordingCaller) CallJsonRpc(context.Context, string, []interface{}) ([]byte, error) {
	return nil, nil
}
func (r *recordingCaller) CallREST(context.Context, string, string) (int, []byte, error) {
	return 0, nil, nil
}
