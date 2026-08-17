package evm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// The other cache tests in this package assemble an EvmJsonRpcCache by hand —
// they set the struct fields directly and hand it a data.MockConnector. That
// leaves the whole construction path dark: NewEvmJsonRpcCache builds the
// connectors, binds each policy to the connector its config names, copies the
// connector's tags onto the policy, and configures compression. None of that is
// exercised by a hand-built struct.
//
// cacheFixture closes that gap. It builds the cache through its real
// constructor, over a REAL data connector (the in-memory one) and a REAL policy
// set, so a value written by Set is a value another goroutine can read back
// with Get. A mock connector cannot show that: it returns whatever the test
// told it to, so a Set that silently stores the wrong bytes still "reads back"
// correctly.
//
// How to reuse it:
//
//	f := newCacheFixture(t, cacheConfig(
//	    cacheConns(memoryConnector("mem")),
//	    cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
//	))
//	require.NoError(t, f.cache.Set(ctx, req, resp))
//	got := f.mustGetEventually(ctx, req)
//
// The in-memory connector is backed by ristretto, whose writes land
// asynchronously, so reads go through mustGetEventually / getEventually rather
// than a bare Get. Those poll on a short bounded deadline; they never wait on a
// background poller.
type cacheFixture struct {
	t     *testing.T
	cache *EvmJsonRpcCache
	conns map[string]data.Connector
}

// newCacheFixture builds the cache through NewEvmJsonRpcCache and fails the
// test if construction errors. Use newCacheFixtureErr when the error IS the
// thing under test.
func newCacheFixture(t *testing.T, cfg *common.CacheConfig) *cacheFixture {
	t.Helper()
	f, err := newCacheFixtureErr(t, cfg)
	require.NoError(t, err)
	return f
}

// newCacheFixtureErr returns the construction error instead of failing, and
// also re-creates each configured connector so a test can inspect the bytes
// that actually landed in storage.
func newCacheFixtureErr(t *testing.T, cfg *common.CacheConfig) (*cacheFixture, error) {
	t.Helper()
	logger := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.WarnLevel)
	cache, err := NewEvmJsonRpcCache(context.Background(), &logger, cfg)
	if err != nil {
		return nil, err
	}
	f := &cacheFixture{t: t, cache: cache, conns: map[string]data.Connector{}}
	// Index the connectors the cache built, by walking its policies. Reaching
	// them this way (rather than dialing a second set) means a test inspects
	// the SAME storage the cache wrote to.
	for _, p := range cache.policies {
		c := p.GetConnector()
		f.conns[c.Id()] = c
	}
	return f, nil
}

// connector returns the connector the cache bound to its policies under id.
func (f *cacheFixture) connector(id string) data.Connector {
	f.t.Helper()
	c, ok := f.conns[id]
	require.Truef(f.t, ok, "no connector %q is bound to any policy", id)
	return c
}

// getEventually polls cache.Get until it returns a response or the deadline
// passes. It returns nil when nothing ever landed.
func (f *cacheFixture) getEventually(ctx context.Context, req *common.NormalizedRequest) *common.NormalizedResponse {
	f.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := f.cache.Get(ctx, req)
		require.NoError(f.t, err)
		if resp != nil {
			return resp
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// mustGetEventually is getEventually with a failure when nothing lands.
func (f *cacheFixture) mustGetEventually(ctx context.Context, req *common.NormalizedRequest) string {
	f.t.Helper()
	resp := f.getEventually(ctx, req)
	require.NotNil(f.t, resp, "nothing was readable from the cache within the deadline")
	jrr, err := resp.JsonRpcResponse()
	require.NoError(f.t, err)
	return string(jrr.GetResultBytes())
}

// --- config builders ---

// memoryConnector describes one real in-memory connector, optionally tagged.
func memoryConnector(id string, tags ...string) *common.ConnectorConfig {
	return &common.ConnectorConfig{
		Id:     id,
		Driver: common.DriverMemory,
		Tags:   tags,
		Memory: &common.MemoryConnectorConfig{
			MaxItems:     10_000,
			MaxTotalSize: "16MB",
		},
	}
}

// cachePolicyCfg describes one cache policy bound to connectorId for one method. It
// matches every network and the unknown finality state, which is what a
// request carrying no network resolves to.
func cachePolicyCfg(connectorId, method string) *common.CachePolicyConfig {
	ttl := 5 * time.Minute
	return &common.CachePolicyConfig{
		Connector: connectorId,
		Network:   "*",
		Method:    method,
		Finality:  common.DataFinalityStateUnknown,
		TTL:       common.FixedDuration(ttl),
	}
}

// cacheConfig assembles connectors and policies into a CacheConfig. Connectors
// come first, policies second; both are variadic through the two helpers above.
func cacheConfig(conns []*common.ConnectorConfig, policies []*common.CachePolicyConfig) *common.CacheConfig {
	return &common.CacheConfig{Connectors: conns, Policies: policies}
}

func cacheConns(c ...*common.ConnectorConfig) []*common.ConnectorConfig { return c }

func cachePolicies(p ...*common.CachePolicyConfig) []*common.CachePolicyConfig { return p }

// withCompression turns compression on for cfg.
func withCompression(cfg *common.CacheConfig, level string, threshold int) *common.CacheConfig {
	enabled := true
	cfg.Compression = &common.CompressionConfig{
		Enabled:   &enabled,
		Algorithm: "zstd",
		ZstdLevel: level,
		Threshold: threshold,
	}
	return cfg
}

// --- request/response builders ---

// cacheRequest builds a normalized request from a JSON-RPC body.
func cacheRequest(body string) *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(body))
}

// cacheResponse pairs a raw JSON result with req, which is the shape Set
// expects.
func cacheResponse(t *testing.T, req *common.NormalizedRequest, raw string) *common.NormalizedResponse {
	t.Helper()
	resp, err := jsonResult(req, raw)
	require.NoError(t, err)
	return resp
}
