package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erpc/erpc/architecture/btc"
	"github.com/erpc/erpc/common"
)

// eRPC must not route to a node on a chain the operator did not configure. A
// testnet bitcoind in a mainnet pool answers every request with testnet blocks,
// and until this gate existed it joined the pool and served them.
//
// These tests drive the REAL bitcoin family against a faked bitcoind, because
// the reconciliation that makes the gate hard — the node says "main" where the
// config says "mainnet" — only exists on that path. The family-independent half
// (what bootstrap does with the verdict) is proved with the fake family below.

// fakeBitcoindServer answers getblockchaininfo for a caught-up node on `chain`.
// An empty `chain` omits the field, which is the older client that reports no
// chain at all.
func fakeBitcoindServer(t *testing.T, chain string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		result := map[string]interface{}{
			"blocks":               962470,
			"headers":              962470,
			"verificationprogress": 0.999999,
			"initialblockdownload": false,
		}
		if chain != "" {
			result["chain"] = chain
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"result": result, "error": nil, "id": "erpc-probe",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newBtcUpstream(t *testing.T, ctx context.Context, endpoint, chain string) *Upstream {
	t.Helper()
	return newTestUpstream(t, ctx, &common.UpstreamConfig{
		Id: "u1", Type: common.UpstreamType(btc.Architecture),
		Endpoint: endpoint, Chain: chain,
	})
}

func TestChainFamilyBootstrap_TestnetNodeIsRefusedFromAMainnetPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	u := newBtcUpstream(t, ctx, fakeBitcoindServer(t, "test").URL, "mainnet")

	err := u.detectFeatures(ctx)
	if err == nil {
		t.Fatal("a testnet bitcoind joined a mainnet pool; it would serve testnet blocks to mainnet clients")
	}
	// The message has to name both sides, or an operator cannot tell which of
	// the two is wrong.
	for _, want := range []string{"mainnet", "test"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
	// Permanent: neither the config nor the node's chain changes by itself, so
	// a retry loop would only bury the message.
	if !isFatal(err) {
		t.Fatalf("a chain mismatch was reported as retryable: %v", err)
	}
	// The networkId is the only thing binding an upstream to a network. Without
	// it the node cannot be selected for anything.
	if got := u.NetworkId(); got != "n/a" {
		t.Fatalf("networkId = %q for a node on the wrong chain; it must not be routable", got)
	}
}

func TestChainFamilyBootstrap_MainnetNodeIsAcceptedIntoAMainnetPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// bitcoind says "main" where the operator writes "mainnet". A gate that
	// refused this would take every correct node out of service.
	u := newBtcUpstream(t, ctx, fakeBitcoindServer(t, "main").URL, "mainnet")

	if err := u.detectFeatures(ctx); err != nil {
		t.Fatalf("a mainnet bitcoind was refused from a mainnet pool: %v", err)
	}
	if got := u.NetworkId(); got != "btc:mainnet" {
		t.Fatalf("networkId = %q, want btc:mainnet", got)
	}
	if _, cordoned := u.CordonedReason("*"); cordoned {
		t.Fatal("a node on the configured chain was cordoned")
	}
}

func TestChainFamilyBootstrap_TestnetNodeIsAcceptedIntoATestnetPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	u := newBtcUpstream(t, ctx, fakeBitcoindServer(t, "test").URL, "testnet")

	if err := u.detectFeatures(ctx); err != nil {
		t.Fatalf("a testnet bitcoind was refused from a testnet pool: %v", err)
	}
	if got := u.NetworkId(); got != "btc:testnet" {
		t.Fatalf("networkId = %q, want btc:testnet", got)
	}
}

func TestChainFamilyBootstrap_ANodeThatReportsNoChainStillJoins(t *testing.T) {
	// No answer is not a wrong answer. An older or unusual client omits the
	// field, and refusing it would take a working upstream out of service over
	// a missing string.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	u := newBtcUpstream(t, ctx, fakeBitcoindServer(t, "").URL, "mainnet")

	if err := u.detectFeatures(ctx); err != nil {
		t.Fatalf("a node that reported no chain was refused: %v", err)
	}
	if got := u.NetworkId(); got != "btc:mainnet" {
		t.Fatalf("networkId = %q, want btc:mainnet", got)
	}
	if _, cordoned := u.CordonedReason("*"); cordoned {
		t.Fatal("a node that reported no chain was cordoned; eRPC observed nothing against it")
	}
}

func TestChainFamilyBootstrap_MismatchedNodePublishesNoTip(t *testing.T) {
	// A height from another chain is not comparable with this one's. Head lag
	// is derived from the highest tip in the network, so one testnet height
	// among mainnet upstreams would make every correct node look millions of
	// blocks behind.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wrong := newBtcUpstream(t, ctx, fakeBitcoindServer(t, "test").URL, "mainnet")
	if err := wrong.detectFeatures(ctx); err == nil {
		t.Fatal("a testnet node bootstrapped into a mainnet pool")
	}
	got := wrong.metricsTracker.
		GetUpstreamMethodMetrics(wrong, "*", common.DataFinalityStateAll).
		BlockHeadLag.Load()
	if got != 0 {
		t.Fatalf("a refused node reached the health tracker (lag %d); its height ranks the whole pool", got)
	}
}

func TestChainProbePoller_CordonsANodeThatChangesChainUnderTheEndpoint(t *testing.T) {
	// An endpoint outlives the node behind it. A DNS name or a load balancer
	// gets repointed at a testnet node, eRPC never restarts, and bootstrap's
	// one-time check cannot see it.
	u, fam, poller := newProbePoller(t, common.ChainProbe{
		Liveness: common.ChainHealthy, Tip: 962470, Chain: "mainnet",
	})
	poller.poll()
	if _, cordoned := u.CordonedReason("*"); cordoned {
		t.Fatal("a node on the configured chain was cordoned")
	}

	fam.set(common.ChainProbe{Liveness: common.ChainHealthy, Tip: 4000000, Chain: "someothernet"})
	poller.poll()

	reason, cordoned := u.CordonedReason("*")
	if !cordoned {
		t.Fatal("an upstream that moved to another chain kept serving")
	}
	// The reason has to name both chains: the node looks healthy from every
	// other angle, so the chain is the only thing that explains the cordon.
	for _, want := range []string{"someothernet", "mainnet"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("cordon reason %q does not mention %q", reason, want)
		}
	}
	// And its height never reached the tracker. 4000000 among mainnet
	// upstreams would make every correct node look 3 million blocks behind.
	if got := tipOf(t, u); got != 0 {
		t.Fatalf("another chain's height ranked this pool (lag %d)", got)
	}
}

// TestChainFamilyBootstrap_MismatchGateIsFamilyIndependent proves the bootstrap
// half without bitcoin's naming rule: the fake family calls two names the same
// chain only when they are equal.
func TestChainFamilyBootstrap_MismatchGateIsFamilyIndependent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fam := &probingFakeFamily{&fakeFamily{probe: common.ChainProbe{
		Liveness: common.ChainHealthy, Tip: 812345, Chain: "someothernet",
	}}}
	registerFakeFamily(t, fam)

	u := newTestUpstream(t, ctx, &common.UpstreamConfig{
		Id: "u1", Type: common.UpstreamType(fakeFamilyName),
		Endpoint: "http://node.localhost:1234", Chain: "mainnet",
	})

	err := u.detectFeatures(ctx)
	if err == nil {
		t.Fatal("bootstrap accepted a node whose family says it is on another chain")
	}
	if !isFatal(err) {
		t.Fatalf("a chain mismatch was reported as retryable: %v", err)
	}

	// And the same family accepts the node once the names agree, so the gate is
	// asking the family rather than refusing everything it cannot read.
	fam.probe.Chain = "mainnet"
	if err := u.detectFeatures(ctx); err != nil {
		t.Fatalf("bootstrap refused a node its family accepts: %v", err)
	}
	if got := u.NetworkId(); got != "fakechain:mainnet" {
		t.Fatalf("networkId = %q, want fakechain:mainnet", got)
	}
}
