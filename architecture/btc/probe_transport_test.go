package btc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
)

// NewProbeCaller is the seam the upstream layer uses to build a bitcoind probe
// transport without knowing bitcoind's dialect. If it stops threading the
// endpoint or the client through, every btc upstream probes the wrong address
// or probes with no timeout at all.
func TestNewProbeCaller_ThreadsTheEndpointAndClientThrough(t *testing.T) {
	client := &http.Client{Timeout: 3 * time.Second}
	pc := New().NewProbeCaller("http://user:pass@btc1.localhost:8332", client)

	h, ok := pc.(*HttpProbeCaller)
	if !ok {
		t.Fatalf("NewProbeCaller returned %T, want *HttpProbeCaller", pc)
	}
	if h.Endpoint != "http://user:pass@btc1.localhost:8332" {
		t.Fatalf("Endpoint = %q, want the caller's endpoint including its credentials", h.Endpoint)
	}
	if h.Client != client {
		t.Fatal("NewProbeCaller did not use the caller's http.Client; the probe would have no timeout")
	}
}

// The family must satisfy the probe-transport factory interface, because
// upstream bootstrap refuses every btc upstream without it.
func TestFamily_IsAProbeTransportFactory(t *testing.T) {
	var _ common.ProbeTransportFactory = New()
	if New().NewProbeCaller("http://x", &http.Client{}) == nil {
		t.Fatal("NewProbeCaller returned nil")
	}
}

// An endpoint that is not a usable URL must fail at request-build time. A
// probe that panicked or silently returned a zero height would report a
// misconfigured node as "healthy at height 0".
func TestCallJsonRpc_UnbuildableEndpointIsAnErrorNotAZero(t *testing.T) {
	h := &HttpProbeCaller{Endpoint: "http://\x7f-invalid", Client: &http.Client{Timeout: time.Second}}

	raw, err := h.CallJsonRpc(context.Background(), "getblockchaininfo", nil)
	if err == nil {
		t.Fatal("an unbuildable endpoint produced no error")
	}
	if raw != nil {
		t.Fatalf("raw = %q returned alongside the error", raw)
	}
}

// bitcoind can answer 200 with a body that is not JSON at all — an HTML error
// page from a reverse proxy in front of it is the common case. Reporting the
// decode failure is what tells an operator the traffic never reached bitcoind.
func TestCallJsonRpc_UndecodableBodyOnA200IsADecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	}))
	defer srv.Close()

	h := &HttpProbeCaller{Endpoint: srv.URL, Client: &http.Client{Timeout: 3 * time.Second}}
	_, err := h.CallJsonRpc(context.Background(), "getblockchaininfo", nil)
	if err == nil {
		t.Fatal("an HTML body was accepted as a JSON-RPC envelope")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error = %v, want it to say the envelope could not be decoded", err)
	}
}

// A non-2xx status with no JSON-RPC error body is still a failure, and the
// status code is the only clue an operator gets. Losing it leaves them with
// "the probe failed" and nothing else.
func TestCallJsonRpc_NonJsonErrorStatusReportsTheHttpCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("Unauthorized"))
	}))
	defer srv.Close()

	h := &HttpProbeCaller{Endpoint: srv.URL, Client: &http.Client{Timeout: 3 * time.Second}}
	_, err := h.CallJsonRpc(context.Background(), "getblockchaininfo", nil)
	if err == nil {
		t.Fatal("HTTP 401 was accepted as a successful probe")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v, want it to carry the 401 so an operator sees the bad rpcauth", err)
	}
}

// A well-formed envelope with no result member means the node answered but
// said nothing. Returning empty bytes would let the family decode them into a
// zero-height chainInfo and report the node as syncing rather than broken.
func TestCallJsonRpc_EmptyResultIsAnErrorNotAnEmptyAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":null,"id":"erpc-probe"}`))
	}))
	defer srv.Close()

	h := &HttpProbeCaller{Endpoint: srv.URL, Client: &http.Client{Timeout: 3 * time.Second}}
	raw, err := h.CallJsonRpc(context.Background(), "getblockchaininfo", nil)
	if err == nil {
		t.Fatalf("an envelope with no result was accepted, returning %q", raw)
	}
	if !strings.Contains(err.Error(), "empty result") {
		t.Fatalf("error = %v, want it to name the empty result", err)
	}
}

// A body larger than the read cap must not pin memory on every poll tick. A
// probe talks to whatever address is configured, and a URL pointing at a
// streaming endpoint would otherwise grow the process without bound.
func TestCallJsonRpc_OversizedBodyIsCappedNotStreamedForever(t *testing.T) {
	// A body just over the cap, made of a single JSON string so nothing about
	// the shape short-circuits the read.
	huge := strings.Repeat("A", maxProbeBody+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":"` + huge + `","error":null,"id":"erpc-probe"}`))
	}))
	defer srv.Close()

	h := &HttpProbeCaller{Endpoint: srv.URL, Client: &http.Client{Timeout: 10 * time.Second}}
	// Truncating mid-JSON makes the envelope undecodable, which is the correct
	// verdict: eRPC must not act on a body it only half read.
	_, err := h.CallJsonRpc(context.Background(), "getblockchaininfo", nil)
	if err == nil {
		t.Fatal("a body past the read cap was accepted as a complete envelope")
	}
}

// A body exactly at the cap must still be read whole, or a legitimately large
// answer would be rejected. This is the negative control for the test above.
func TestCallJsonRpc_BodyUnderTheCapIsReadWhole(t *testing.T) {
	payload := strings.Repeat("A", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":"` + payload + `","error":null,"id":"erpc-probe"}`))
	}))
	defer srv.Close()

	h := &HttpProbeCaller{Endpoint: srv.URL, Client: &http.Client{Timeout: 10 * time.Second}}
	raw, err := h.CallJsonRpc(context.Background(), "getblockchaininfo", nil)
	if err != nil {
		t.Fatalf("a 4 KiB result was rejected: %v", err)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("result is not the string that was sent: %v", err)
	}
	if got != payload {
		t.Fatalf("result is %d bytes, want %d — the body was truncated", len(got), len(payload))
	}
}

// The probe must reuse its connection even when the read cap truncated the
// body. It runs on a tick, so a connection leaked per tick exhausts the pool
// and the node starts refusing eRPC while serving everyone else.
//
// The body is deliberately LARGER than the cap. On a small body the cap never
// bites and the read consumes everything, so the explicit drain is invisible —
// only an over-cap body proves it is there.
func TestCallJsonRpc_ReusesTheConnectionEvenWhenTheReadCapTruncates(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	oversized := strings.Repeat("A", maxProbeBody+4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.RemoteAddr] = true
		mu.Unlock()
		_, _ = w.Write([]byte(`{"result":"` + oversized + `","error":null,"id":"erpc-probe"}`))
	}))
	defer srv.Close()

	h := &HttpProbeCaller{Endpoint: srv.URL, Client: &http.Client{Timeout: 20 * time.Second}}
	for i := 0; i < 3; i++ {
		// The truncated envelope is an error; the connection is what matters.
		_, _ = h.CallJsonRpc(context.Background(), "getblockchaininfo", nil)
	}

	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("3 probes opened %d connections, want 1 — the body is not drained before close", n)
	}
}

// A nil caller must not panic. The upstream layer can hold a nil transport for
// an upstream whose construction failed, and a panicking probe loop takes the
// whole process down rather than one upstream.
func TestCallJsonRpc_NilReceiverIsAnErrorNotAPanic(t *testing.T) {
	var h *HttpProbeCaller
	if _, err := h.CallJsonRpc(context.Background(), "getblockchaininfo", nil); err == nil {
		t.Fatal("a nil probe caller returned no error")
	}
}

// bitcoind clears `initialblockdownload` once it is NEAR the tip, so a node
// replaying a long reorg can be out of IBD, level with its own headers, and
// still materially behind. verificationprogress is the only honest signal
// left, and serving such a node returns stale UTXO state.
func TestProbe_LowVerificationProgressIsSyncingEvenWhenLevelWithHeaders(t *testing.T) {
	c := &cannedCaller{result: chainInfoJSON(t, 812345, 812345, 0.98, false)}

	got := New().Probe(context.Background(), c)

	if got.Liveness != common.ChainSyncing {
		t.Fatalf("liveness = %v, want syncing at verificationprogress 0.98", got.Liveness)
	}
	if got.Tip != 812345 {
		t.Fatalf("tip = %d, want 812345 reported even while syncing", got.Tip)
	}
	if !strings.Contains(got.Detail, "verificationprogress") {
		t.Fatalf("detail = %q, want it to name verificationprogress so an operator knows why", got.Detail)
	}
}

// The negative control for the test above: progress at the floor must serve.
// Without it, a probe that called everything syncing would pass.
func TestProbe_VerificationProgressAtTheFloorIsHealthy(t *testing.T) {
	c := &cannedCaller{result: chainInfoJSON(t, 812345, 812345, minVerificationProgress, false)}

	if got := New().Probe(context.Background(), c); got.Liveness != common.ChainHealthy {
		t.Fatalf("liveness = %v at the %.4f floor, want healthy", got.Liveness, minVerificationProgress)
	}
}

// The family must be in the global registry after package init, or nothing
// resolves "btc" and every btc upstream is refused as an unknown type.
func TestInit_RegistersTheBtcFamily(t *testing.T) {
	f, ok := common.LookupChainFamily(Architecture)
	if !ok {
		t.Fatal("the btc family is not registered; every btc upstream would be an unsupported type")
	}
	if f.Family() != Architecture {
		t.Fatalf("registered family = %q, want %q", f.Family(), Architecture)
	}
}
