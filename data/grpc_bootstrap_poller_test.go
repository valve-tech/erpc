package data

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// newPollerConnector builds a connector wired to one fake BDS server under the
// given network id, without running the bootstrap. The poller and its
// bookkeeping are what these tests exercise.
func newPollerConnector(t *testing.T, networkId string, cli clients.GrpcBdsClient) *GrpcConnector {
	t.Helper()
	lg := zerolog.Nop()
	return &GrpcConnector{
		id:                 "grpc-poll",
		logger:             &lg,
		appCtx:             context.Background(),
		clientByNetwork:    map[string]clients.GrpcBdsClient{networkId: cli},
		earliestByNetwork:  map[string]uint64{},
		latestByNetwork:    map[string]uint64{},
		finalizedByNetwork: map[string]uint64{},
		latestTsByNetwork:  map[string]int64{},
	}
}

// dialFake builds a real BDS gRPC client against addr.
func dialFake(t *testing.T, addr string) clients.GrpcBdsClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	lg := zerolog.Nop()
	parsed, err := url.Parse("grpc://" + addr)
	require.NoError(t, err)
	cli, err := clients.NewGrpcBdsClient(ctx, &lg, "<cache>", nil, parsed, 1)
	require.NoError(t, err)
	return cli
}

// ─────────────────────────── fetchGrpcServers ───────────────────────────

// The bootstrap endpoint's documented shape is a JSON array of server URLs.
// Getting this wrong means the connector starts with no servers and every
// cache read misses, silently.
func TestFetchGrpcServers_ParsesAJsonArray(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `["grpc://a:1","grpc://b:2"]`)
	}))
	t.Cleanup(srv.Close)

	lg := zerolog.Nop()
	got, err := fetchGrpcServers(context.Background(), &lg, srv.URL)

	require.NoError(t, err)
	assert.Equal(t, []string{"grpc://a:1", "grpc://b:2"}, got)
}

// A bootstrap endpoint may answer with plain newline-separated text. Blank
// lines and surrounding whitespace must not become empty server entries — an
// empty entry becomes an unparsable URL and a permanently failing task.
func TestFetchGrpcServers_ParsesNewlineSeparatedText(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "  grpc://a:1  \n\n grpc://b:2 \n")
	}))
	t.Cleanup(srv.Close)

	lg := zerolog.Nop()
	got, err := fetchGrpcServers(context.Background(), &lg, srv.URL)

	require.NoError(t, err)
	assert.Equal(t, []string{"grpc://a:1", "grpc://b:2"}, got)
}

// An empty JSON array is not a valid server list. Today the parser falls
// through to the newline branch and yields one entry, the literal "[]". That
// is a bootstrap that reports success with a server nobody can dial.
func TestFetchGrpcServers_AnEmptyJsonArrayFallsThroughToTheTextBranch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(srv.Close)

	lg := zerolog.Nop()
	got, err := fetchGrpcServers(context.Background(), &lg, srv.URL)

	require.NoError(t, err)
	assert.Equal(t, []string{"[]"}, got,
		"today an empty array is read as one literal server; a fix should return no servers")
}

// An empty body must produce no servers rather than one empty entry.
func TestFetchGrpcServers_AnEmptyBodyYieldsNoServers(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	t.Cleanup(srv.Close)

	lg := zerolog.Nop()
	got, err := fetchGrpcServers(context.Background(), &lg, srv.URL)

	require.NoError(t, err)
	assert.Empty(t, got)
}

// A bootstrap endpoint that is down must surface the transport error, not an
// empty list. An empty list looks like "this cluster has no servers".
func TestFetchGrpcServers_ReportsATransportFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	addr := srv.URL
	srv.Close() // nothing listens on addr any more

	lg := zerolog.Nop()
	got, err := fetchGrpcServers(context.Background(), &lg, addr)

	require.Error(t, err)
	assert.Nil(t, got)
}

// A malformed endpoint must fail while building the request, before any I/O.
func TestFetchGrpcServers_RejectsAMalformedEndpoint(t *testing.T) {
	t.Parallel()
	lg := zerolog.Nop()

	got, err := fetchGrpcServers(context.Background(), &lg, "http://\x7f/bad")

	require.Error(t, err)
	assert.Nil(t, got)
}

// A cancelled context must stop the fetch rather than hang.
func TestFetchGrpcServers_HonoursACancelledContext(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `["grpc://a:1"]`)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	lg := zerolog.Nop()
	got, err := fetchGrpcServers(ctx, &lg, srv.URL)

	require.Error(t, err)
	assert.Nil(t, got)
}

// ─────────────────────── bootstrap through NewGrpcConnector ───────────────────────

// The whole bootstrap path, end to end: discover one server over HTTP, dial it
// over gRPC, probe eth_chainId, and register the client under the network id
// the probe reported. An operator configures only the bootstrap URL, so every
// step after it has to work unattended.
func TestNewGrpcConnector_BootstrapsFromAnHttpEndpointAndProbesTheChain(t *testing.T) {
	t.Parallel()
	addr, fake, _ := startFakeBdsServer(t, 137)
	fake.SetBlock("latest", 500, 1700000000)

	boot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, fmt.Sprintf(`["grpc://%s"]`, addr))
	}))
	t.Cleanup(boot.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lg := zerolog.Nop()
	cfg := &common.GrpcConnectorConfig{
		Bootstrap:  boot.URL,
		GetTimeout: common.Duration(10 * time.Second),
		PoolSize:   1,
	}
	g, err := NewGrpcConnector(ctx, &lg, "grpc-boot", cfg)
	require.NoError(t, err)

	g.mu.RLock()
	cli := g.clientByNetwork["evm:137"]
	networks := len(g.clientByNetwork)
	g.mu.RUnlock()

	require.NotNil(t, cli, "the probed chain id must decide the network the client is registered under")
	assert.Equal(t, 1, networks)
	assert.Positive(t, fake.ChainIdCalls(), "bootstrap must probe eth_chainId")
	assert.NoError(t, g.checkReady())
}

// A bootstrap endpoint that is down must fail construction. Returning a usable
// connector with no servers would make every cache read a silent miss.
func TestNewGrpcConnector_ABrokenBootstrapEndpointFailsConstruction(t *testing.T) {
	t.Parallel()
	boot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := boot.URL
	boot.Close()

	lg := zerolog.Nop()
	g, err := NewGrpcConnector(context.Background(), &lg, "grpc-boot-fail", &common.GrpcConnectorConfig{
		Bootstrap: url,
	})

	require.Error(t, err)
	assert.Nil(t, g)
	assert.Contains(t, err.Error(), "bootstrap failed")
}

// With NetworkId configured the connector must skip the chain probe entirely.
// That is the only way an SVM server can be registered: it answers eth_chainId
// with Unimplemented, so a probe would leave the connector with no clients.
func TestNewGrpcConnector_AConfiguredNetworkIdSkipsTheChainProbe(t *testing.T) {
	t.Parallel()
	addr, fake, _ := startFakeBdsServer(t, 137)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lg := zerolog.Nop()
	g, err := NewGrpcConnector(ctx, &lg, "grpc-static", &common.GrpcConnectorConfig{
		Servers:   []string{"grpc://" + addr},
		NetworkId: "svm:mainnet",
		PoolSize:  1,
	})
	require.NoError(t, err)

	g.mu.RLock()
	cli := g.clientByNetwork["svm:mainnet"]
	g.mu.RUnlock()

	require.NotNil(t, cli, "a configured networkId must register the client under exactly that id")
	assert.Zero(t, fake.ChainIdCalls(), "a configured networkId must not cost a probe round trip")
}

// A server URL that does not parse is a configuration mistake, not a transient
// fault. The task must be marked fatal so the initializer stops retrying it
// forever, and the connector must come up with no client for it.
func TestNewGrpcConnector_AnUnparsableServerUrlRegistersNoClient(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lg := zerolog.Nop()
	g, err := NewGrpcConnector(ctx, &lg, "grpc-bad-url", &common.GrpcConnectorConfig{
		Servers:  []string{"grpc://\x7f:1"},
		PoolSize: 1,
	})
	require.NoError(t, err, "one bad server must not stop the connector from being built")

	g.mu.RLock()
	n := len(g.clientByNetwork)
	g.mu.RUnlock()
	assert.Zero(t, n)
	assert.Error(t, g.checkReady(), "a connector with no clients must report itself not ready")
}

// Two servers answering for the same chain are a misconfiguration. Both are
// probed, and exactly one client survives — the connector holds one client per
// network, never a list.
//
// Coverage note: this reaches the duplicate-drop branch, but no mutation of
// that branch is observable from outside. Dropping the guard would overwrite
// the first client with the second, and both outcomes leave one client the
// caller cannot tell apart.
func TestNewGrpcConnector_TwoServersForOneChainLeaveOneClient(t *testing.T) {
	t.Parallel()
	addrA, fakeA, _ := startFakeBdsServer(t, 42161)
	addrB, fakeB, _ := startFakeBdsServer(t, 42161)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lg := zerolog.Nop()
	g, err := NewGrpcConnector(ctx, &lg, "grpc-dup", &common.GrpcConnectorConfig{
		Servers:  []string{"grpc://" + addrA, "grpc://" + addrB},
		PoolSize: 1,
	})
	require.NoError(t, err)

	g.mu.RLock()
	n := len(g.clientByNetwork)
	g.mu.RUnlock()
	assert.Equal(t, 1, n, "two servers for one chain must leave exactly one registered client")
	assert.Positive(t, fakeA.ChainIdCalls()+fakeB.ChainIdCalls(), "both servers must be probed")
}

// Servers answering for different chains must each get their own client, keyed
// by the chain the probe reported. Registering them under one key would send a
// network's reads to another chain's reader.
func TestNewGrpcConnector_RegistersOneClientPerChain(t *testing.T) {
	t.Parallel()
	addrA, _, _ := startFakeBdsServer(t, 1)
	addrB, fakeB, _ := startFakeBdsServer(t, 42161)
	fakeB.SetBlock("latest", 777, 1700000000)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lg := zerolog.Nop()
	g, err := NewGrpcConnector(ctx, &lg, "grpc-multi", &common.GrpcConnectorConfig{
		Servers:  []string{"grpc://" + addrA, "grpc://" + addrB},
		PoolSize: 1,
	})
	require.NoError(t, err)

	g.mu.RLock()
	_, hasMainnet := g.clientByNetwork["evm:1"]
	_, hasArbitrum := g.clientByNetwork["evm:42161"]
	n := len(g.clientByNetwork)
	g.mu.RUnlock()

	assert.Equal(t, 2, n)
	assert.True(t, hasMainnet, "the mainnet server must be registered under evm:1")
	assert.True(t, hasArbitrum, "the arbitrum server must be registered under evm:42161")

	// The client registered under evm:42161 must really be the arbitrum
	// server: poll and check the head it reports.
	g.pollBlockHeadsOnce(ctx)
	g.mu.RLock()
	defer g.mu.RUnlock()
	assert.Equal(t, uint64(777), g.latestByNetwork["evm:42161"])
}

// A configured header must reach the wire on every call the connector makes.
// This is how an edge-api auth key gets to the backing reader; without it the
// server answers Unauthenticated and the cache never warms.
func TestNewGrpcConnector_AppliesConfiguredHeaders(t *testing.T) {
	t.Parallel()
	addr, fake, _ := startFakeBdsServer(t, 1)
	fake.SetBlock("latest", 900, 1700000000)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lg := zerolog.Nop()
	g, err := NewGrpcConnector(ctx, &lg, "grpc-hdr", &common.GrpcConnectorConfig{
		Servers:  []string{"grpc://" + addr},
		Headers:  map[string]string{"authorization": "Bearer t"},
		PoolSize: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"authorization": "Bearer t"}, g.headers)

	g.pollBlockHeadsOnce(ctx)
	assert.Equal(t, []string{"Bearer t"}, fake.LastMetadata().Get("authorization"),
		"the configured header must reach the wire, not just the connector's own map")
}

// ─────────────────────────── the head poller ───────────────────────────

// One poll pass must refresh all three tags and the head timestamp. earliest
// powers the fast-miss rejection in Get and the head timestamp powers the
// realtime cache-age guard, so a poll that half-runs leaves both wrong.
func TestPollBlockHeadsOnce_RefreshesAllThreeTags(t *testing.T) {
	t.Parallel()
	addr, fake, _ := startFakeBdsServer(t, 1)
	fake.SetBlock("earliest", 10, 1600000000)
	fake.SetBlock("latest", 900, 1700000000)
	fake.SetBlock("finalized", 880, 1699999000)

	g := newPollerConnector(t, "evm:1", dialFake(t, addr))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g.pollBlockHeadsOnce(ctx)

	g.mu.RLock()
	defer g.mu.RUnlock()
	assert.Equal(t, uint64(10), g.earliestByNetwork["evm:1"])
	assert.Equal(t, uint64(900), g.latestByNetwork["evm:1"])
	assert.Equal(t, uint64(880), g.finalizedByNetwork["evm:1"])
	assert.Equal(t, int64(1700000000), g.latestTsByNetwork["evm:1"])
}

// The head timestamp the poller stores is what CacheHeadReporter serves to the
// realtime cache-age guard. Before the first successful poll it must report
// "unknown" so the guard fails open instead of judging freshness by zero.
func TestCacheLatestBlockTimestamp_UnknownUntilThePollerRuns(t *testing.T) {
	t.Parallel()
	addr, fake, _ := startFakeBdsServer(t, 1)
	fake.SetBlock("latest", 900, 1700000000)

	g := newPollerConnector(t, "evm:1", dialFake(t, addr))

	_, known := g.CacheLatestBlockTimestamp("evm:1")
	assert.False(t, known, "no poll has run yet, so the head time must be unknown")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g.pollBlockHeadsOnce(ctx)

	ts, known := g.CacheLatestBlockTimestamp("evm:1")
	assert.True(t, known)
	assert.Equal(t, int64(1700000000), ts)
}

// A transient failure on one tag must leave that tag's previous value alone.
// Zeroing earliest on a blip would disable the fast-miss rejection; zeroing
// latest would make the connector claim it trails the chain by decades.
func TestPollBlockHeadsOnce_ATransientFailureKeepsThePreviousValues(t *testing.T) {
	t.Parallel()
	addr, fake, _ := startFakeBdsServer(t, 1)
	fake.SetBlock("earliest", 10, 1600000000)
	fake.SetBlock("latest", 900, 1700000000)
	fake.SetBlock("finalized", 880, 1699999000)

	g := newPollerConnector(t, "evm:1", dialFake(t, addr))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	g.pollBlockHeadsOnce(ctx)

	fake.FailTag("earliest", codes.Unavailable, "reader restarting")
	fake.FailTag("finalized", codes.Unavailable, "reader restarting")
	fake.SetBlock("latest", 950, 1700000600)
	g.pollBlockHeadsOnce(ctx)

	g.mu.RLock()
	defer g.mu.RUnlock()
	assert.Equal(t, uint64(10), g.earliestByNetwork["evm:1"], "a failed earliest must not zero the known value")
	assert.Equal(t, uint64(880), g.finalizedByNetwork["evm:1"], "a failed finalized must not zero the known value")
	assert.Equal(t, uint64(950), g.latestByNetwork["evm:1"], "the tag that did answer must still advance")
}

// A server that answers with no block at all is also a non-answer. It must not
// be read as block zero.
func TestPollBlockHeadsOnce_AnEmptyBlockAnswerKeepsThePreviousValue(t *testing.T) {
	t.Parallel()
	addr, fake, _ := startFakeBdsServer(t, 1)
	fake.SetBlock("latest", 900, 1700000000)

	g := newPollerConnector(t, "evm:1", dialFake(t, addr))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	g.pollBlockHeadsOnce(ctx)
	fake.ClearTag("latest")
	g.pollBlockHeadsOnce(ctx)

	g.mu.RLock()
	defer g.mu.RUnlock()
	assert.Equal(t, uint64(900), g.latestByNetwork["evm:1"], "an empty answer must not be read as block 0")
}

// The head timestamp moves in lockstep with the head number. A latest block
// whose timestamp is unknown must reset the reported head time to unknown,
// rather than leaving the previous block's time attached to a newer head.
func TestPollBlockHeadsOnce_AnAdvancedHeadWithNoTimestampReportsUnknown(t *testing.T) {
	t.Parallel()
	addr, fake, _ := startFakeBdsServer(t, 1)
	fake.SetBlock("latest", 900, 1700000000)

	g := newPollerConnector(t, "evm:1", dialFake(t, addr))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	g.pollBlockHeadsOnce(ctx)

	fake.SetBlock("latest", 950, 0)
	g.pollBlockHeadsOnce(ctx)

	ts, known := g.CacheLatestBlockTimestamp("evm:1")
	assert.False(t, known, "the head advanced with no time, so the reported head time must be unknown")
	assert.Zero(t, ts)

	g.mu.RLock()
	defer g.mu.RUnlock()
	assert.Equal(t, uint64(950), g.latestByNetwork["evm:1"])
}

// The poller speaks EVM. A Solana network must be skipped outright: the three
// tag calls would be answered Unimplemented forever, three wasted round trips
// per interval for values that stay unknown either way.
func TestPollBlockHeadsOnce_SkipsANonEvmNetwork(t *testing.T) {
	t.Parallel()
	addr, fake, _ := startFakeBdsServer(t, 1)
	fake.SetBlock("latest", 900, 1700000000)

	g := newPollerConnector(t, "svm:mainnet", dialFake(t, addr))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g.pollBlockHeadsOnce(ctx)

	assert.Zero(t, fake.BlockCalls(), "an SVM network must cost the poller no round trips")
	g.mu.RLock()
	defer g.mu.RUnlock()
	assert.Zero(t, g.latestByNetwork["svm:mainnet"])
}

// The earliest block the poller learns is what makes Get reject a request for
// a pruned block without a round trip. Pin the two sides of that boundary.
func TestGrpcConnector_EarliestFromThePollerRejectsAPrunedBlock(t *testing.T) {
	t.Parallel()
	addr, fake, _ := startFakeBdsServer(t, 1)
	fake.SetBlock("earliest", 100, 1600000000)
	fake.SetBlock("latest", 900, 1700000000)

	g := newPollerConnector(t, "evm:1", dialFake(t, addr))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	g.initializer = &util.Initializer{}
	g.pollBlockHeadsOnce(ctx)

	below := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x32",false]}`))
	below.SetNetwork(fakeBlockNumberNetwork{})
	below.SetEvmBlockNumber(uint64(50))
	out, err := g.Get(ctx, ConnectorMainIndex, "pk", "rk", below)
	assert.Nil(t, out)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound),
		"a block below earliest must be rejected without a round trip, got %v", err)

	callsAfterReject := fake.BlockCalls()

	above := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1f4",false]}`))
	above.SetNetwork(fakeBlockNumberNetwork{})
	above.SetEvmBlockNumber(uint64(500))
	fake.SetBlock("0x1f4", 500, 1650000000)
	_, err = g.Get(ctx, ConnectorMainIndex, "pk", "rk", above)
	require.NoError(t, err)
	assert.Greater(t, fake.BlockCalls(), callsAfterReject, "a block at or above earliest must reach the server")
}

// ─────────────────────── the poller's lifecycle ───────────────────────

// The poller goroutine must exit when the connector's context ends. A poller
// that outlives its connector keeps a gRPC connection and a ticker alive for
// the life of the process.
func TestStartBlockHeadPoller_StopsWithItsContext(t *testing.T) {
	t.Parallel()
	addr, fake, _ := startFakeBdsServer(t, 1)
	fake.SetBlock("latest", 900, 1700000000)

	g := newPollerConnector(t, "evm:1", dialFake(t, addr))

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		g.startBlockHeadPoller(ctx)
	}()

	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the head poller did not stop when its context was cancelled")
	}
}
