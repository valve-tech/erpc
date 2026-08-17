package evm

import (
	"context"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

const blockBody = `{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1234",false]}`

// The constructor is the only place a policy learns which connector serves it.
// Bind the wrong one and every write goes to storage nobody reads. A real
// connector shows that end to end: the bytes Set stored are the bytes Get
// returns.
func TestNewEvmJsonRpcCache_BindsEachPolicyToItsNamedConnector(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t, cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	))

	req := cacheRequest(blockBody)
	resp := cacheResponse(t, req, `{"number":"0x1234","hash":"0xabc"}`)
	require.NoError(t, f.cache.Set(ctx, req, resp))

	got := f.mustGetEventually(ctx, cacheRequest(blockBody))
	require.JSONEq(t, `{"number":"0x1234","hash":"0xabc"}`, got)
	require.Equal(t, "mem", f.connector("mem").Id())
}

// A policy naming a connector that does not exist is a configuration mistake
// that must stop startup. Accepting it would leave a policy with a nil
// connector, and the first request through it panics in production.
func TestNewEvmJsonRpcCache_PolicyNamingAnUnknownConnectorIsRefused(t *testing.T) {
	_, err := newCacheFixtureErr(t, cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("does-not-exist", "eth_getBlockByNumber")),
	))
	require.Error(t, err)
	require.Contains(t, err.Error(), "does-not-exist",
		"the error must name the connector the operator mistyped")
}

// A connector that cannot be built must fail construction and name itself, so
// an operator can tell which of several connectors is misconfigured.
func TestNewEvmJsonRpcCache_ConnectorBuildFailureNamesTheConnector(t *testing.T) {
	bad := memoryConnector("broken")
	bad.Memory.MaxItems = 0 // the memory connector refuses a zero item budget

	_, err := newCacheFixtureErr(t, cacheConfig(
		cacheConns(bad),
		cachePolicies(cachePolicyCfg("broken", "eth_getBlockByNumber")),
	))
	require.Error(t, err)
	require.Contains(t, err.Error(), "broken",
		"the error must name the connector that failed to build")
}

// An unknown driver must be refused rather than silently skipped. A skipped
// connector produces the "connector not found" error one line later, which
// points the operator at the policy instead of at the driver typo.
func TestNewEvmJsonRpcCache_UnknownDriverIsRefused(t *testing.T) {
	_, err := newCacheFixtureErr(t, cacheConfig(
		cacheConns(&common.ConnectorConfig{Id: "weird", Driver: "no-such-driver"}),
		cachePolicies(cachePolicyCfg("weird", "eth_getBlockByNumber")),
	))
	require.Error(t, err)
	require.Contains(t, err.Error(), "weird")
}

// The constructor copies the connector's tags onto every policy it backs. That
// copy is what lets a `use-upstream` selector gate the cache: a connector fed
// by one data source must not serve a request pinned to another. Without the
// copy the policy has no tags, MatchesUpstreamSelector always says yes, and a
// pinned request reads another source's data.
func TestNewEvmJsonRpcCache_ConnectorTagsGateBothSides(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t, cacheConfig(
		cacheConns(memoryConnector("mem", "systx")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	))

	pinned := func(selector string) *common.NormalizedRequest {
		r := cacheRequest(blockBody)
		r.SetDirectives(&common.RequestDirectives{UseUpstream: selector})
		return r
	}

	// A selector that admits the tag writes and reads normally. This is the
	// control: without it the two misses below could just mean writing is
	// broken.
	admitted := pinned("systx")
	require.NoError(t, f.cache.Set(ctx, admitted, cacheResponse(t, admitted, `{"number":"0x1234"}`)))
	require.JSONEq(t, `{"number":"0x1234"}`, f.mustGetEventually(ctx, pinned("systx")))

	// Read side: a request pinned to a DIFFERENT source must not be served the
	// value that is provably sitting in this connector.
	got, err := f.cache.Get(ctx, pinned("other-source"))
	require.NoError(t, err)
	require.Nil(t, got, "a request pinned away from this connector's tag read its data anyway")

	// Write side: the same request must not store into this connector either.
	excluded := pinned("other-source")
	require.NoError(t, f.cache.Set(ctx, excluded, cacheResponse(t, excluded, `{"number":"0xbeef"}`)))
	require.JSONEq(t, `{"number":"0x1234"}`, f.mustGetEventually(ctx, pinned("systx")),
		"an excluded request overwrote the value stored for the admitted one")
}

// Every configured zstd level must resolve to the level zstd actually uses. A
// silently ignored level means an operator who asked for "best" pays fastest's
// ratio and never learns why the bill did not move.
func TestNewEvmJsonRpcCache_ResolvesEveryConfiguredZstdLevel(t *testing.T) {
	cases := []struct {
		configured string
		want       zstd.EncoderLevel
	}{
		{"", zstd.SpeedFastest},
		{"fastest", zstd.SpeedFastest},
		{"FASTEST", zstd.SpeedFastest}, // case-insensitive
		{"default", zstd.SpeedDefault},
		{"better", zstd.SpeedBetterCompression},
		{"best", zstd.SpeedBestCompression},
		{"nonsense", zstd.SpeedFastest}, // unknown falls back, does not fail
	}
	for _, tc := range cases {
		t.Run("level="+tc.configured, func(t *testing.T) {
			f := newCacheFixture(t, withCompression(cacheConfig(
				cacheConns(memoryConnector("mem")),
				cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
			), tc.configured, 0))
			require.True(t, f.cache.compressionEnabled)
			require.Equal(t, tc.want, f.cache.compressionLevel)
		})
	}
}

// An unset threshold must fall back to 512 bytes, not to 0. A zero threshold
// would compress every tiny result, and a zstd frame around 20 bytes of JSON is
// bigger than the JSON.
func TestNewEvmJsonRpcCache_ThresholdDefaultsTo512(t *testing.T) {
	unset := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	), "fastest", 0))
	require.Equal(t, 512, unset.cache.compressionThreshold)

	set := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	), "fastest", 64))
	require.Equal(t, 64, set.cache.compressionThreshold)
}

// With no compression block the cache must stay in pass-through mode. If it
// armed itself anyway, every cached value would gain a zstd frame that an
// operator reading the store by hand cannot decode.
func TestNewEvmJsonRpcCache_CompressionOffLeavesThePoolsUnbuilt(t *testing.T) {
	f := newCacheFixture(t, cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	))
	require.False(t, f.cache.compressionEnabled)
	require.Nil(t, f.cache.encoderPool)
	require.Nil(t, f.cache.decoderPool)
}

// A compression block with enabled=false must also stay off. Reading only the
// presence of the block would arm compression for an operator who explicitly
// turned it off.
func TestNewEvmJsonRpcCache_CompressionExplicitlyDisabledStaysOff(t *testing.T) {
	disabled := false
	cfg := cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	)
	cfg.Compression = &common.CompressionConfig{Enabled: &disabled, ZstdLevel: "best"}

	f := newCacheFixture(t, cfg)
	require.False(t, f.cache.compressionEnabled)
	require.Nil(t, f.cache.encoderPool)
}

// WithProjectId hands every project its own cache view. The clone must carry
// the shared policies AND the shared compression pools: a clone with a nil
// encoder pool panics on the first compressible write.
func TestWithProjectId_ClonesTheCacheWithoutDisturbingTheOriginal(t *testing.T) {
	f := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	), "better", 64))

	clone := f.cache.WithProjectId("proj-a")

	require.Equal(t, "proj-a", clone.projectId)
	require.Empty(t, f.cache.projectId, "WithProjectId must not label the original")
	require.NotSame(t, f.cache, clone)

	require.Equal(t, f.cache.policies, clone.policies)
	require.True(t, clone.compressionEnabled)
	require.Equal(t, 64, clone.compressionThreshold)
	require.Equal(t, zstd.SpeedBetterCompression, clone.compressionLevel)
	require.Same(t, f.cache.encoderPool, clone.encoderPool)
	require.Same(t, f.cache.decoderPool, clone.decoderPool)

	// Two projects must not share a label.
	other := f.cache.WithProjectId("proj-b")
	require.Equal(t, "proj-b", other.projectId)
	require.Equal(t, "proj-a", clone.projectId)
}

// SetPolicies replaces the policy set wholesale. A cache left with the old
// policies after a reload keeps writing into connectors the operator removed.
func TestSetPolicies_ReplacesThePolicySet(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t, cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	))

	req := cacheRequest(blockBody)
	require.NoError(t, f.cache.Set(ctx, req, cacheResponse(t, req, `{"number":"0x1234"}`)))
	require.JSONEq(t, `{"number":"0x1234"}`, f.mustGetEventually(ctx, cacheRequest(blockBody)))

	f.cache.SetPolicies(nil)

	resp, err := f.cache.Get(ctx, cacheRequest(blockBody))
	require.NoError(t, err)
	require.Nil(t, resp, "a cache with no policies still served a cached value")
	require.Empty(t, f.cache.policies)

	// And a fresh set is honoured, so the test is not just proving nil works.
	f.cache.SetPolicies([]*data.CachePolicy{})
	require.NotNil(t, f.cache.policies)
}

// IsObjectNull is what every caller uses to decide "is caching configured at
// all". Getting it wrong either dereferences a nil cache or disables a
// perfectly good one.
func TestIsObjectNull_TrueOnlyForAnUnusableCache(t *testing.T) {
	var nilCache *EvmJsonRpcCache
	require.True(t, nilCache.IsObjectNull())
	require.True(t, (&EvmJsonRpcCache{}).IsObjectNull(), "a cache with no logger cannot serve")

	f := newCacheFixture(t, cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	))
	require.False(t, f.cache.IsObjectNull())
	require.False(t, f.cache.WithProjectId("proj").IsObjectNull())
}

// --- compression helpers ---

// The compressed value must survive the round trip through a real connector.
// A write path that compresses and a read path that fails to notice returns
// zstd bytes to the caller as if they were JSON.
func TestCompression_RoundTripsThroughARealConnector(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	), "fastest", 64))

	// Highly repetitive JSON, comfortably over the threshold, so zstd wins.
	raw := `{"number":"0x1234","logs":[` +
		strings.TrimSuffix(strings.Repeat(`{"topic":"0xaaaa","data":"0xbbbb"},`, 40), ",") + `]}`
	require.Greater(t, len(raw), 64)

	req := cacheRequest(blockBody)
	resp := cacheResponse(t, req, raw)
	require.NoError(t, f.cache.Set(ctx, req, resp))

	// The bytes in STORAGE must really be a zstd frame. Reading the connector
	// directly is the only way to tell compression apart from a no-op that
	// happens to round-trip: Get decompresses either way.
	blockRef, _, err := ResolveCacheBlockRef(ctx, req, resp)
	require.NoError(t, err)
	pk, rk, err := generateKeysForJsonRpcRequest(req, blockRef, ctx)
	require.NoError(t, err)

	var stored []byte
	require.Eventually(t, func() bool {
		b, gerr := f.connector("mem").Get(ctx, data.ConnectorMainIndex, pk, rk, req)
		if gerr != nil || len(b) == 0 {
			return false
		}
		stored = b
		return true
	}, 2*time.Second, 5*time.Millisecond, "nothing reached the connector")

	require.True(t, f.cache.isCompressed(stored),
		"the value in storage is not a zstd frame, so compression never ran")
	require.Less(t, len(stored), len(raw), "the stored frame is not smaller than the JSON")

	// And the read path must still hand the caller the original JSON.
	require.JSONEq(t, raw, f.mustGetEventually(ctx, cacheRequest(blockBody)))
}

// Below the threshold nothing is compressed. Framing a 20-byte result makes it
// bigger and costs CPU on the hottest path in the cache.
func TestCompressValueBytes_BelowThresholdIsStoredVerbatim(t *testing.T) {
	f := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	), "fastest", 1024))

	// Highly compressible, so "not compressed" can only mean the threshold
	// stopped it — not that zstd gave up.
	small := []byte(strings.Repeat("a", 200))
	require.Less(t, len(small), 1024)

	got, compressed := f.cache.compressValueBytes(small)
	require.False(t, compressed, "a value under the threshold was compressed anyway")
	require.Equal(t, small, got)

	// Control: the same bytes above the threshold DO compress, so the
	// assertion above is about the threshold and nothing else.
	f.cache.compressionThreshold = 16
	_, compressed = f.cache.compressValueBytes(small)
	require.True(t, compressed)
}

// zstd sometimes makes data BIGGER — random bytes have no redundancy to
// exploit. Storing the larger frame would waste space on every such value.
func TestCompressValueBytes_IncompressiblePayloadIsStoredVerbatim(t *testing.T) {
	f := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	), "fastest", 16))

	noise := make([]byte, 64)
	_, err := rand.Read(noise)
	require.NoError(t, err)

	got, compressed := f.cache.compressValueBytes(noise)
	require.False(t, compressed, "a zstd frame larger than its input was stored anyway")
	require.Equal(t, noise, got)
}

// With compression off the helper must not touch the value at all, whatever
// its size. Otherwise a cache configured for plain storage still writes frames.
func TestCompressValueBytes_DisabledCacheNeverCompresses(t *testing.T) {
	f := newCacheFixture(t, cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	))
	big := []byte(strings.Repeat("a", 4096))
	got, compressed := f.cache.compressValueBytes(big)
	require.False(t, compressed)
	require.Equal(t, big, got)
}

// isCompressed reads the zstd magic number. It decides whether the read path
// decompresses; a false positive turns real JSON into a decode error, and a
// false negative hands zstd bytes to the caller.
func TestIsCompressed_MatchesOnlyTheZstdMagicNumber(t *testing.T) {
	c := &EvmJsonRpcCache{}
	cases := []struct {
		name  string
		input []byte
		want  bool
	}{
		{"zstd frame header", []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00}, true},
		{"exactly the magic number", []byte{0x28, 0xB5, 0x2F, 0xFD}, true},
		{"json object", []byte(`{"a":1}`), false},
		{"three bytes of the magic number", []byte{0x28, 0xB5, 0x2F}, false},
		{"empty", []byte{}, false},
		{"nil", nil, false},
		{"last byte differs", []byte{0x28, 0xB5, 0x2F, 0xFE}, false},
		{"first byte differs", []byte{0x29, 0xB5, 0x2F, 0xFD}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, c.isCompressed(tc.input))
		})
	}
}

// Old entries written before compression was turned on carry no frame. The
// read path must return them untouched rather than failing the lookup.
func TestDecompressValueBytes_PassesUncompressedInputThrough(t *testing.T) {
	f := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	), "fastest", 16))

	plain := []byte(`{"number":"0x1234"}`)
	got, err := f.cache.decompressValueBytes(plain)
	require.NoError(t, err)
	require.Equal(t, plain, got)
}

// A truncated or corrupt frame must surface as an error. Returning the raw
// bytes instead would hand a client a body that is not JSON at all.
func TestDecompressValueBytes_CorruptFrameIsAnError(t *testing.T) {
	f := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	), "fastest", 16))

	good, ok := f.cache.compressValueBytes([]byte(strings.Repeat("ab", 512)))
	require.True(t, ok)

	truncated := append([]byte(nil), good[:len(good)-4]...)
	_, err := f.cache.decompressValueBytes(truncated)
	require.Error(t, err, "a truncated zstd frame decoded without complaint")
}

// The encoder/decoder pools are shared across projects and requests. A pooled
// encoder that is not reset carries the previous value's state, so the second
// compression of the same input must produce the same bytes as the first.
func TestCompression_PooledEncoderIsResetBetweenValues(t *testing.T) {
	f := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	), "fastest", 16))

	payload := []byte(strings.Repeat(`{"topic":"0xdeadbeef"},`, 60))
	first, ok := f.cache.compressValueBytes(payload)
	require.True(t, ok)
	firstCopy := append([]byte(nil), first...)

	// Compress a different value in between, so a leaked encoder state would
	// show up on the third call.
	_, _ = f.cache.compressValueBytes([]byte(strings.Repeat("z", 4096)))

	third, ok := f.cache.compressValueBytes(payload)
	require.True(t, ok)
	require.Equal(t, firstCopy, third, "the pooled encoder leaked state between values")

	round, err := f.cache.decompressValueBytes(third)
	require.NoError(t, err)
	require.Equal(t, payload, round)
}

// --- NormalizeRequest ---

// NormalizeRequest is the seam that lets the request pipeline stop naming
// architectures. It must really run the EVM normalisation: the block tag or
// number in params has to end up on the request as metadata, because the cache
// key, the gRPC routing and the block-availability check all read it from
// there. A handler that returns nil without normalising leaves every request
// with an unknown block number.
func TestNormalizeRequest_AppliesEvmNormalisationToTheRequest(t *testing.T) {
	h := &EvmArchitectureHandler{}
	req := cacheRequest(blockBody)

	require.NoError(t, h.NormalizeRequest(context.Background(), req))

	require.Equal(t, int64(0x1234), req.EvmBlockNumber(),
		"the handler did not run EVM normalisation, so no block number was recorded")
}

// A body that is not JSON-RPC at all must return the parse error, not a nil
// error with an unnormalised request. Swallowing it would send a malformed
// request on to the upstream as if it had been checked.
func TestNormalizeRequest_ParseFailureIsReturned(t *testing.T) {
	h := &EvmArchitectureHandler{}
	err := h.NormalizeRequest(context.Background(), cacheRequest(`not json at all`))
	require.Error(t, err)
}

// A method with no request refs must pass through untouched and without error.
// The unknown-method path is the common one — most methods carry no block
// reference — so it must never fail.
func TestNormalizeRequest_MethodWithoutBlockRefsIsLeftAlone(t *testing.T) {
	h := &EvmArchitectureHandler{}
	req := cacheRequest(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`)

	require.NoError(t, h.NormalizeRequest(context.Background(), req))

	require.Nil(t, req.EvmBlockNumber(),
		"a method with no block refs must not gain a block number")
}

// A pool whose New returns nil is what a zstd encoder that cannot be built
// looks like from the call site. The cache must fall back to storing the value
// uncompressed, not panic on the nil type assertion — a cache write sits on the
// response path of a served request.
func TestCompressValueBytes_AnEmptyEncoderPoolDegradesToUncompressed(t *testing.T) {
	f := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	), "fastest", 16))

	f.cache.encoderPool = &sync.Pool{New: func() interface{} { return nil }}

	payload := []byte(strings.Repeat("a", 200))
	got, compressed := f.cache.compressValueBytes(payload)
	require.False(t, compressed)
	require.Equal(t, payload, got)
}

// The read side must fail loudly instead of panicking when no decoder is
// available. Returning the raw frame would hand a client zstd bytes labelled
// as JSON.
func TestDecompressValueBytes_AnEmptyDecoderPoolIsAnError(t *testing.T) {
	f := newCacheFixture(t, withCompression(cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	), "fastest", 16))

	frame, ok := f.cache.compressValueBytes([]byte(strings.Repeat("ab", 512)))
	require.True(t, ok)

	f.cache.decoderPool = &sync.Pool{New: func() interface{} { return nil }}

	_, err := f.cache.decompressValueBytes(frame)
	require.Error(t, err, "a missing decoder returned the compressed frame as if it were the value")
}
