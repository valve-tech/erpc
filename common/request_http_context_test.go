package common

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// A NormalizedRequest carries two things nothing else can reconstruct once the
// HTTP layer is gone: who the caller is, and what client they used. Every test
// below is about one of those surviving the hops eRPC makes internally.

func TestCopyHttpContextFrom_CarriesTheCallerIdentityIntoADerivedRequest(t *testing.T) {
	// eRPC splits one client call into several internal requests — a getLogs
	// range split, a composite lookup. Each derived request is metered and
	// logged on its own, so without this copy the split traffic is attributed to
	// no user and no client, and a per-user quota stops seeing most of the load.
	source := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs"}`))
	h := http.Header{}
	h.Set("User-Agent", "viem/2.0.0")
	source.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
	source.SetUser(&User{Id: "tenant-7"})

	derived := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"eth_getLogs"}`))
	require.Equal(t, "unknown", derived.AgentName())
	require.Equal(t, "n/a", derived.UserId())

	derived.CopyHttpContextFrom(source)

	require.Equal(t, "viem", derived.AgentName())
	require.Equal(t, "tenant-7", derived.UserId())
	require.Equal(t, "tenant-7", derived.User().Id)
}

func TestCopyHttpContextFrom_CopiesOnlyWhatTheSourceActuallyHas(t *testing.T) {
	// A source built internally (a state poll, a chainId probe) carries neither
	// agent nor user. Copying from it must leave the target as it was rather
	// than storing a nil user or an empty agent label, which would show up in
	// metrics as a distinct client.
	source := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))

	derived := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":2,"method":"eth_chainId"}`))
	h := http.Header{}
	h.Set("User-Agent", "curl/8.0")
	derived.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
	derived.SetUser(&User{Id: "keep-me"})

	derived.CopyHttpContextFrom(source)

	require.Equal(t, "curl", derived.AgentName(), "an empty source must not blank the target")
	require.Equal(t, "keep-me", derived.UserId())
}

func TestCopyHttpContextFrom_IsANoOpWhenEitherSideIsMissing(t *testing.T) {
	// Both sides are reached through interfaces that may hand back nil. A panic
	// here would take down the request that triggered the split, not just the
	// split itself.
	var nilReq *NormalizedRequest
	require.NotPanics(t, func() { nilReq.CopyHttpContextFrom(nil) })

	real := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	require.NotPanics(t, func() { real.CopyHttpContextFrom(nil) })
	require.NotPanics(t, func() { nilReq.CopyHttpContextFrom(real) })
}

func TestUserId_ReportsNotAvailableRatherThanAnEmptyLabel(t *testing.T) {
	// UserId is a metric label. An empty string and "n/a" are two different
	// series, so an anonymous request must always land on the same one.
	var nilReq *NormalizedRequest
	require.Equal(t, "n/a", nilReq.UserId())

	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	require.Equal(t, "n/a", r.UserId(), "no user at all")

	r.SetUser(&User{})
	require.Equal(t, "n/a", r.UserId(), "a user with no id is still not identifiable")

	r.SetUser(&User{Id: "tenant-7"})
	require.Equal(t, "tenant-7", r.UserId())
}

func TestAgentName_FallsBackToUnknownUntilEnrichFromHttpRuns(t *testing.T) {
	// Internal requests never see an HTTP header. They must report one stable
	// label, because a nil read would panic inside the metrics path.
	var nilReq *NormalizedRequest
	require.Equal(t, "unknown", nilReq.AgentName())
	require.Equal(t, "unknown",
		NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`)).AgentName())
}

func TestGetUserAgent_PrefersTheQueryParameterOverTheHeader(t *testing.T) {
	// A browser cannot set User-Agent, so the query parameter is the only way a
	// debug link can label itself. It has to win, and both sources have to be
	// optional — the internal callers pass nil for each.
	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	h := http.Header{}
	h.Set("User-Agent", "from-header")
	q := url.Values{}
	q.Set("user-agent", "from-query")

	require.Equal(t, "from-query", r.getUserAgent(h, q))
	require.Equal(t, "from-header", r.getUserAgent(h, url.Values{}), "an empty param does not win")
	require.Equal(t, "from-header", r.getUserAgent(h, nil))
	require.Equal(t, "from-query", r.getUserAgent(nil, q))
	require.Equal(t, "", r.getUserAgent(nil, nil))
}

func TestSimplifyAgentName_CollapsesAUserAgentIntoALowCardinalityLabel(t *testing.T) {
	// The agent name becomes a Prometheus label. Every distinct value is a new
	// time series, and a raw User-Agent carries a version and a build id — a
	// handful of clients would otherwise mint thousands of series.
	//
	// Every row below is a user agent whose FIRST WORD is not the label, so the
	// named branch is the only thing that can produce the answer. Rows where the
	// generic first-word fallthrough already yields the label (`curl/8.4.0`,
	// `viem/2.7.9`, `ethers/…`, `insomnia/…`, `HTTPie/…`, `Java/…`, `Ruby/…`,
	// `alchemy-sdk/…`) are deliberately absent: asserting them proves nothing
	// about the branch, because deleting the branch does not change the answer.
	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	for _, tc := range []struct{ userAgent, want string }{
		{"libcurl-agent/1.0", "curl"},
		{"GNU Wget/1.21.3", "wget"},
		{"PostmanRuntime/7.36.0", "postman"},
		{"python-requests/2.31.0", "python"},
		{"node-fetch/3.3.2", "nodejs"},
		{"SomeJavaScriptClient/1.0", "nodejs"},
		{"Go-http-client/2.0", "go"},
		{"reqwest/0.11 go/1.22", "go"},
		{"Apache-HttpClient/4.5.14 (Java/17.0.9)", "java"},
		{"rust-reqwest/0.11", "rust"},
		{"Faraday v2.7.11 ruby/3.2", "ruby"},
		{"GuzzleHttp/7.8 php/8.2", "php"},
		{"Mozilla/5.0 (X11) Chrome/120.0.0.0", "chrome"},
		{"Mozilla/5.0 (X11) Firefox/121.0", "firefox"},
		{"Mozilla/5.0 (Macintosh) Version/17.0 Safari/605.1", "safari"},
		{"web3.py/6.13.0", "web3"},
		{"infura-provider/1.0", "infura-sdk"},
	} {
		t.Run(tc.userAgent, func(t *testing.T) {
			require.Equal(t, tc.want, r.simplifyAgentName(tc.userAgent))
		})
	}
}

func TestSimplifyAgentName_ClipsAnUnrecognisedAgentInsteadOfPassingItThrough(t *testing.T) {
	// The unknown case is the one that actually protects the metrics store: an
	// unrecognised client is the DEFAULT, not the exception. Only its first word
	// survives, cut at the version separator and capped at 20 characters.
	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	require.Equal(t, "myclient", r.simplifyAgentName("MyClient/1.2.3 (linux)"),
		"the version is dropped, and the label is lowercased")
	require.Equal(t, "myclient", r.simplifyAgentName("myclient (build 9)"),
		"a parenthesis cuts the name too")
	require.Equal(t, "myclient", r.simplifyAgentName("  myclient  "),
		"surrounding whitespace is not part of the name")
	require.Equal(t, "aaaaaaaaaabbbbbbbbbb",
		r.simplifyAgentName("aaaaaaaaaabbbbbbbbbbcccccccccc"),
		"a long name is capped at 20 characters")
	require.Equal(t, "other", r.simplifyAgentName(""),
		"no user agent at all is one label, not an empty one")
	require.Equal(t, "other", r.simplifyAgentName("/1.2.3"),
		"a name that is nothing but a version has no usable word left")
}

// KNOWN DEFECT, pinned so a fix shows up as a test change.
//
// simplifyAgentName is an ordered chain of substring tests, and two of its
// later branches are shadowed by an earlier one that also matches:
//
//   - "quicknode" contains "node", so the `node` test at common/request.go:1243
//     fires first and the `quicknode` branch at common/request.go:1288 is
//     unreachable for every real QuickNode user agent.
//   - Every Chromium-based browser sends "Chrome" in its user agent, so the
//     `chrome` test at common/request.go:1261 claims Edge before the `edge`
//     branch at common/request.go:1270 is reached. Released Edge also spells
//     itself "Edg/", which "edge" does not match at all.
//
// The operator sees no error. They see QuickNode SDK traffic filed under
// "nodejs" and Edge traffic filed under "chrome", so a per-client breakdown
// silently reports the wrong client.
func TestSimplifyAgentName_EarlierBranchesShadowQuicknodeAndEdge(t *testing.T) {
	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))

	require.Equal(t, "nodejs", r.simplifyAgentName("quicknode-sdk/2.0"),
		"defect: `node` matches first, so no user agent can ever reach the quicknode branch")
	require.Equal(t, "nodejs", r.simplifyAgentName("@quicknode/sdk 2.0"))

	const edgeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0"
	require.Equal(t, "chrome", r.simplifyAgentName(edgeUA),
		"defect: released Edge is filed as chrome")
}

func TestEnrichFromHttp_StoresTheRawUserAgentOnlyInRawMode(t *testing.T) {
	// Raw mode is the opt-in for operators who accept the cardinality. The two
	// modes must actually differ, or the setting is decorative.
	h := http.Header{}
	h.Set("User-Agent", "MyClient/1.2.3 (linux)")

	raw := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	raw.EnrichFromHttp(h, nil, UserAgentTrackingModeRaw)
	require.Equal(t, "MyClient/1.2.3 (linux)", raw.AgentName())

	simplified := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	simplified.EnrichFromHttp(h, nil, UserAgentTrackingModeSimplified)
	require.Equal(t, "myclient", simplified.AgentName())
}

// ---------------------------------------------------------------------------
// The upstream list a request carries
// ---------------------------------------------------------------------------

func TestUpstreams_HandsOutASnapshotTheCallerCannotUseToReorderTheRequest(t *testing.T) {
	// Callers read this list to rank or filter upstreams. If it were the live
	// slice, a policy that sorts its copy would silently reorder the request's
	// own selection order mid-flight.
	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	up1, up2 := newMockUpstream("u1"), newMockUpstream("u2")
	r.SetUpstreams([]Upstream{up1, up2})

	require.Equal(t, 2, r.UpstreamsCount())

	snap := r.Upstreams()
	require.Len(t, snap, 2)
	require.Equal(t, "u1", snap[0].Id())

	snap[0], snap[1] = snap[1], snap[0]
	require.Equal(t, "u1", r.Upstreams()[0].Id(), "the request keeps its own order")

	// Selection still starts at the head, which is the property the snapshot
	// exists to protect.
	first, err := r.NextUpstream()
	require.NoError(t, err)
	require.Equal(t, "u1", first.Id())
}

func TestUpstreams_ReportsEmptinessWithoutAllocating(t *testing.T) {
	// Callers branch on `len(...) == 0`, and a nil request reaches here from the
	// composite paths. Both must answer rather than panic.
	var nilReq *NormalizedRequest
	require.Equal(t, 0, nilReq.UpstreamsCount())
	require.Nil(t, nilReq.Upstreams())

	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	require.Equal(t, 0, r.UpstreamsCount())
	require.Nil(t, r.Upstreams())

	r.SetUpstreams([]Upstream{})
	require.Equal(t, 0, r.UpstreamsCount())
	require.Nil(t, r.Upstreams())
}

func TestNextUpstream_HonoursTheUseUpstreamDirective(t *testing.T) {
	// `use-upstream` pins a request to one node — the debug path, and the way an
	// operator verifies a single upstream in production. Selecting a
	// non-matching upstream would answer from a node the caller explicitly
	// excluded, and the answer would look correct.
	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	r.SetUpstreams([]Upstream{newMockUpstream("alpha"), newMockUpstream("beta")})
	r.SetDirectives(&RequestDirectives{UseUpstream: "beta"})

	selected, err := r.NextUpstream()
	require.NoError(t, err)
	require.Equal(t, "beta", selected.Id())

	// With every match consumed there is nothing left, and the request must say
	// so rather than fall back to the excluded upstream.
	_, err = r.NextUpstream()
	require.Error(t, err)
	require.True(t, HasErrorCode(err, ErrCodeNoUpstreamsLeftToSelect))
}

func TestNextUpstream_SaysNoUpstreamsWereSetAtAll(t *testing.T) {
	// An empty list and an exhausted list are different operator problems: one
	// is a config or routing gap, the other is every node failing. The messages
	// must not be interchangeable.
	var nilReq *NormalizedRequest
	_, err := nilReq.NextUpstream()
	require.Error(t, err)

	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	r.SetUpstreams(nil)
	_, err = r.NextUpstream()
	require.Error(t, err)
	require.Contains(t, err.Error(), "no upstreams are set for this request")

	r.SetUpstreams([]Upstream{newMockUpstream("alpha")})
	_, err = r.NextUpstream()
	require.NoError(t, err)
	_, err = r.NextUpstream()
	require.Error(t, err)
	require.Contains(t, err.Error(), "no more non-consumed or working upstreams left")
}

func TestMarkUpstreamCompleted_FreesTheReservationForACancelledHedge(t *testing.T) {
	// A cancelled hedge never reached the upstream. Leaving it consumed would
	// take a healthy node out of the pool for the rest of the request, and the
	// request would exhaust its upstreams while a working one sat idle.
	ctx := context.Background()
	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	up := newMockUpstream("alpha")
	r.SetUpstreams([]Upstream{up})

	selected, err := r.NextUpstream()
	require.NoError(t, err)
	r.MarkUpstreamCompleted(ctx, selected, nil, NewErrEndpointRequestCanceled(context.Canceled))

	_, exists := r.ErrorsByUpstream.Load(up)
	require.False(t, exists, "a cancelled hedge is not a failure to report")

	again, err := r.NextUpstream()
	require.NoError(t, err, "the upstream must be selectable again")
	require.Equal(t, "alpha", again.Id())
}

// ---------------------------------------------------------------------------
// Request-level validity and identity
// ---------------------------------------------------------------------------

func TestValidate_RejectsEveryRequestThatCannotNameAMethod(t *testing.T) {
	// Validate runs before routing. A request that reaches the network layer
	// with no method matches no cache policy and no failsafe matcher, and fails
	// deep inside the upstream client with an error that names nothing useful.
	var nilReq *NormalizedRequest
	require.Error(t, nilReq.Validate())

	// A request with neither a body nor a parsed JSON-RPC request has to say so.
	// Falling through to the method lookup would report an unresolvable method
	// instead, which points the operator at the caller's JSON rather than at the
	// empty POST they actually sent.
	err := NewNormalizedRequest(nil).Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "request body is nil")

	badJson := NewNormalizedRequest([]byte(`{not json`))
	err = badJson.Validate()
	require.Error(t, err)
	require.True(t, HasErrorCode(err, ErrCodeInvalidRequest))

	// An absent `method` key and a present-but-empty one are different failures.
	// The first cannot be read at all; the second parses and names nothing.
	absent := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1}`))
	err = absent.Validate()
	require.Error(t, err)
	require.True(t, HasErrorCode(err, ErrCodeJsonRpcRequestUnmarshal))

	blank := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":""}`))
	err = blank.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "method is required")

	// A request built in code, with a JsonRpcRequest that names no method,
	// reaches the same guard by a different route.
	built := NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{})
	err = built.Validate()
	require.Error(t, err)
	require.Contains(t, err.Error(), "method is required")

	require.NoError(t, NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`)).Validate())
}

func TestCacheHash_RefusesToKeyARequestItCannotParse(t *testing.T) {
	// The cache hash is the cache key. Returning a hash for an unparseable
	// request would let two different bodies share one cache entry, and the
	// second caller would receive the first one's answer.
	bad := NewNormalizedRequest([]byte(`{not json`))
	_, err := bad.CacheHash()
	require.Error(t, err)

	good := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":["0x1"]}`))
	h1, err := good.CacheHash()
	require.NoError(t, err)
	require.NotEmpty(t, h1)

	// The same call keys the same way whatever request id the client chose —
	// otherwise the cache never hits.
	sameCall := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":99,"method":"eth_call","params":["0x1"]}`))
	h2, err := sameCall.CacheHash()
	require.NoError(t, err)
	require.Equal(t, h1, h2)

	// A different parameter is a different answer and must key differently.
	otherCall := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":["0x2"]}`))
	h3, err := otherCall.CacheHash()
	require.NoError(t, err)
	require.NotEqual(t, h1, h3)
}

func TestMarshalJSON_FallsBackToTheMethodWhenThereIsNoBody(t *testing.T) {
	// A request built in code has no body. Logs and error payloads marshal it,
	// so an empty document there loses the only clue about what failed.
	withBody := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	out, err := withBody.MarshalJSON()
	require.NoError(t, err)
	require.JSONEq(t, `{"jsonrpc":"2.0","id":1,"method":"eth_call"}`, string(out))

	fromJsonRpc := NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{Method: "eth_getLogs"})
	out, err = fromJsonRpc.MarshalJSON()
	require.NoError(t, err)
	require.JSONEq(t, `{"method":"eth_getLogs"}`, string(out))

	empty := NewNormalizedRequest(nil)
	out, err = empty.MarshalJSON()
	require.NoError(t, err)
	require.Nil(t, out, "nothing known about the request means nothing to serialise")
}

func TestCompositeType_ReportsNoneUntilOneIsSet(t *testing.T) {
	// Composite handling branches on this. Reporting a composite type for a
	// plain request would send it down the splitting path and re-issue it.
	var nilReq *NormalizedRequest
	require.Equal(t, "", nilReq.CompositeType())
	require.False(t, nilReq.IsCompositeRequest())

	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs"}`))
	require.Equal(t, CompositeTypeNone, r.CompositeType())
	require.False(t, r.IsCompositeRequest())

	r.SetCompositeType("")
	require.Equal(t, CompositeTypeNone, r.CompositeType(), "an empty type is not a type")

	r.SetCompositeType(CompositeTypeLogsSplitOnError)
	require.Equal(t, CompositeTypeLogsSplitOnError, r.CompositeType())
	require.True(t, r.IsCompositeRequest())
}

func TestSetCacheDal_IsReadBackAndNilSafe(t *testing.T) {
	// The cache layer is installed per request by the network. A request that
	// cannot report it would consult no cache at all and silently forward every
	// call to an upstream.
	var nilReq *NormalizedRequest
	require.Nil(t, nilReq.CacheDal())

	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	require.Nil(t, r.CacheDal())

	dal := &noopCacheDal{}
	r.SetCacheDal(dal)
	require.Same(t, dal, r.CacheDal())
}

// noopCacheDal is the smallest thing that satisfies CacheDAL, so the accessor
// can be tested without a real cache.
type noopCacheDal struct{}

func (n *noopCacheDal) Get(ctx context.Context, req *NormalizedRequest) (*NormalizedResponse, error) {
	return nil, nil
}
func (n *noopCacheDal) Set(ctx context.Context, req *NormalizedRequest, res *NormalizedResponse) error {
	return nil
}
func (n *noopCacheDal) IsObjectNull() bool { return n == nil }
