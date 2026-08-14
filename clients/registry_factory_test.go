package clients

import (
	"context"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// registryWithPools builds a client registry that owns the named proxy pools.
func registryWithPools(t *testing.T, cfgs []*common.ProxyPoolConfig) *ClientRegistry {
	t.Helper()
	lg := zerolog.Nop()
	pools, err := NewProxyPoolRegistry(cfgs, &lg)
	if err != nil {
		t.Fatalf("NewProxyPoolRegistry: %v", err)
	}
	return NewClientRegistry(&lg, "test-project", pools, nil)
}

// upstreamWith builds an upstream of a registered fake family.
func upstreamWith(t *testing.T, family, id, endpoint string) common.Upstream {
	t.Helper()
	registerClientFamily(t, &fakeClientFamily{name: common.NetworkArchitecture(family)})
	ups := common.NewFakeUpstream(id)
	ups.Config().Type = common.UpstreamType(family)
	ups.Config().Endpoint = endpoint
	return ups
}

// An upstream with no endpoint must be refused, and the message must say the
// endpoint is MISSING.
//
// The scheme switch downstream also rejects it — with "unsupported endpoint
// scheme: " — so the refusal is not what is at stake here; the diagnosis is.
// That message sends an operator hunting a scheme typo in a field they never
// filled in.
func TestCreateClient_EmptyEndpointIsRefusedAsMissingNotAsABadScheme(t *testing.T) {
	registerClientFamily(t, &fakeClientFamily{name: "noendpoint"})
	ups := common.NewFakeUpstream("noendpoint-1")
	ups.Config().Type = common.UpstreamType("noendpoint")
	ups.Config().Endpoint = ""

	c, err := registryWithPools(t, nil).CreateClient(context.Background(), ups)
	if err == nil {
		t.Fatal("an upstream with no endpoint produced a client")
	}
	if c != nil {
		t.Fatal("a client was returned alongside the error")
	}
	if !strings.Contains(err.Error(), "endpoint is required") {
		t.Fatalf("error = %v, want it to say the endpoint is required", err)
	}
}

// A proxy pool named in config but never defined is a config typo. Falling
// back to direct requests would silently send traffic out of the wrong egress
// address — which is the one thing the pool exists to control.
func TestCreateClient_UnknownProxyPoolIsRefusedNotSilentlyBypassed(t *testing.T) {
	registerClientFamily(t, &fakeClientFamily{name: "proxied"})
	ups := common.NewFakeUpstream("proxied-1")
	ups.Config().Type = common.UpstreamType("proxied")
	ups.Config().Endpoint = "http://proxied-1.localhost:8545"
	ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{ProxyPool: "pool-that-does-not-exist"}

	c, err := registryWithPools(t, nil).CreateClient(context.Background(), ups)
	if err == nil {
		t.Fatal("an unknown proxy pool was silently bypassed and a direct client was built")
	}
	if c != nil {
		t.Fatal("a client was returned alongside the error")
	}
	if !strings.Contains(err.Error(), "proxy pool") {
		t.Fatalf("error = %v, want it to name the proxy pool", err)
	}
}

// A pool that IS defined must be found and threaded into the client. Without
// this the negative test above would pass on a factory that rejects every
// pool.
func TestCreateClient_DefinedProxyPoolIsAccepted(t *testing.T) {
	registerClientFamily(t, &fakeClientFamily{name: "proxiedok"})
	ups := common.NewFakeUpstream("proxiedok-1")
	ups.Config().Type = common.UpstreamType("proxiedok")
	ups.Config().Endpoint = "http://proxiedok-1.localhost:8545"
	ups.Config().JsonRpc = &common.JsonRpcUpstreamConfig{ProxyPool: "pool1"}

	reg := registryWithPools(t, []*common.ProxyPoolConfig{{ID: "pool1", Urls: []string{"http://myproxy:8080"}}})
	c, err := reg.CreateClient(context.Background(), ups)
	if err != nil {
		t.Fatalf("CreateClient with a defined pool: %v", err)
	}
	hc, ok := c.(*GenericHttpJsonRpcClient)
	if !ok {
		t.Fatalf("client type = %T, want *GenericHttpJsonRpcClient", c)
	}
	if hc.proxyPool == nil {
		t.Fatal("the client was built without its proxy pool; traffic would leave the wrong address")
	}
}

// A ws:// endpoint must produce a WebSocket client, not an HTTP one. The
// scheme is the only thing that selects the transport, so a factory that
// ignored it would open a plain POST against a socket endpoint.
func TestCreateClient_WebsocketSchemeSelectsTheWebsocketClient(t *testing.T) {
	// The dial fails (nothing is listening) and the client says so in a log
	// rather than an error, on purpose: an upstream that is down at boot must
	// still be constructed so it can reconnect later.
	ups := upstreamWith(t, "wschain", "wschain-1", "ws://127.0.0.1:1/ws")

	c, err := registryWithPools(t, nil).CreateClient(context.Background(), ups)
	if err != nil {
		t.Fatalf("CreateClient for a ws endpoint: %v", err)
	}
	if c.GetType() != ClientTypeWsJsonRpc {
		t.Fatalf("client type = %v, want %v", c.GetType(), ClientTypeWsJsonRpc)
	}
}

// grpc+bds must produce the gRPC client. Getting this wrong sends JSON-RPC
// over a port that speaks protobuf.
func TestCreateClient_GrpcSchemeSelectsTheGrpcClient(t *testing.T) {
	ups := upstreamWith(t, "grpcchain", "grpcchain-1", "grpc+bds://127.0.0.1:50051")

	c, err := registryWithPools(t, nil).CreateClient(context.Background(), ups)
	if err != nil {
		t.Fatalf("CreateClient for a grpc+bds endpoint: %v", err)
	}
	if c.GetType() != ClientTypeGrpcBds {
		t.Fatalf("client type = %v, want %v", c.GetType(), ClientTypeGrpcBds)
	}
}

// GetOrCreateClient must return the SAME client for the same upstream and a
// DIFFERENT one for a different upstream.
//
// Both halves are needed. The first alone passes on a registry keyed by
// nothing at all — which would hand every upstream the first upstream's
// client, sending all traffic to one node while the dashboard shows a healthy
// pool. The second alone passes on a registry that caches nothing and leaks a
// client per call.
func TestGetOrCreateClient_CachesPerUpstreamNotGlobally(t *testing.T) {
	registerClientFamily(t, &fakeClientFamily{name: "cached"})
	newUps := func(id, endpoint string) common.Upstream {
		u := common.NewFakeUpstream(id)
		u.Config().Type = common.UpstreamType("cached")
		u.Config().Endpoint = endpoint
		return u
	}
	upsA := newUps("cached-a", "http://cached-a.localhost:8545")
	upsB := newUps("cached-b", "http://cached-b.localhost:8545")
	reg := registryWithPools(t, nil)

	firstA, err := reg.GetOrCreateClient(context.Background(), upsA)
	if err != nil {
		t.Fatalf("first GetOrCreateClient(A): %v", err)
	}
	secondA, err := reg.GetOrCreateClient(context.Background(), upsA)
	if err != nil {
		t.Fatalf("second GetOrCreateClient(A): %v", err)
	}
	if firstA != secondA {
		t.Fatal("GetOrCreateClient built a second client for the same upstream")
	}

	clientB, err := reg.GetOrCreateClient(context.Background(), upsB)
	if err != nil {
		t.Fatalf("GetOrCreateClient(B): %v", err)
	}
	if clientB == firstA {
		t.Fatal("a second upstream was handed the first upstream's client; all traffic would go to one node")
	}
}

// Concurrent callers that all miss the cache must still end up with ONE
// client. This is the race the SHARED sync.Once exists for: with a per-call
// Once every caller built its own client, and each loser leaked its goroutines
// (shutdown waiter, ping loop, batch timer) for the process lifetime.
//
// The identity check alone cannot prove it — the race window is short enough
// that the goroutines usually serialise anyway. So this also asserts the
// structural invariant: exactly one memo entry per upstream, which is what
// makes a second build impossible rather than merely unlikely.
func TestGetOrCreateClient_ConcurrentCallersShareOneMemoAndOneClient(t *testing.T) {
	ups := upstreamWith(t, "raced", "raced-1", "http://raced-1.localhost:8545")
	reg := registryWithPools(t, nil)

	const callers = 16
	got := make([]ClientInterface, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	done := make(chan struct{}, callers)

	for i := 0; i < callers; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			<-start
			got[i], errs[i] = reg.GetOrCreateClient(context.Background(), ups)
		}(i)
	}
	close(start)
	for i := 0; i < callers; i++ {
		<-done
	}

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	for i := 1; i < callers; i++ {
		if got[i] != got[0] {
			t.Fatalf("caller %d got a different client; concurrent misses each built one", i)
		}
	}

	memos := 0
	reg.clientCreations.Range(func(_, _ any) bool { memos++; return true })
	if memos != 1 {
		t.Fatalf("%d build memos for one upstream, want 1 — the sync.Once is not shared", memos)
	}
	cached := 0
	reg.clients.Range(func(_, _ any) bool { cached++; return true })
	if cached != 1 {
		t.Fatalf("%d cached clients for one upstream, want 1", cached)
	}
}

// A failed creation must NOT be cached as a success. Caching a nil client
// would hand every later caller a nil to dereference.
func TestCreateClient_FailureIsNotCachedAsAClient(t *testing.T) {
	registerClientFamily(t, &fakeClientFamily{name: "badscheme"})
	ups := common.NewFakeUpstream("badscheme-1")
	ups.Config().Type = common.UpstreamType("badscheme")
	ups.Config().Endpoint = "ftp://badscheme-1.localhost"

	reg := registryWithPools(t, nil)
	if _, err := reg.CreateClient(context.Background(), ups); err == nil {
		t.Fatal("an unsupported scheme produced a client")
	}
	c, err := reg.GetOrCreateClient(context.Background(), ups)
	if err == nil {
		t.Fatal("the failed creation was cached as a success")
	}
	if c != nil {
		t.Fatal("a nil client was cached and handed to a later caller")
	}
}

// An empty pool ID means "no proxy", not "pool not found". Erroring here would
// refuse every upstream that simply does not use a proxy.
func TestGetPool_EmptyIdMeansDirectRequests(t *testing.T) {
	lg := zerolog.Nop()
	reg, err := NewProxyPoolRegistry(nil, &lg)
	if err != nil {
		t.Fatalf("NewProxyPoolRegistry: %v", err)
	}
	pool, err := reg.GetPool("")
	if err != nil {
		t.Fatalf("GetPool(\"\") = %v, want no error", err)
	}
	if pool != nil {
		t.Fatal("GetPool(\"\") returned a pool; an unproxied upstream would be routed through it")
	}
}

// A pool config with no URLs is a config mistake. Accepting it would build a
// pool that can hand out no client at all.
func TestNewProxyPoolRegistry_RejectsAPoolWithNoUrls(t *testing.T) {
	lg := zerolog.Nop()
	_, err := NewProxyPoolRegistry([]*common.ProxyPoolConfig{{ID: "empty"}}, &lg)
	if err == nil {
		t.Fatal("a proxy pool with no URLs was accepted")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error = %v, want it to name the offending pool", err)
	}
}

// An unparseable proxy URL must be refused at boot, not at first request.
func TestNewProxyPoolRegistry_RejectsAnInvalidProxyUrl(t *testing.T) {
	lg := zerolog.Nop()
	_, err := NewProxyPoolRegistry([]*common.ProxyPoolConfig{{ID: "bad", Urls: []string{"://not a url"}}}, &lg)
	if err == nil {
		t.Fatal("an unparseable proxy URL was accepted")
	}
}

// GetClient must round-robin so load spreads across the configured proxies. A
// pool that always returns the same client concentrates every request on one
// egress address and hits that address's rate limit first.
func TestProxyPool_GetClientRoundRobins(t *testing.T) {
	lg := zerolog.Nop()
	reg, err := NewProxyPoolRegistry([]*common.ProxyPoolConfig{
		{ID: "p", Urls: []string{"http://proxy-a:8080", "http://proxy-b:8080"}},
	}, &lg)
	if err != nil {
		t.Fatalf("NewProxyPoolRegistry: %v", err)
	}
	pool, err := reg.GetPool("p")
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}

	seen := map[interface{}]bool{}
	for i := 0; i < 4; i++ {
		c, err := pool.GetClient()
		if err != nil {
			t.Fatalf("GetClient: %v", err)
		}
		seen[c] = true
	}
	if len(seen) != 2 {
		t.Fatalf("GetClient returned %d distinct clients over 4 calls, want 2", len(seen))
	}
}
