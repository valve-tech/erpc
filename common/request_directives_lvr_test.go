package common

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const requestTestTimeout = 10 * time.Second

func boolp(b bool) *bool       { return &b }
func stringp(s string) *string { return &s }

// jsonResponse builds a parsed response with the given raw result, so the
// last-valid-response rules can be exercised on real emptiness checks rather
// than on a stub.
func jsonResponse(t *testing.T, result string) *NormalizedResponse {
	t.Helper()
	r, _ := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":`+result+`}`)
	_, err := r.JsonRpcResponse(context.Background())
	require.NoError(t, err)
	return r
}

// The last-valid-response slot is what a request falls back to when every
// remaining attempt fails. Storing the wrong thing there serves stale or
// corrupt data to the client while every log line still says "success".

func TestSetLastValidResponse_RefusesAnIntegrityRejectedResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTestTimeout)
	defer cancel()

	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber"}`))

	corrupt := jsonResponse(t, `"0xbad"`)
	corrupt.MarkIntegrityRejected()

	require.False(t, req.SetLastValidResponse(ctx, corrupt),
		"a response an integrity check rejected must never enter the slot")
	require.Nil(t, req.LastValidResponse())

	// A clean response from another attempt still gets in.
	good := jsonResponse(t, `"0xgood"`)
	require.True(t, req.SetLastValidResponse(ctx, good))
	require.Same(t, good, req.LastValidResponse())

	// And a later rejected response must not displace it.
	require.False(t, req.SetLastValidResponse(ctx, corrupt))
	require.Same(t, good, req.LastValidResponse())
}

// A non-empty result must never be replaced by an empty one: the empty answer
// would be served even though a real one was already in hand.
func TestSetLastValidResponse_NonEmptyBeatsEmpty(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTestTimeout)
	defer cancel()

	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs"}`))

	empty := jsonResponse(t, `null`)
	require.True(t, req.SetLastValidResponse(ctx, empty), "an empty result is better than nothing")
	require.Same(t, empty, req.LastValidResponse())

	full := jsonResponse(t, `["0x1"]`)
	require.True(t, req.SetLastValidResponse(ctx, full), "a real result must replace an empty one")
	require.Same(t, full, req.LastValidResponse())

	// The reverse must be refused.
	require.False(t, req.SetLastValidResponse(ctx, empty))
	require.Same(t, full, req.LastValidResponse())

	// A second real result does replace the first — the latest attempt wins.
	newer := jsonResponse(t, `["0x2"]`)
	require.True(t, req.SetLastValidResponse(ctx, newer))
	require.Same(t, newer, req.LastValidResponse())
}

// Storing a valid response also records which upstream produced it, so the
// X-ERPC-Upstream header and the logs name the right node.
func TestSetLastValidResponse_RecordsTheProducingUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTestTimeout)
	defer cancel()

	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	require.Nil(t, req.LastUpstream())

	up := NewFakeUpstream("up-alpha")
	resp := jsonResponse(t, `"0x1"`)
	resp.SetUpstream(up)

	require.True(t, req.SetLastValidResponse(ctx, resp))
	require.Same(t, up, req.LastUpstream())

	// A nil upstream must not clear the recorded one.
	req.SetLastUpstream(nil)
	require.Same(t, up, req.LastUpstream())
}

func TestSetLastValidResponse_NilInputsAreRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTestTimeout)
	defer cancel()

	var nilReq *NormalizedRequest
	require.False(t, nilReq.SetLastValidResponse(ctx, jsonResponse(t, `"0x1"`)))
	require.Nil(t, nilReq.LastValidResponse())

	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	require.False(t, req.SetLastValidResponse(ctx, nil))
	require.Nil(t, req.LastValidResponse())
}

// A reject path must only drop the response it rejected. With hedged attempts
// in flight, an unconditional clear can throw away a valid response another
// attempt stored a moment earlier.
func TestClearLastValidResponseIf_OnlyDropsTheNamedResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTestTimeout)
	defer cancel()

	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	first := jsonResponse(t, `"0x1"`)
	require.True(t, req.SetLastValidResponse(ctx, first))

	// A hedge stored a newer response before the reject path ran.
	second := jsonResponse(t, `"0x2"`)
	require.True(t, req.SetLastValidResponse(ctx, second))

	req.ClearLastValidResponseIf(first)
	require.Same(t, second, req.LastValidResponse(),
		"clearing a stale response must not drop the one now in the slot")

	req.ClearLastValidResponseIf(second)
	require.Nil(t, req.LastValidResponse())

	// Nil arguments and a nil receiver are no-ops.
	req.ClearLastValidResponseIf(nil)
	var nilReq *NormalizedRequest
	require.NotPanics(t, func() { nilReq.ClearLastValidResponseIf(first) })
}

func TestClearLastValidResponse_DropsWhateverIsThere(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTestTimeout)
	defer cancel()

	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	require.True(t, req.SetLastValidResponse(ctx, jsonResponse(t, `"0x1"`)))

	req.ClearLastValidResponse()
	require.Nil(t, req.LastValidResponse())

	var nilReq *NormalizedRequest
	require.NotPanics(t, nilReq.ClearLastValidResponse)
}

// The integrity bookkeeping answers "did a check save us, and which one".
// Losing the check id turns a specific finding into an unexplained rejection.
func TestNormalizedRequest_IntegrityBookkeeping(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber"}`))

	require.False(t, req.IntegrityCaught())
	require.Equal(t, "", req.IntegrityRejectedCheck())
	require.Equal(t, "", req.IntegrityRejectedFinality())
	require.Equal(t, time.Duration(0), req.IntegrityOverhead())

	req.MarkIntegrityCaught("transactions-root", "finalized")
	require.True(t, req.IntegrityCaught())
	require.Equal(t, "transactions-root", req.IntegrityRejectedCheck())
	require.Equal(t, "finalized", req.IntegrityRejectedFinality())

	// Empty arguments must not erase what is already recorded — a later check
	// that reports no id would otherwise wipe the reason.
	req.MarkIntegrityCaught("", "")
	require.Equal(t, "transactions-root", req.IntegrityRejectedCheck())
	require.Equal(t, "finalized", req.IntegrityRejectedFinality())

	req.AddIntegrityOverhead(30 * time.Millisecond)
	req.AddIntegrityOverhead(20 * time.Millisecond)
	require.Equal(t, 50*time.Millisecond, req.IntegrityOverhead(), "overhead accumulates across attempts")

	// A non-positive duration must not move the counter backwards.
	req.AddIntegrityOverhead(-time.Second)
	require.Equal(t, 50*time.Millisecond, req.IntegrityOverhead())

	var nilReq *NormalizedRequest
	require.False(t, nilReq.IntegrityCaught())
	require.Equal(t, "", nilReq.IntegrityRejectedCheck())
	require.Equal(t, "", nilReq.IntegrityRejectedFinality())
	require.Equal(t, time.Duration(0), nilReq.IntegrityOverhead())
	require.NotPanics(t, func() { nilReq.MarkIntegrityCaught("x", "y"); nilReq.AddIntegrityOverhead(time.Second) })
}

// --- directives ---

func TestApplyDirectiveDefaults_FillsEveryConfiguredField(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{
		RetryEmpty:                 boolp(true),
		RetryPending:               boolp(true),
		SkipCacheRead:              "redis-*",
		UseUpstream:                stringp("alchemy-*"),
		SkipInterpolation:          boolp(true),
		SkipConsensus:              boolp(true),
		EnforceHighestBlock:        boolp(true),
		EnforceGetLogsBlockRange:   boolp(true),
		EnforceNonNullTaggedBlocks: boolp(true),
	})

	d := req.Directives()
	require.NotNil(t, d)
	require.True(t, d.RetryEmpty)
	require.True(t, d.RetryPending)
	require.Equal(t, "redis-*", d.SkipCacheRead)
	require.Equal(t, "alchemy-*", d.UseUpstream)
	require.True(t, d.SkipInterpolation)
	require.True(t, d.SkipConsensus)
	require.True(t, d.EnforceHighestBlock)
	require.True(t, d.EnforceGetLogsBlockRange)
	require.True(t, d.EnforceNonNullTaggedBlocks)
}

// skipCacheRead is declared as `interface{}` so YAML `true` arrives as a bool.
// Dropping the non-string branch would leave it empty and quietly re-enable
// the cache the operator turned off.
func TestApplyDirectiveDefaults_SkipCacheReadAcceptsANonString(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{SkipCacheRead: true})

	require.Equal(t, "true", req.Directives().SkipCacheRead)
	require.True(t, req.ShouldSkipCacheRead(""))
}

// Network.Forward calls ApplyDirectiveDefaults defensively. It must not undo
// the headers the HTTP layer already parsed.
func TestApplyDirectiveDefaults_DoesNotOverwriteExistingDirectives(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	req.SetDirectives(&RequestDirectives{UseUpstream: "from-header"})

	req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{UseUpstream: stringp("from-config")})
	require.Equal(t, "from-header", req.Directives().UseUpstream)

	// A nil config is a no-op and must not allocate an empty directive set,
	// which would then block a later real default.
	fresh := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	fresh.ApplyDirectiveDefaults(nil)
	require.Nil(t, fresh.Directives())
}

func TestEnrichFromHttp_HeadersSetEveryDirective(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	h := http.Header{}
	h.Set("X-ERPC-Retry-Empty", "true")
	h.Set("X-ERPC-Retry-Pending", "TRUE")
	h.Set("X-ERPC-Skip-Cache-Read", " redis-* ")
	h.Set("X-ERPC-Use-Upstream", "alchemy-*")
	h.Set("X-ERPC-Skip-Interpolation", "true")
	h.Set("X-ERPC-Skip-Consensus", "true")
	h.Set("X-ERPC-Enforce-Highest-Block", "true")
	h.Set("X-ERPC-Enforce-GetLogs-Range", "true")
	h.Set("X-ERPC-Enforce-Non-Null-Tagged-Blocks", "true")
	h.Set("X-ERPC-Integrity", " strict ")

	req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)

	d := req.Directives()
	require.NotNil(t, d)
	require.True(t, d.RetryEmpty)
	require.True(t, d.RetryPending, "the value is matched case-insensitively")
	require.Equal(t, "redis-*", d.SkipCacheRead, "the value is trimmed")
	require.Equal(t, "alchemy-*", d.UseUpstream)
	require.True(t, d.SkipInterpolation)
	require.True(t, d.SkipConsensus)
	require.True(t, d.EnforceHighestBlock)
	require.True(t, d.EnforceGetLogsBlockRange)
	require.True(t, d.EnforceNonNullTaggedBlocks)
	require.Equal(t, "strict", d.IntegritySelector)
}

// Anything other than "true" means false. A header of "1" or "yes" must not
// silently enable a behaviour the caller did not ask for in the documented way.
func TestEnrichFromHttp_OnlyTheWordTrueEnablesABooleanDirective(t *testing.T) {
	for _, v := range []string{"1", "yes", "on", "false", "TRUEISH"} {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		h := http.Header{}
		h.Set("X-ERPC-Retry-Empty", v)
		req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
		require.False(t, req.Directives().RetryEmpty, "header value %q must not enable the directive", v)
	}
}

// Query parameters are parsed after headers so a URL can still override, which
// is what makes a browser-side debug link work.
func TestEnrichFromHttp_QueryParamsOverrideHeaders(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	h := http.Header{}
	h.Set("X-ERPC-Use-Upstream", "from-header")
	h.Set("X-ERPC-Retry-Empty", "true")

	q := url.Values{}
	q.Set("use-upstream", "from-query")
	q.Set("retry-empty", "false")
	q.Set("skip-cache-read", "true")
	q.Set("integrity", "strict")
	q.Set("retry-pending", "true")
	q.Set("skip-interpolation", "true")
	q.Set("skip-consensus", "true")
	q.Set("enforce-highest-block", "true")
	q.Set("enforce-getlogs-range", "true")
	q.Set("enforce-non-null-tagged-blocks", "true")

	req.EnrichFromHttp(h, q, UserAgentTrackingModeSimplified)

	d := req.Directives()
	require.Equal(t, "from-query", d.UseUpstream)
	require.False(t, d.RetryEmpty, "the query parameter must win over the header")
	require.Equal(t, "true", d.SkipCacheRead)
	require.Equal(t, "strict", d.IntegritySelector)
	require.True(t, d.RetryPending)
	require.True(t, d.SkipInterpolation)
	require.True(t, d.SkipConsensus)
	require.True(t, d.EnforceHighestBlock)
	require.True(t, d.EnforceGetLogsBlockRange)
	require.True(t, d.EnforceNonNullTaggedBlocks)
}

// With no directive present the request must keep whatever the config set, and
// must NOT allocate a directive set of its own. The order matters: the HTTP
// layer calls EnrichFromHttp before Network.Forward calls
// ApplyDirectiveDefaults, and ApplyDirectiveDefaults refuses to run once
// directives exist — so an allocation here would silently discard every
// configured default for requests that carry no header.
func TestEnrichFromHttp_NoDirectivesLeavesTheDefaultsAlone(t *testing.T) {
	t.Run("existing defaults survive", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{UseUpstream: stringp("from-config")})

		h := http.Header{}
		h.Set("User-Agent", "curl/8.0")
		req.EnrichFromHttp(h, url.Values{}, UserAgentTrackingModeSimplified)

		require.Equal(t, "from-config", req.Directives().UseUpstream)
	})

	t.Run("no directive set is allocated", func(t *testing.T) {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

		h := http.Header{}
		h.Set("User-Agent", "curl/8.0")
		req.EnrichFromHttp(h, url.Values{}, UserAgentTrackingModeSimplified)

		require.Nil(t, req.Directives(),
			"a request with no directive header must stay open to the config defaults")

		// The defaults must therefore still apply afterwards.
		req.ApplyDirectiveDefaults(&DirectiveDefaultsConfig{UseUpstream: stringp("from-config")})
		require.Equal(t, "from-config", req.Directives().UseUpstream)
	})
}

// A shared directive set (the network's defaults) must be cloned before an HTTP
// header edits it, or one caller's header would change every other request.
func TestEnrichFromHttp_ClonesSharedDirectivesBeforeEditing(t *testing.T) {
	shared := &RequestDirectives{UseUpstream: "shared-value", RetryPending: true}

	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	req.SetDirectives(shared)

	h := http.Header{}
	h.Set("X-ERPC-Use-Upstream", "per-request")
	req.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)

	require.Equal(t, "per-request", req.Directives().UseUpstream)
	require.Equal(t, "shared-value", shared.UseUpstream,
		"the shared directive set must not be mutated by one request's header")
	require.True(t, req.Directives().RetryPending, "the untouched fields must survive the clone")
}

// allowClientDirectives lets an operator refuse client-supplied directives. A
// directive that slipped through would let any caller pin traffic to one
// upstream or bypass the cache.
func TestEnrichFromHttp_HonoursTheAllowClientDirectiveMatcher(t *testing.T) {
	build := func(matcher MatcherFunc) *NormalizedRequest {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.SetAllowClientDirectiveMatcher(matcher)
		h := http.Header{}
		h.Set("X-ERPC-Use-Upstream", "alchemy")
		h.Set("X-ERPC-Skip-Cache-Read", "true")
		q := url.Values{}
		q.Set("retry-empty", "true")
		req.EnrichFromHttp(h, q, UserAgentTrackingModeSimplified)
		return req
	}

	t.Run("deny all", func(t *testing.T) {
		d := build(DenyAllClientDirectives).Directives()
		require.NotNil(t, d)
		require.Equal(t, "", d.UseUpstream)
		require.Equal(t, "", d.SkipCacheRead)
		require.False(t, d.RetryEmpty)
	})

	t.Run("allow one directive only", func(t *testing.T) {
		only, err := NewWildcardMatcher("use-upstream")
		require.NoError(t, err)

		d := build(only).Directives()
		require.Equal(t, "alchemy", d.UseUpstream, "the permitted directive must still apply")
		require.Equal(t, "", d.SkipCacheRead, "a directive outside the allow list must be dropped")
		require.False(t, d.RetryEmpty)
	})

	t.Run("no matcher allows everything", func(t *testing.T) {
		d := build(nil).Directives()
		require.Equal(t, "alchemy", d.UseUpstream)
		require.Equal(t, "true", d.SkipCacheRead)
		require.True(t, d.RetryEmpty)
	})
}

// skipCacheRead can name specific connectors. Reading it as a plain boolean
// would skip every cache, including ones the caller wanted to keep.
func TestShouldSkipCacheRead_TrueFalseAndPatterns(t *testing.T) {
	withDirective := func(v string) *NormalizedRequest {
		req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
		req.SetDirectives(&RequestDirectives{SkipCacheRead: v})
		return req
	}

	t.Run("unset or false skips nothing", func(t *testing.T) {
		require.False(t, withDirective("").ShouldSkipCacheRead("redis-main"))
		require.False(t, withDirective("false").ShouldSkipCacheRead("redis-main"))
		require.False(t, withDirective("FALSE").ShouldSkipCacheRead("redis-main"))
	})

	t.Run("true skips every connector", func(t *testing.T) {
		require.True(t, withDirective("true").ShouldSkipCacheRead("redis-main"))
		require.True(t, withDirective("TRUE").ShouldSkipCacheRead("memory"))
		require.True(t, withDirective("true").ShouldSkipCacheRead(""),
			"with no connector named, 'true' still means skip")
	})

	t.Run("a pattern skips only matching connectors", func(t *testing.T) {
		req := withDirective("redis-*")
		require.True(t, req.ShouldSkipCacheRead("redis-main"))
		require.False(t, req.ShouldSkipCacheRead("memory"))
		// Before any connector is chosen a pattern cannot decide, so nothing is
		// skipped — skipping everything here would disable the cache entirely.
		require.False(t, req.ShouldSkipCacheRead(""))
	})

	t.Run("nil request and nil directives skip nothing", func(t *testing.T) {
		var nilReq *NormalizedRequest
		require.False(t, nilReq.ShouldSkipCacheRead("redis-main"))
		require.False(t, NewNormalizedRequest(nil).ShouldSkipCacheRead("redis-main"))
	})
}

// Internal requests (state poller, chain-id probe) bypass retry, hedge and
// breaker policies. Misreading a client request as internal would silently
// disable failover for it.
func TestNormalizedRequest_IsInternalOnlyWhenTheDirectiveSaysSo(t *testing.T) {
	var nilReq *NormalizedRequest
	require.False(t, nilReq.IsInternal())

	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`))
	require.False(t, req.IsInternal(), "a request with no directives is not internal")

	req.SetDirectives(&RequestDirectives{})
	require.False(t, req.IsInternal())

	req.SetDirectives(&RequestDirectives{IsInternal: true})
	require.True(t, req.IsInternal())
}

// The trusted user header attributes metrics when a gateway did the auth. Auth
// must always win, or a client-supplied header could impersonate another user.
func TestSetUserFromTrustedHeader_NeverOverridesAuth(t *testing.T) {
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	require.Nil(t, req.User())

	req.SetUserFromTrustedHeader("  ")
	require.Nil(t, req.User(), "a blank header must not create a user")

	req.SetUserFromTrustedHeader("  alice  ")
	require.NotNil(t, req.User())
	require.Equal(t, "alice", req.User().Id, "the value is trimmed")

	// A second header value must not replace the identity already in place.
	req.SetUserFromTrustedHeader("mallory")
	require.Equal(t, "alice", req.User().Id)

	// And when auth resolved a user first, the header is ignored entirely.
	authed := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	authed.SetUser(&User{Id: "from-auth"})
	authed.SetUserFromTrustedHeader("mallory")
	require.Equal(t, "from-auth", authed.User().Id)

	var nilReq *NormalizedRequest
	require.NotPanics(t, func() { nilReq.SetUserFromTrustedHeader("x") })
	require.Nil(t, nilReq.User())
}

// The user agent becomes a metrics label. Raw mode keeps the whole string;
// simplified mode must reduce it, or one library's version bump doubles the
// series count.
func TestEnrichFromHttp_UserAgentModeDecidesTheLabel(t *testing.T) {
	agent := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/120.0.0.0 Safari/537.36"

	h := http.Header{}
	h.Set("User-Agent", agent)

	raw := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	raw.EnrichFromHttp(h, nil, UserAgentTrackingModeRaw)

	simplified := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	simplified.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)

	rawLabel := raw.AgentName()
	simpleLabel := simplified.AgentName()

	require.Equal(t, agent, rawLabel, "raw mode must keep the header verbatim")
	require.NotEmpty(t, simpleLabel, "simplified mode must still produce a label")
	require.NotEqual(t, rawLabel, simpleLabel, "simplified mode must actually reduce the string")
	require.Less(t, len(simpleLabel), len(rawLabel))
	require.False(t, strings.Contains(simpleLabel, "537.36"),
		"a version number must not reach the metrics label")
}
