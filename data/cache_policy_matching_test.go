package data

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCachePolicy(t *testing.T, cfg *common.CachePolicyConfig) *CachePolicy {
	t.Helper()
	p, err := NewCachePolicy(cfg, nil)
	require.NoError(t, err)
	return p
}

// TestCachePolicy_MatchesForSet_NetworkAndMethod proves the write side honours
// the operator's network and method globs. A policy that matched too widely
// would store one chain's data under another chain's policy and serve wrong
// answers; one that matched too narrowly would silently cache nothing.
func TestCachePolicy_MatchesForSet_NetworkAndMethod(t *testing.T) {
	cases := []struct {
		name      string
		network   string
		method    string
		reqNet    string
		reqMethod string
		want      bool
	}{
		{"exact network and method", "evm:1", "eth_getLogs", "evm:1", "eth_getLogs", true},
		{"wrong network", "evm:1", "eth_getLogs", "evm:137", "eth_getLogs", false},
		{"wrong method", "evm:1", "eth_getLogs", "evm:1", "eth_call", false},
		{"network wildcard", "*", "eth_getLogs", "evm:137", "eth_getLogs", true},
		{"method wildcard", "evm:1", "*", "evm:1", "anything_here", true},
		{"method prefix glob matches", "evm:1", "eth_get*", "evm:1", "eth_getBalance", true},
		{"method prefix glob rejects another family", "evm:1", "eth_get*", "evm:1", "eth_call", false},
		{"network family glob", "evm:*", "eth_call", "evm:42161", "eth_call", true},
		{"network family glob rejects another architecture", "evm:*", "eth_call", "svm:mainnet", "eth_call", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestCachePolicy(t, &common.CachePolicyConfig{
				Network: tc.network,
				Method:  tc.method,
			})
			got, err := p.MatchesForSet(tc.reqNet, tc.reqMethod, nil, common.DataFinalityStateFinalized, false)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestCachePolicy_MatchesForSet_FinalityIsExact locks in the write-side rule:
// a policy stores a response only when the response's finality equals the
// policy's own. Any loosening here would let unfinalized data land in a
// long-TTL finalized bucket and get served after a reorg.
func TestCachePolicy_MatchesForSet_FinalityIsExact(t *testing.T) {
	all := []common.DataFinalityState{
		common.DataFinalityStateFinalized,
		common.DataFinalityStateUnfinalized,
		common.DataFinalityStateRealtime,
		common.DataFinalityStateUnknown,
	}

	for _, policyFinality := range all {
		for _, responseFinality := range all {
			p := newTestCachePolicy(t, &common.CachePolicyConfig{
				Network:  "*",
				Method:   "*",
				Finality: policyFinality,
			})
			got, err := p.MatchesForSet("evm:1", "eth_call", nil, responseFinality, false)
			require.NoError(t, err)
			assert.Equal(t, policyFinality == responseFinality, got,
				"policy finality %s must store %s only when they are equal", policyFinality, responseFinality)
		}
	}
}

// TestCachePolicy_MatchesForSet_EmptyBehaviour covers the three-way switch an
// operator uses to keep empty results out of a cache, or to build a dedicated
// short-TTL bucket that holds only empties.
func TestCachePolicy_MatchesForSet_EmptyBehaviour(t *testing.T) {
	cases := []struct {
		behavior     common.CacheEmptyBehavior
		isEmptyish   bool
		wantStored   bool
		whyItMatters string
	}{
		{common.CacheEmptyBehaviorIgnore, true, false, "ignore must drop an empty response"},
		{common.CacheEmptyBehaviorIgnore, false, true, "ignore must still store a real response"},
		{common.CacheEmptyBehaviorAllow, true, true, "allow must store an empty response"},
		{common.CacheEmptyBehaviorAllow, false, true, "allow must store a real response"},
		{common.CacheEmptyBehaviorOnly, true, true, "only must store an empty response"},
		{common.CacheEmptyBehaviorOnly, false, false, "only must drop a real response"},
	}

	for _, tc := range cases {
		t.Run(tc.whyItMatters, func(t *testing.T) {
			p := newTestCachePolicy(t, &common.CachePolicyConfig{
				Network: "*",
				Method:  "*",
				Empty:   tc.behavior,
			})
			got, err := p.MatchesForSet("evm:1", "eth_getLogs", nil, common.DataFinalityStateFinalized, tc.isEmptyish)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStored, got, tc.whyItMatters)
		})
	}

	t.Run("EmptyState reports the configured behaviour", func(t *testing.T) {
		for _, b := range []common.CacheEmptyBehavior{
			common.CacheEmptyBehaviorIgnore,
			common.CacheEmptyBehaviorAllow,
			common.CacheEmptyBehaviorOnly,
		} {
			p := newTestCachePolicy(t, &common.CachePolicyConfig{Network: "*", Method: "*", Empty: b})
			assert.Equal(t, b, p.EmptyState())
		}
	})
}

// TestCachePolicy_MatchesForGet_FinalityWidening pins the read-side asymmetry.
// A request known to target finalized data may also read from an unfinalized
// bucket, because the response was written before the block finalized. Losing
// that widening turns every such read into a cache miss and doubles upstream
// load; widening it further would serve realtime data as finalized.
func TestCachePolicy_MatchesForGet_FinalityWidening(t *testing.T) {
	cases := []struct {
		requestFinality common.DataFinalityState
		policyFinality  common.DataFinalityState
		want            bool
	}{
		{common.DataFinalityStateFinalized, common.DataFinalityStateFinalized, true},
		{common.DataFinalityStateFinalized, common.DataFinalityStateUnfinalized, true},
		{common.DataFinalityStateFinalized, common.DataFinalityStateRealtime, false},
		{common.DataFinalityStateFinalized, common.DataFinalityStateUnknown, false},

		{common.DataFinalityStateUnfinalized, common.DataFinalityStateUnfinalized, true},
		{common.DataFinalityStateUnfinalized, common.DataFinalityStateFinalized, false},
		{common.DataFinalityStateUnfinalized, common.DataFinalityStateRealtime, false},
		{common.DataFinalityStateUnfinalized, common.DataFinalityStateUnknown, false},

		{common.DataFinalityStateRealtime, common.DataFinalityStateRealtime, true},
		{common.DataFinalityStateRealtime, common.DataFinalityStateFinalized, false},
		{common.DataFinalityStateRealtime, common.DataFinalityStateUnfinalized, false},
		{common.DataFinalityStateRealtime, common.DataFinalityStateUnknown, false},

		// An unknown request finality has to try every bucket, because the
		// finality of the stored response is also unknown.
		{common.DataFinalityStateUnknown, common.DataFinalityStateFinalized, true},
		{common.DataFinalityStateUnknown, common.DataFinalityStateUnfinalized, true},
		{common.DataFinalityStateUnknown, common.DataFinalityStateRealtime, true},
		{common.DataFinalityStateUnknown, common.DataFinalityStateUnknown, true},
	}

	for _, tc := range cases {
		p := newTestCachePolicy(t, &common.CachePolicyConfig{
			Network:  "*",
			Method:   "*",
			Finality: tc.policyFinality,
		})
		got, err := p.MatchesForGet("evm:1", "eth_getLogs", nil, tc.requestFinality)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got,
			"request finality %s reading a %s policy", tc.requestFinality, tc.policyFinality)
	}
}

// TestCachePolicy_MatchesForGet_NetworkAndMethod proves the read side applies
// the same globs as the write side. A read that ignored the method glob would
// return another method's cached body.
func TestCachePolicy_MatchesForGet_NetworkAndMethod(t *testing.T) {
	p := newTestCachePolicy(t, &common.CachePolicyConfig{
		Network: "evm:1",
		Method:  "eth_get*",
	})

	got, err := p.MatchesForGet("evm:1", "eth_getLogs", nil, common.DataFinalityStateUnknown)
	require.NoError(t, err)
	assert.True(t, got)

	got, err = p.MatchesForGet("evm:137", "eth_getLogs", nil, common.DataFinalityStateUnknown)
	require.NoError(t, err)
	assert.False(t, got, "another network must not read this policy's entries")

	got, err = p.MatchesForGet("evm:1", "eth_call", nil, common.DataFinalityStateUnknown)
	require.NoError(t, err)
	assert.False(t, got, "another method must not read this policy's entries")
}

// TestCachePolicy_AppliesTo proves the get/set gate. An operator uses a
// set-only policy to warm a slow cold store and a get-only policy to read from
// a store nothing writes to; mixing them up would write to the read-only tier.
func TestCachePolicy_AppliesTo(t *testing.T) {
	cases := []struct {
		appliesTo common.CachePolicyAppliesTo
		wantSet   bool
		wantGet   bool
	}{
		{"", true, true},
		{common.CachePolicyAppliesToBoth, true, true},
		{common.CachePolicyAppliesToSet, true, false},
		{common.CachePolicyAppliesToGet, false, true},
	}

	for _, tc := range cases {
		t.Run(string(tc.appliesTo)+" appliesTo", func(t *testing.T) {
			p := newTestCachePolicy(t, &common.CachePolicyConfig{
				Network:   "*",
				Method:    "*",
				AppliesTo: tc.appliesTo,
			})

			gotSet, err := p.MatchesForSet("evm:1", "eth_call", nil, common.DataFinalityStateFinalized, false)
			require.NoError(t, err)
			assert.Equal(t, tc.wantSet, gotSet, "set gate")

			gotGet, err := p.MatchesForGet("evm:1", "eth_call", nil, common.DataFinalityStateFinalized)
			require.NoError(t, err)
			assert.Equal(t, tc.wantGet, gotGet, "get gate")
		})
	}
}

// TestCachePolicy_MatchesForSetAndGet_Params proves the params filter reaches
// both sides. This is how an operator caches eth_getBlockByNumber(latest)
// differently from a pinned block, so a params filter that only ran on set
// would read the wrong entry back.
func TestCachePolicy_MatchesForSetAndGet_Params(t *testing.T) {
	p := newTestCachePolicy(t, &common.CachePolicyConfig{
		Network: "*",
		Method:  "eth_getBlockByNumber",
		Params:  []interface{}{"0x*", true},
	})

	matching := []interface{}{"0x1b4", true}
	nonMatching := []interface{}{"latest", true}
	wrongSecond := []interface{}{"0x1b4", false}

	got, err := p.MatchesForSet("evm:1", "eth_getBlockByNumber", matching, common.DataFinalityStateFinalized, false)
	require.NoError(t, err)
	assert.True(t, got)

	got, err = p.MatchesForSet("evm:1", "eth_getBlockByNumber", nonMatching, common.DataFinalityStateFinalized, false)
	require.NoError(t, err)
	assert.False(t, got, "a params mismatch must block the write")

	got, err = p.MatchesForSet("evm:1", "eth_getBlockByNumber", wrongSecond, common.DataFinalityStateFinalized, false)
	require.NoError(t, err)
	assert.False(t, got, "every configured param position must match, not just the first")

	got, err = p.MatchesForGet("evm:1", "eth_getBlockByNumber", matching, common.DataFinalityStateFinalized)
	require.NoError(t, err)
	assert.True(t, got)

	got, err = p.MatchesForGet("evm:1", "eth_getBlockByNumber", nonMatching, common.DataFinalityStateFinalized)
	require.NoError(t, err)
	assert.False(t, got, "a params mismatch must block the read")
}

// TestCachePolicy_MatchesForSetAndGet_ReportsABadPattern proves a malformed
// operator glob surfaces as an error rather than a silent non-match. A silent
// non-match would disable the cache with no signal at all.
func TestCachePolicy_MatchesForSetAndGet_ReportsABadPattern(t *testing.T) {
	badNetwork := newTestCachePolicy(t, &common.CachePolicyConfig{Network: "evm:1 | (", Method: "*"})
	_, err := badNetwork.MatchesForSet("evm:1", "eth_call", nil, common.DataFinalityStateFinalized, false)
	require.Error(t, err, "an unbalanced network pattern must be reported")
	_, err = badNetwork.MatchesForGet("evm:1", "eth_call", nil, common.DataFinalityStateFinalized)
	require.Error(t, err)

	badMethod := newTestCachePolicy(t, &common.CachePolicyConfig{Network: "*", Method: "eth_call | ("})
	_, err = badMethod.MatchesForSet("evm:1", "eth_call", nil, common.DataFinalityStateFinalized, false)
	require.Error(t, err, "an unbalanced method pattern must be reported")
	_, err = badMethod.MatchesForGet("evm:1", "eth_call", nil, common.DataFinalityStateFinalized)
	require.Error(t, err)

	badParam := newTestCachePolicy(t, &common.CachePolicyConfig{
		Network: "*", Method: "*", Params: []interface{}{"0x1 | ("},
	})
	_, err = badParam.MatchesForSet("evm:1", "eth_call", []interface{}{"0x1"}, common.DataFinalityStateFinalized, false)
	require.Error(t, err, "an unbalanced param pattern must be reported")
	_, err = badParam.MatchesForGet("evm:1", "eth_call", []interface{}{"0x1"}, common.DataFinalityStateFinalized)
	require.Error(t, err)
}

// TestCachePolicy_MatchesSizeLimits keeps oversized bodies out of a cache and
// undersized ones out of an expensive tier. Both bounds are inclusive; an
// off-by-one here quietly changes which tier every borderline response lands in.
func TestCachePolicy_MatchesSizeLimits(t *testing.T) {
	sizeStr := func(s string) *string { return &s }

	t.Run("no limits accepts everything", func(t *testing.T) {
		p := newTestCachePolicy(t, &common.CachePolicyConfig{Network: "*", Method: "*"})
		assert.True(t, p.MatchesSizeLimits(0))
		assert.True(t, p.MatchesSizeLimits(1<<30))
	})

	t.Run("min only", func(t *testing.T) {
		p := newTestCachePolicy(t, &common.CachePolicyConfig{
			Network: "*", Method: "*", MinItemSize: sizeStr("1kb"),
		})
		assert.False(t, p.MatchesSizeLimits(1023), "just under the floor must be rejected")
		assert.True(t, p.MatchesSizeLimits(1024), "the floor itself must be accepted")
		assert.True(t, p.MatchesSizeLimits(1<<20))
	})

	t.Run("max only", func(t *testing.T) {
		p := newTestCachePolicy(t, &common.CachePolicyConfig{
			Network: "*", Method: "*", MaxItemSize: sizeStr("1kb"),
		})
		assert.True(t, p.MatchesSizeLimits(1024), "the ceiling itself must be accepted")
		assert.False(t, p.MatchesSizeLimits(1025), "just over the ceiling must be rejected")
		assert.True(t, p.MatchesSizeLimits(0))
	})

	t.Run("both bounds", func(t *testing.T) {
		p := newTestCachePolicy(t, &common.CachePolicyConfig{
			Network: "*", Method: "*", MinItemSize: sizeStr("1kb"), MaxItemSize: sizeStr("2kb"),
		})
		assert.False(t, p.MatchesSizeLimits(1023))
		assert.True(t, p.MatchesSizeLimits(1024))
		assert.True(t, p.MatchesSizeLimits(1536))
		assert.True(t, p.MatchesSizeLimits(2048))
		assert.False(t, p.MatchesSizeLimits(2049))
	})
}

// TestNewCachePolicy_RejectsAnUnparseableSize fails startup on a typo rather
// than booting with the limit dropped, which would let unbounded bodies into
// the cache.
func TestNewCachePolicy_RejectsAnUnparseableSize(t *testing.T) {
	bad := "one megabyte"

	p, err := NewCachePolicy(&common.CachePolicyConfig{Network: "*", Method: "*", MinItemSize: &bad}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "minItemSize")
	assert.Nil(t, p)

	p, err = NewCachePolicy(&common.CachePolicyConfig{Network: "*", Method: "*", MaxItemSize: &bad}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxItemSize")
	assert.Nil(t, p)
}

// TestCachePolicy_ResolveTTL is the single source of truth shared by the
// write-side storage expiry and the read-side age guard. If the two ever
// disagree, the cache serves an entry the guard has already declared stale, or
// expires one the guard would still accept.
func TestCachePolicy_ResolveTTL(t *testing.T) {
	const coldStart = 5 * time.Second

	cases := []struct {
		name      string
		ttl       *common.BlockTimeAdaptiveDuration
		blockTime time.Duration
		want      time.Duration
	}{
		{"no ttl configured", nil, 12 * time.Second, 0},
		{
			name: "fixed ttl ignores the block time",
			ttl:  &common.BlockTimeAdaptiveDuration{Fallback: common.Duration(2 * time.Second)},
			// A fixed TTL must not drift when the chain speeds up.
			blockTime: 12 * time.Second,
			want:      2 * time.Second,
		},
		{
			name:      "block time multiplier tracks the chain",
			ttl:       &common.BlockTimeAdaptiveDuration{BlockTimeMultiplier: 1.5},
			blockTime: 12 * time.Second,
			want:      18 * time.Second,
		},
		{
			name:      "an unknown block time falls back when a fallback is set",
			ttl:       &common.BlockTimeAdaptiveDuration{BlockTimeMultiplier: 1.5, Fallback: common.Duration(3 * time.Second)},
			blockTime: 0,
			want:      3 * time.Second,
		},
		{
			name: "an unknown block time with no fallback uses the cold start default",
			// Without this the TTL would be zero and every entry would expire
			// immediately during the first poll interval after a restart.
			ttl:       &common.BlockTimeAdaptiveDuration{BlockTimeMultiplier: 1.5},
			blockTime: 0,
			want:      coldStart,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestCachePolicy(t, &common.CachePolicyConfig{Network: "*", Method: "*", TTL: tc.ttl})
			assert.Equal(t, tc.want, p.ResolveTTL(tc.blockTime, coldStart))
		})
	}
}

// TestCachePolicy_GetTTL returns the fixed component only. Telemetry labels use
// it, and a block-time-resolved value would change per sample and explode the
// metric cardinality.
func TestCachePolicy_GetTTL(t *testing.T) {
	t.Run("fixed ttl", func(t *testing.T) {
		p := newTestCachePolicy(t, &common.CachePolicyConfig{
			Network: "*", Method: "*",
			TTL: &common.BlockTimeAdaptiveDuration{Fallback: common.Duration(2 * time.Second)},
		})
		require.NotNil(t, p.GetTTL())
		assert.Equal(t, 2*time.Second, *p.GetTTL())
	})

	t.Run("block time ttl reports its fallback, not a resolved value", func(t *testing.T) {
		p := newTestCachePolicy(t, &common.CachePolicyConfig{
			Network: "*", Method: "*",
			TTL: &common.BlockTimeAdaptiveDuration{BlockTimeMultiplier: 10, Fallback: common.Duration(3 * time.Second)},
		})
		require.NotNil(t, p.GetTTL())
		assert.Equal(t, 3*time.Second, *p.GetTTL())
	})

	t.Run("no ttl still returns a non-nil pointer", func(t *testing.T) {
		p := newTestCachePolicy(t, &common.CachePolicyConfig{Network: "*", Method: "*"})
		require.NotNil(t, p.GetTTL(), "telemetry dereferences this; a nil would panic the metrics path")
		assert.Equal(t, time.Duration(0), *p.GetTTL())
	})
}

// TestCachePolicy_String labels the policy in logs and errors. An operator
// reading "which policy dropped my write" needs the network, method, and
// finality to appear.
func TestCachePolicy_String(t *testing.T) {
	p := newTestCachePolicy(t, &common.CachePolicyConfig{
		Network:  "evm:1",
		Method:   "eth_getLogs",
		Finality: common.DataFinalityStateUnfinalized,
	})

	s := p.String()
	assert.Contains(t, s, "network=evm:1")
	assert.Contains(t, s, "method=eth_getLogs")
	assert.Contains(t, s, "finality=unfinalized")
}

// TestCachePolicy_MarshalJSON keeps the admin/debug view honest: the rendered
// policy must be the configured one, so an operator can confirm what erpc
// actually loaded.
func TestCachePolicy_MarshalJSON(t *testing.T) {
	p := newTestCachePolicy(t, &common.CachePolicyConfig{
		Connector: "memory-1",
		Network:   "evm:1",
		Method:    "eth_getLogs",
	})

	body, err := p.MarshalJSON()
	require.NoError(t, err)
	assert.Contains(t, string(body), `"memory-1"`)
	assert.Contains(t, string(body), `"evm:1"`)
	assert.Contains(t, string(body), `"eth_getLogs"`)
}

// TestCachePolicy_GetConnector returns the connector the policy was built with.
// The cache calls Get/Set on this object, so handing back the wrong one — or
// nil — would read from the wrong tier.
func TestCachePolicy_GetConnector(t *testing.T) {
	logger := zerolog.Nop()
	conn, err := NewMemoryConnector(context.Background(), &logger, "memory-1", &common.MemoryConnectorConfig{
		MaxItems: 100, MaxTotalSize: "1mb",
	})
	require.NoError(t, err)

	p, err := NewCachePolicy(&common.CachePolicyConfig{Network: "*", Method: "*"}, conn)
	require.NoError(t, err)
	assert.Same(t, conn, p.GetConnector())
	assert.Equal(t, "memory-1", p.GetConnector().Id(),
		"the policy must carry the connector the operator named, not a default one")
}
