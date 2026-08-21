package svm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
)

// malformedRequest is a request whose body is not a JSON-RPC call at all. Every
// handler entry point starts by asking for the method, so this is the one input
// that exercises each of their "could not read the method" branches.
func malformedRequest() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`not json at all`))
}

func svmRequest(method string) *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`))
}

// HandleUpstreamPreForward is a deliberate no-op: SVM does nothing between
// upstream selection and the wire. It must SAY nothing was handled, because a
// `true` here short-circuits the request and the upstream is never called.
func TestHandleUpstreamPreForward_HandlesNothingAndShortCircuitsNothing(t *testing.T) {
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}

	handled, resp, err := h.HandleUpstreamPreForward(
		context.Background(), net, &svmUpstreamStub{}, svmRequest("getSlot"), false)

	if handled {
		t.Fatal("HandleUpstreamPreForward claimed the request; the upstream would never be called")
	}
	if resp != nil {
		t.Fatal("HandleUpstreamPreForward returned a response it did not produce")
	}
	if err != nil {
		t.Fatalf("HandleUpstreamPreForward returned an error: %v", err)
	}
}

// The handler must hand the pipeline SVM's own error normalizer. A nil (or
// another family's) extractor means every Solana error code reaches the router
// unclassified, and retryable errors stop being retried.
func TestNewJsonRpcErrorExtractor_ReturnsTheSvmNormalizer(t *testing.T) {
	got := (&SvmArchitectureHandler{}).NewJsonRpcErrorExtractor()
	if got == nil {
		t.Fatal("the SVM handler supplies no error extractor")
	}
	if _, ok := got.(*JsonRpcErrorExtractor); !ok {
		t.Fatalf("extractor is %T, want SVM's own *JsonRpcErrorExtractor", got)
	}
}

// A request whose method cannot be read must fail at the FIRST hook rather
// than being forwarded. Forwarding it burns an upstream call on a request that
// no node can answer.
func TestHandleProjectPreForward_UnreadableMethodIsRefused(t *testing.T) {
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}

	handled, resp, err := h.HandleProjectPreForward(context.Background(), net, malformedRequest())
	if err == nil {
		t.Fatal("a request with no readable method was accepted at the project layer")
	}
	if handled || resp != nil {
		t.Fatal("a failed method read still produced a handled response")
	}
}

func TestHandleNetworkPreForward_UnreadableMethodIsRefused(t *testing.T) {
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}

	handled, resp, err := h.HandleNetworkPreForward(context.Background(), net, nil, malformedRequest())
	if err == nil {
		t.Fatal("a request with no readable method was accepted at the network layer")
	}
	if handled || resp != nil {
		t.Fatal("a failed method read still produced a handled response")
	}
}

// The post-forward hooks run on the way OUT, when the answer already exists.
// An unreadable method there must pass the response through untouched: turning
// a successful answer into an error because the request body was odd would
// lose data the upstream already paid for.
func TestHandleNetworkPostForward_UnreadableMethodPassesTheResponseThrough(t *testing.T) {
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	resp := common.NewNormalizedResponse()

	got, err := h.HandleNetworkPostForward(context.Background(), net, malformedRequest(), resp, nil)
	if err != nil {
		t.Fatalf("an unreadable method turned a good response into an error: %v", err)
	}
	if got != resp {
		t.Fatal("the response was replaced rather than passed through")
	}
}

// An upstream error must reach the caller unchanged, and no post-forward rule
// may run on a failed request.
//
// The second case is the sharp one: a request that failed but still carries a
// partial response must NOT have that response rewritten. Flooring a slot the
// upstream did not stand behind hands the caller a number no node ever
// reported, which then feeds the caller's next getBlock.
func TestHandleNetworkPostForward_UpstreamErrorIsPassedThroughUntouched(t *testing.T) {
	h := &SvmArchitectureHandler{}
	// A high tip: this is the value a slot rule would floor a response to.
	net := &fakeNetwork{
		cfg:           &common.NetworkConfig{Architecture: common.ArchitectureSvm, Svm: &common.SvmNetworkConfig{Commitment: "finalized"}},
		finalizedSlot: 300_000_000,
		indexedSlot:   300_000_000,
		latestSlot:    300_000_000,
	}
	want := errors.New("upstream refused the connection")

	got, err := h.HandleNetworkPostForward(context.Background(), net, svmRequest("getSlot"), nil, want)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want the original upstream error", err)
	}
	if got != nil {
		t.Fatal("a response was invented for a failed request")
	}

	slotReq, partial := slotResponse(t, "getSlot", 1000)

	gotPartial, errPartial := h.HandleNetworkPostForward(context.Background(), net, slotReq, partial, want)
	if !errors.Is(errPartial, want) {
		t.Fatalf("error = %v, want the original upstream error", errPartial)
	}
	if slot := readSlot(t, gotPartial); slot != 1000 {
		t.Fatalf("a failed request's slot was rewritten to %d; the caller gets a number no node reported", slot)
	}
}

// The same rule, asserted on the hook itself.
//
// The handler and the hook BOTH refuse a failed request, so neither guard is
// observable through the other — removing one leaves the other to catch it.
// Driving the hook directly is what makes its own guard testable, and this is
// the layer that would do the rewriting.
func TestNetworkPostForwardGetSlot_PartialResponseOnAFailedRequestIsNotRewritten(t *testing.T) {
	net := &fakeNetwork{
		cfg:           &common.NetworkConfig{Architecture: common.ArchitectureSvm, Svm: &common.SvmNetworkConfig{Commitment: "finalized"}},
		finalizedSlot: 300_000_000,
		indexedSlot:   300_000_000,
		latestSlot:    300_000_000,
	}
	want := errors.New("upstream refused the connection")
	req, resp := slotResponse(t, "getSlot", 1000)

	got, err := networkPostForward_getSlot(context.Background(), net, req, resp, want)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want the original upstream error", err)
	}
	if slot := readSlot(t, got); slot != 1000 {
		t.Fatalf("the slot on a FAILED request was floored to %d; the caller gets a number no node reported", slot)
	}
}

// The negative control: on a SUCCESSFUL request the same fixture must be
// floored to the tip. Without it the test above passes on a hook that never
// rewrites anything.
func TestNetworkPostForwardGetSlot_StaleSlotOnASuccessfulRequestIsFloored(t *testing.T) {
	net := &fakeNetwork{
		cfg:           &common.NetworkConfig{Architecture: common.ArchitectureSvm, Svm: &common.SvmNetworkConfig{Commitment: "finalized"}},
		finalizedSlot: 300_000_000,
		indexedSlot:   300_000_000,
		latestSlot:    300_000_000,
	}
	req, resp := slotResponse(t, "getSlot", 1000)

	got, err := networkPostForward_getSlot(context.Background(), net, req, resp, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot := readSlot(t, got); slot != 300_000_000 {
		t.Fatalf("slot = %d, want it floored to the 300000000 tip", slot)
	}
}

// A method with no network post-forward rule must pass straight through. Only
// getSlot is routed there; applying the slot floor to anything else rewrites
// an unrelated answer with a slot number.
//
// The fixture is deliberately slot-SHAPED (a bare integer result under a high
// network tip). An empty response would be passed through by the slot hook too,
// so it could not tell "not routed" from "routed and declined".
func TestHandleNetworkPostForward_UnroutedMethodPassesThrough(t *testing.T) {
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{
		cfg:           &common.NetworkConfig{Architecture: common.ArchitectureSvm, Svm: &common.SvmNetworkConfig{Commitment: "finalized"}},
		finalizedSlot: 300_000_000,
		indexedSlot:   300_000_000,
		latestSlot:    300_000_000,
	}
	req, resp := slotResponse(t, "getBalance", 1000)

	got, err := h.HandleNetworkPostForward(context.Background(), net, req, resp, nil)
	if err != nil {
		t.Fatalf("an unrouted method produced an error: %v", err)
	}
	if slot := readSlot(t, got); slot != 1000 {
		t.Fatalf("getBalance was rewritten to %d; the slot rule ran on a method it does not own", slot)
	}
}

// getBlockHeight must NOT be routed to the slot post-forward hook. Solana's
// block height and slot number are different counters — block height trails
// the slot number by the count of skipped slots, tens of millions on
// mainnet-beta. Applying the slot-tip floor would replace a block height with
// a slot number, and the canonical transaction-expiry check (getBlockHeight vs
// lastValidBlockHeight) would then see every transaction as expired.
func TestHandleNetworkPostForward_GetBlockHeightIsNotTreatedAsASlot(t *testing.T) {
	h := &SvmArchitectureHandler{}
	// A network with a high slot tip: if getBlockHeight were routed to the slot
	// hook, this tip is what would overwrite the answer.
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}, latestSlot: 300_000_000}

	jrr := &common.JsonRpcResponse{}
	if err := jrr.SetID(1); err != nil {
		t.Fatalf("SetID: %v", err)
	}
	jrr.SetResult([]byte("250000000"))
	resp := common.NewNormalizedResponse().WithJsonRpcResponse(jrr)

	got, err := h.HandleNetworkPostForward(context.Background(), net, svmRequest("getBlockHeight"), resp, nil)
	if err != nil {
		t.Fatalf("getBlockHeight produced an error: %v", err)
	}
	outJrr, err := got.JsonRpcResponse()
	if err != nil {
		t.Fatalf("JsonRpcResponse: %v", err)
	}
	if string(outJrr.GetResultBytes()) != "250000000" {
		t.Fatalf("block height became %s; a slot number replaced it and every transaction would read as expired",
			outJrr.GetResultBytes())
	}
}

// The upstream post-forward hook must not read a method it cannot parse, and
// must pass whatever it was given straight back.
func TestHandleUpstreamPostForward_UnreadableMethodPassesThrough(t *testing.T) {
	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	resp := common.NewNormalizedResponse()

	got, err := h.HandleUpstreamPostForward(
		context.Background(), net, &svmUpstreamStub{}, malformedRequest(), resp, nil, false)
	if err != nil {
		t.Fatalf("an unreadable method produced an error on the way out: %v", err)
	}
	if got != resp {
		t.Fatal("the response was replaced rather than passed through")
	}
}

// simulateTransaction is deliberately ABSENT from the non-retryable write set:
// it is read-only, so retrying or hedging it is safe and costs nothing but a
// duplicate read. Treating it as a write would strand every simulation on the
// first upstream that hiccups.
//
// This pins the documented intent so a future "consistency" edit to the write
// list cannot quietly change it.
func TestHandleUpstreamPostForward_SimulateTransactionIsNotGuardedAsAWrite(t *testing.T) {
	if IsNonRetryableWriteMethod("simulateTransaction") {
		t.Fatal("simulateTransaction is treated as a non-retryable write; it is read-only and safe to retry")
	}

	h := &SvmArchitectureHandler{}
	net := &fakeNetwork{cfg: &common.NetworkConfig{Architecture: common.ArchitectureSvm}}
	upstreamErr := errors.New("upstream timed out")

	// A failing sendTransaction is held: the transaction may still land via the
	// original node, so a failover would broadcast it twice.
	_, writeErr := h.HandleUpstreamPostForward(
		context.Background(), net, &svmUpstreamStub{}, svmRequest("sendTransaction"), nil, upstreamErr, false)
	if writeErr == nil {
		t.Fatal("a failed sendTransaction produced no error at all")
	}

	// A failing simulateTransaction keeps the plain upstream error, so the
	// generic retry machinery above can send it elsewhere.
	_, simErr := h.HandleUpstreamPostForward(
		context.Background(), net, &svmUpstreamStub{}, svmRequest("simulateTransaction"), nil, upstreamErr, false)
	if !errors.Is(simErr, upstreamErr) {
		t.Fatalf("simulateTransaction error = %v, want the plain upstream error so it can be retried", simErr)
	}
	if common.HasErrorCode(simErr, common.ErrCodeUpstreamRequestSkipped) {
		t.Fatalf("simulateTransaction was skipped like a write: %v", simErr)
	}
}

// The handler must be registered under the svm architecture at package init,
// or the generic pipeline runs no SVM hooks at all: no commitment injection,
// no write guard, no slot tracking.
func TestInit_RegistersTheSvmArchitectureHandler(t *testing.T) {
	h, err := common.GetArchitectureHandler(common.ArchitectureSvm)
	if err != nil || h == nil {
		t.Fatalf("no architecture handler registered for svm (%v); none of its hooks would run", err)
	}
	if _, isSvm := h.(*SvmArchitectureHandler); !isSvm {
		t.Fatalf("svm handler is %T, want *SvmArchitectureHandler", h)
	}
}

// The chain family must be registered too. Without it nothing resolves "svm"
// and every svm upstream is refused as an unknown type.
func TestInit_RegistersTheSvmChainFamily(t *testing.T) {
	f, ok := common.LookupChainFamily(common.ArchitectureSvm)
	if !ok {
		t.Fatal("the svm chain family is not registered")
	}
	if f.Family() != common.ArchitectureSvm {
		t.Fatalf("registered family = %q, want svm", f.Family())
	}
	// SVM is http-only: its state poller and cache assume request/response
	// semantics, so a ws:// endpoint must be refused rather than half-work.
	if ok, reason := f.(common.EndpointSchemeGate).SupportsEndpointScheme("ws"); ok {
		t.Fatal("the svm family accepted a ws endpoint")
	} else if !strings.Contains(strings.ToLower(reason), "http") {
		t.Fatalf("refusal reason = %q, want it to point the operator at http", reason)
	}
}
