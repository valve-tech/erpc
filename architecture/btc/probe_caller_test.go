package btc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
)

// fakeBitcoind is a stand-in bitcoind: it speaks the real wire shape
// (JSON-RPC 1.0 envelope, 500-with-error-body on RPC failure) so these tests
// exercise the transport rather than a convenient fiction.
type fakeBitcoind struct {
	*httptest.Server
	// closeOnce guards Close: a test that closes the server to simulate an
	// unreachable node would otherwise collide with t.Cleanup's close, and
	// httptest.Server.Close blocks on the second call.
	closeOnce sync.Once
	// height/headers drive getblockchaininfo.
	height  int64
	headers int64
	ibd     bool
	// rpcError, when set, is returned as a JSON-RPC error with HTTP 500,
	// which is what bitcoind actually does.
	rpcError string
	// gotMethods records what was asked for.
	gotMethods []string
}

func newFakeBitcoind(t *testing.T, height, headers int64) *fakeBitcoind {
	t.Helper()
	f := &fakeBitcoind{height: height, headers: headers}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		f.gotMethods = append(f.gotMethods, req.Method)

		w.Header().Set("Content-Type", "application/json")
		if f.rpcError != "" {
			// bitcoind's real behaviour: HTTP 500 carrying a JSON-RPC error.
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"result": nil,
				"error":  map[string]interface{}{"code": -8, "message": f.rpcError},
				"id":     "erpc-probe",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"result": map[string]interface{}{
				"chain":                "main",
				"blocks":               f.height,
				"headers":              f.headers,
				"verificationprogress": 0.999999,
				"initialblockdownload": f.ibd,
			},
			"error": nil,
			"id":    "erpc-probe",
		})
	}))
	t.Cleanup(f.closeSafely)
	return f
}

// closeSafely shuts the server down exactly once.
func (f *fakeBitcoind) closeSafely() {
	f.closeOnce.Do(func() {
		if f.Server != nil {
			f.Server.Close()
		}
	})
}

func callerFor(f *fakeBitcoind) *HttpProbeCaller {
	return &HttpProbeCaller{Endpoint: f.URL, Client: &http.Client{Timeout: 5 * time.Second}}
}

func TestHttpProbeCaller_EndToEndAgainstFakeBitcoind(t *testing.T) {
	node := newFakeBitcoind(t, 812345, 812345)

	got := New().Probe(context.Background(), callerFor(node))

	if got.Liveness != common.ChainHealthy {
		t.Fatalf("liveness = %v (%s / %v), want healthy", got.Liveness, got.Detail, got.Err)
	}
	if got.Tip != 812345 {
		t.Fatalf("tip = %d, want 812345", got.Tip)
	}
	if len(node.gotMethods) != 1 || node.gotMethods[0] != "getblockchaininfo" {
		t.Fatalf("node saw %v, want exactly [getblockchaininfo]", node.gotMethods)
	}
}

func TestHttpProbeCaller_SendsJsonRpcBitcoindExpects(t *testing.T) {
	// bitcoind is strict about the envelope. Asserting the wire shape here is
	// what catches a change that a canned-caller unit test cannot see.
	var seen map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &seen)
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if r.Method != http.MethodPost {
			t.Errorf("HTTP method = %s, want POST", r.Method)
		}
		_, _ = w.Write([]byte(`{"result":{"blocks":1,"headers":1},"error":null,"id":"erpc-probe"}`))
	}))
	defer srv.Close()

	_, err := callerFor(&fakeBitcoind{Server: srv}).CallJsonRpc(context.Background(), "getblockchaininfo", nil)
	if err != nil {
		t.Fatalf("CallJsonRpc: %v", err)
	}
	if seen["method"] != "getblockchaininfo" {
		t.Errorf("method = %v", seen["method"])
	}
	if _, ok := seen["params"]; !ok {
		t.Error("params omitted; some bitcoind versions reject a missing params field")
	}
	if seen["params"] == nil {
		t.Error("params sent as null; bitcoind wants [] — a nil slice must be normalized")
	}
}

func TestHttpProbeCaller_RpcErrorBodyBeatsHttpStatus(t *testing.T) {
	// bitcoind returns HTTP 500 WITH a useful JSON-RPC error. Reporting only
	// "http 500" would throw away the one line that says what is wrong.
	node := newFakeBitcoind(t, 0, 0)
	node.rpcError = "Block height out of range"

	_, err := callerFor(node).CallJsonRpc(context.Background(), "getblockchaininfo", nil)
	if err == nil {
		t.Fatal("rpc error body produced no error")
	}
	if !strings.Contains(err.Error(), "Block height out of range") {
		t.Fatalf("error %q does not carry bitcoind's message", err)
	}
}

func TestProbe_RpcErrorSurfacesAsDownWithTheReason(t *testing.T) {
	node := newFakeBitcoind(t, 0, 0)
	node.rpcError = "Loading block index..."

	got := New().Probe(context.Background(), callerFor(node))

	if got.Liveness != common.ChainDown {
		t.Fatalf("liveness = %v, want down while the node is loading its index", got.Liveness)
	}
	if got.Err == nil || !strings.Contains(got.Err.Error(), "Loading block index") {
		t.Fatalf("probe error %v lost bitcoind's reason", got.Err)
	}
}

func TestHttpProbeCaller_UnreachableEndpointIsAnError(t *testing.T) {
	// Closed server → connection refused. The probe must report down, not
	// silently succeed with a zero height.
	node := newFakeBitcoind(t, 1, 1)
	node.closeSafely()

	got := New().Probe(context.Background(), callerFor(node))
	if got.Liveness != common.ChainDown {
		t.Fatalf("liveness = %v, want down for a refused connection", got.Liveness)
	}
	if got.Tip != 0 {
		t.Fatalf("tip = %d, want 0 when nothing answered", got.Tip)
	}
}

func TestHttpProbeCaller_NilClientIsRejectedNotPanicked(t *testing.T) {
	// A probe with no timeout can wedge the poll loop forever. Refuse it.
	c := &HttpProbeCaller{Endpoint: "http://127.0.0.1:1"}
	if _, err := c.CallJsonRpc(context.Background(), "getblockchaininfo", nil); err == nil {
		t.Fatal("nil http client accepted; a probe without a timeout can hang the poll loop")
	}
}

func TestHttpProbeCaller_ContextCancellationIsHonoured(t *testing.T) {
	// A poll tick that outlives its context must abandon the call, or a slow
	// node accumulates in-flight probes until the pool is exhausted.
	// The handler parks until the test releases it. It must NOT wait only on
	// r.Context(): Go's server does not cancel a request context when the
	// CLIENT times out on an otherwise-idle HTTP/1.1 connection, so the
	// handler would outlive the test and srv.Close() would block forever.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = callerFor(&fakeBitcoind{Server: srv}).CallJsonRpc(ctx, "getblockchaininfo", nil)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("CallJsonRpc ignored context cancellation")
	}
}

func TestHttpProbeCaller_RESTIsRefused(t *testing.T) {
	// Bitcoin is a JSON-RPC family. A REST call means a caller reached for the
	// wrong transport; returning a plausible zero would hide that.
	_, _, err := (&HttpProbeCaller{}).CallREST(context.Background(), "GET", "/anything")
	if err == nil {
		t.Fatal("CallREST succeeded on a JSON-RPC-only family")
	}
}

// TestFailoverPicksTheMostCaughtUpNode is the brief's Step 3 acceptance: given
// several bitcoind upstreams at different heights, the probe results must rank
// them so the most caught-up node is chosen and a syncing one is excluded.
//
// Ranking itself is chain-agnostic and already tested in health/ and
// internal/policy — what needs proving here is that the BTC family produces
// inputs those layers can rank. So this asserts on the probe results, which is
// the fork's actual seam into them.
func TestFailoverPicksTheMostCaughtUpNode(t *testing.T) {
	behind := newFakeBitcoind(t, 811000, 812345) // 1345 blocks behind headers
	syncing := newFakeBitcoind(t, 812340, 812345)
	syncing.ibd = true
	current := newFakeBitcoind(t, 812345, 812345)

	f := New()
	type result struct {
		name  string
		probe common.ChainProbe
	}
	var serving []result
	for _, tc := range []struct {
		name string
		node *fakeBitcoind
	}{{"behind", behind}, {"syncing", syncing}, {"current", current}} {
		p := f.Probe(context.Background(), callerFor(tc.node))
		if p.Liveness.Serving() {
			serving = append(serving, result{tc.name, p})
		}
	}

	if len(serving) != 1 {
		names := make([]string, 0, len(serving))
		for _, s := range serving {
			names = append(names, s.name)
		}
		sort.Strings(names)
		t.Fatalf("%d nodes reported serving (%v), want only the caught-up one", len(serving), names)
	}
	if serving[0].name != "current" {
		t.Fatalf("serving node = %s, want current", serving[0].name)
	}
	if serving[0].probe.Tip != 812345 {
		t.Fatalf("serving tip = %d, want 812345", serving[0].probe.Tip)
	}

	// And the excluded ones must still report their height, so an operator can
	// watch them catch up rather than seeing them vanish.
	if p := f.Probe(context.Background(), callerFor(behind)); p.Tip != 811000 {
		t.Fatalf("behind node tip = %d, want 811000 reported even while excluded", p.Tip)
	}
}
