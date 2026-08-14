package clients

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
)

// identityServiceConfig mirrors the service config NewGrpcBdsClient installs.
// grpc.NewClient rejects an empty one, so the pool cannot be built without it.
const identityServiceConfig = `{"loadBalancingConfig":[{"round_robin":{}}]}`

// newIdentityPool builds a pool against addr with an armed expected chainId.
// The pool's maintainer goroutine is stopped on cleanup.
func newIdentityPool(t *testing.T, addr string, expectedChainId uint64) (*bdsPool, error) {
	t.Helper()
	lg := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	p, err := newBdsPool(ctx, &lg, "prj1", "bds1", "dns:///"+addr,
		insecure.NewCredentials(), identityServiceConfig, 1, expectedChainId)
	if p != nil {
		t.Cleanup(p.Shutdown)
	}
	return p, err
}

// A BDS endpoint answering for a DIFFERENT chain is cross-wired — a stale DNS
// record, or an address reused by another chain's server. The pool must refuse
// to bootstrap on it. Coming up anyway would let another chain's blocks and
// heads flow into this network's cache and state poller, and nothing
// downstream can tell them apart.
func TestNewBdsPool_RefusesToBootstrapOnACrossWiredEndpoint(t *testing.T) {
	addr, _, stop := startHappyServer(t, 137, 1)
	defer stop()

	p, err := newIdentityPool(t, addr, 1)
	if err == nil {
		t.Fatalf("a pool bootstrapped on a server answering for chainId 137 while expecting 1 (pool=%v)", p)
	}
	if !strings.Contains(err.Error(), "137") || !strings.Contains(err.Error(), "cross-wired") {
		t.Fatalf("error = %v, want it to name the detected chain and call out the cross-wire", err)
	}
}

// A server answering for the RIGHT chain must bootstrap. Without this the test
// above would pass on a pool that refuses everything.
func TestNewBdsPool_BootstrapsOnAMatchingChain(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 1)
	defer stop()

	p, err := newIdentityPool(t, addr, 1)
	if err != nil {
		t.Fatalf("a pool refused a server answering for the expected chain: %v", err)
	}
	if p.Size() != 1 {
		t.Fatalf("pool size = %d, want 1", p.Size())
	}
}

// A server whose identity cannot be DETERMINED (it errors rather than
// answering with a different chain) must not fail the pool. That preserves the
// lazy-dial behaviour eRPC relies on at boot: an upstream that is briefly
// unreachable must still be constructed so it can recover, and every request
// carries its own chainId assertion regardless.
func TestNewBdsPool_TransientVerificationErrorDoesNotFailTheBootstrap(t *testing.T) {
	addr, stop := startErrorServer(t, codes.Unavailable, "backend warming up")
	defer stop()

	p, err := newIdentityPool(t, addr, 1)
	if err != nil {
		t.Fatalf("a transient verification error failed the pool: %v", err)
	}
	if p == nil || p.Size() != 1 {
		t.Fatal("no pool was built for a temporarily unreachable server")
	}
}

// verifyConn must report the mismatch it detected, not just "wrong". The
// detected chain id is what tells an operator WHICH backend the address
// actually points at.
func TestVerifyConn_ReportsTheDetectedChain(t *testing.T) {
	addr, _, stop := startHappyServer(t, 42161, 1)
	defer stop()

	// Bootstrap unarmed so the pool comes up, then arm it and verify.
	p, err := newIdentityPool(t, addr, 0)
	if err != nil {
		t.Fatalf("newBdsPool: %v", err)
	}
	p.expectedChainId.Store(1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ok, detected, verr := p.verifyConn(ctx, p.Pick())
	if verr != nil {
		t.Fatalf("verifyConn: %v", verr)
	}
	if ok {
		t.Fatal("verifyConn accepted a server answering for chainId 42161 while expecting 1")
	}
	if detected != 42161 {
		t.Fatalf("detected chainId = %d, want 42161", detected)
	}
}

// With no expected chainId, verification must be a no-op that passes. This is
// the unarmed case: the cache connector learns the chain by probing, and until
// it does the pool must not reject its own connections.
func TestVerifyConn_UnarmedPoolAcceptsAnything(t *testing.T) {
	addr, _, stop := startHappyServer(t, 99999, 1)
	defer stop()

	p, err := newIdentityPool(t, addr, 0)
	if err != nil {
		t.Fatalf("newBdsPool: %v", err)
	}

	ok, _, verr := p.verifyConn(context.Background(), p.Pick())
	if verr != nil {
		t.Fatalf("verifyConn on an unarmed pool: %v", verr)
	}
	if !ok {
		t.Fatal("an unarmed pool rejected its own connection")
	}
}

// A nil connection must not panic verification. The maintainer walks a
// snapshot of the pool, and a slot can be nil while a replacement is in
// flight — a panic there kills the maintainer for the process lifetime.
func TestVerifyConn_NilConnIsAcceptedNotPanicked(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 1)
	defer stop()

	p, err := newIdentityPool(t, addr, 1)
	if err != nil {
		t.Fatalf("newBdsPool: %v", err)
	}

	if ok, _, verr := p.verifyConn(context.Background(), nil); !ok || verr != nil {
		t.Fatalf("verifyConn(nil) = (%v, _, %v), want (true, _, nil)", ok, verr)
	}
}

// A verification that cannot reach the server must report the transport error,
// NOT a mismatch. Treating an unreachable server as a wrong-chain server would
// make the maintainer quarantine and re-dial healthy connections during any
// blip — the churn storm the hard-cap sentinel exists to prevent.
func TestVerifyConn_UnreachableServerIsAnErrorNotAMismatch(t *testing.T) {
	addr, stop := startErrorServer(t, codes.Unavailable, "gone")
	defer stop()

	p, err := newIdentityPool(t, addr, 1)
	if err != nil {
		t.Fatalf("newBdsPool: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, detected, verr := p.verifyConn(ctx, p.Pick())
	if verr == nil {
		t.Fatal("an unreachable server was reported as a determinate answer")
	}
	if detected != 0 {
		t.Fatalf("detected chainId = %d from a server that never answered", detected)
	}
}

// recycleConn must swap a fresh connection into the slot and leave the pool
// the same size. A recycle that shrank the pool would quietly reduce capacity
// on every maintenance tick.
func TestRecycleConn_ReplacesTheSlotAndKeepsThePoolSize(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 1)
	defer stop()

	p, err := newIdentityPool(t, addr, 1)
	if err != nil {
		t.Fatalf("newBdsPool: %v", err)
	}
	before := p.Pick()

	p.recycleConn(before, "age")

	after := p.Pick()
	if after == before {
		t.Fatal("recycleConn left the old connection in the slot")
	}
	if p.Size() != 1 {
		t.Fatalf("pool size = %d after a recycle, want 1", p.Size())
	}
	if after == nil || after.rpcClient == nil {
		t.Fatal("the replacement connection has no rpc client")
	}
}

// A replacement that itself answers for the wrong chain must NOT be installed.
// Installing it would swap a quarantined connection for another wrong one and
// call the problem fixed.
func TestRecycleConn_RefusesAReplacementOnTheWrongChain(t *testing.T) {
	addr, _, stop := startHappyServer(t, 137, 1)
	defer stop()

	// Bootstrap unarmed so the pool exists, then arm it with a chain the
	// server does not serve — every replacement dial now fails verification.
	p, err := newIdentityPool(t, addr, 0)
	if err != nil {
		t.Fatalf("newBdsPool: %v", err)
	}
	p.expectedChainId.Store(1)
	before := p.Pick()

	p.recycleConn(before, "chainid_mismatch")

	if p.Pick() != before {
		t.Fatal("a replacement answering for the wrong chain was installed")
	}
}

// Recycling a connection that is NOT in the pool must change nothing. The
// maintainer works from a snapshot taken before it dials, so by the time it
// swaps, the slot it read may already hold a different connection. Installing
// into "some slot" anyway would evict a healthy connection the watchdog just
// put there.
func TestRecycleConn_AConnectionThatLeftThePoolCannotEvictALiveSlot(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 1)
	defer stop()

	p, err := newIdentityPool(t, addr, 1)
	if err != nil {
		t.Fatalf("newBdsPool: %v", err)
	}
	live := p.Pick()

	// A freshly dialled connection that was never pooled. Its closedAt is zero,
	// so the dedup window cannot mask the slot lookup here.
	stranger, err := p.dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = stranger.conn.Close() })

	p.recycleConn(stranger, "age")

	if p.Pick() != live {
		t.Fatal("recycling a connection that was never pooled evicted the live one")
	}
	if p.Size() != 1 {
		t.Fatalf("pool size = %d, want 1", p.Size())
	}
}

// pickTargetForBDS decides the dial target and whether TLS is used. Getting
// the TLS choice wrong is an immediate outage: a plaintext dial to a TLS port
// hangs, and a TLS dial to a plaintext port fails the handshake.
func TestPickTargetForBDS(t *testing.T) {
	cases := []struct {
		endpoint   string
		wantTarget string
		wantTLS    bool
	}{
		{"grpc://bds.example.com:50051", "dns:///bds.example.com:50051", false},
		{"grpc://bds.example.com", "dns:///bds.example.com:50051", false},
		{"grpc://bds.example.com:443", "dns:///bds.example.com:443", true},
		{"grpcs://bds.example.com:50051", "dns:///bds.example.com:50051", true},
		{"grpc+tls://bds.example.com:50051", "dns:///bds.example.com:50051", true},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			u, err := url.Parse(tc.endpoint)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			target, useTLS := pickTargetForBDS(u)
			if target != tc.wantTarget {
				t.Fatalf("target = %q, want %q", target, tc.wantTarget)
			}
			if useTLS != tc.wantTLS {
				t.Fatalf("useTLS = %v, want %v", useTLS, tc.wantTLS)
			}
		})
	}
}

// SetExpectedChainId must arm the POOL as well as the client. Arming only the
// client would leave the background maintainer verifying against nothing, so a
// connection that drifts onto another chain mid-life is never caught.
func TestSetExpectedChainId_ArmsThePoolMaintainerToo(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 1)
	defer stop()
	c := newTestClient(t, addr)

	c.SetExpectedChainId(8453)

	if got := c.pool.expectedChainId.Load(); got != 8453 {
		t.Fatalf("pool expectedChainId = %d, want 8453; the maintainer would verify nothing", got)
	}
}

// A request against a pool with no connections must be a transport failure,
// not a nil dereference.
func TestSendRequest_EmptyPoolIsATransportFailure(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 1)
	defer stop()
	c := newTestClient(t, addr)

	c.pool.poolMu.Lock()
	c.pool.conns = nil
	c.pool.poolMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`))
	_, err := c.SendRequest(ctx, req)
	if err == nil {
		t.Fatal("a pool with no connections answered a request")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure) {
		t.Fatalf("error = %v, want a transport failure so the upstream is cordoned", err)
	}
	// The message matters as much as the code. Without the pre-dispatch check
	// the request reaches a handler with a nil connection, the bounded-call
	// recovery turns the nil dereference into a transport failure too, and the
	// operator is handed a panic trace instead of "no available connections".
	if !strings.Contains(err.Error(), "no available connections") {
		t.Fatalf("error = %v, want it to say the pool has no connections", err)
	}
}

// QueryClient must hand back the query surface of a live pooled connection.
// Returning nil would make every eth_query* call panic at the call site.
func TestQueryClient_ReturnsALivePooledConnection(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 1)
	defer stop()
	c := newTestClient(t, addr)

	if c.QueryClient() == nil {
		t.Fatal("QueryClient returned nil for a healthy pool")
	}

	c.pool.poolMu.Lock()
	c.pool.conns = nil
	c.pool.poolMu.Unlock()

	if c.QueryClient() != nil {
		t.Fatal("QueryClient returned a client from an empty pool")
	}
}
