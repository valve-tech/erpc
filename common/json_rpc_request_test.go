package common

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestJsonRpcRequest_UnmarshalJSON_KeepsTheClientIdVerbatim is the reason
// idRaw exists. A client using a nanosecond timestamp as its id must get the
// same digits back; the typed float64 view loses the last three of them and the
// client can then never match a response to its request.
func TestJsonRpcRequest_UnmarshalJSON_KeepsTheClientIdVerbatim(t *testing.T) {
	t.Parallel()

	const bigID = "1755123456789012345"

	var r JsonRpcRequest
	require.NoError(t, r.UnmarshalJSON([]byte(
		`{"jsonrpc":"2.0","id":`+bigID+`,"method":"eth_blockNumber","params":[]}`)))

	require.Equal(t, bigID, string(r.IDRawBytes()),
		"the wire bytes must survive so the response can echo them")
	require.Equal(t, "eth_blockNumber", r.Method)
}

// TestJsonRpcRequest_IDRawBytes_ReturnsACopy — the caller writes these bytes
// straight into a response. Handing out the live slice lets a response writer
// corrupt the request it was built from.
func TestJsonRpcRequest_IDRawBytes_ReturnsACopy(t *testing.T) {
	t.Parallel()

	var r JsonRpcRequest
	require.NoError(t, r.UnmarshalJSON([]byte(`{"id":77,"method":"eth_call"}`)))

	got := r.IDRawBytes()
	require.Equal(t, "77", string(got))
	got[0] = '9'
	require.Equal(t, "77", string(r.IDRawBytes()), "the stored id must be untouched")
}

// TestJsonRpcRequest_UnmarshalJSON_IdShapes walks the id forms erpc must accept
// and the fallbacks it applies. A request with no usable id still needs one, or
// the multiplexer cannot key it.
func TestJsonRpcRequest_UnmarshalJSON_IdShapes(t *testing.T) {
	t.Parallel()

	t.Run("integer id becomes int64", func(t *testing.T) {
		var r JsonRpcRequest
		require.NoError(t, r.UnmarshalJSON([]byte(`{"id":5,"method":"m"}`)))
		require.Equal(t, int64(5), r.ID)
		require.Equal(t, "5", string(r.IDRawBytes()))
	})

	t.Run("string id is kept as a string", func(t *testing.T) {
		var r JsonRpcRequest
		require.NoError(t, r.UnmarshalJSON([]byte(`{"id":"abc","method":"m"}`)))
		require.Equal(t, "abc", r.ID)
		require.Equal(t, `"abc"`, string(r.IDRawBytes()))
	})

	t.Run("a null id gets a generated one and no raw bytes", func(t *testing.T) {
		var r JsonRpcRequest
		require.NoError(t, r.UnmarshalJSON([]byte(`{"id":null,"method":"m"}`)))
		require.NotNil(t, r.ID)
		require.Nil(t, r.IDRawBytes(),
			"echoing back a literal null id would break the client's matching")
	})

	t.Run("a missing id gets a generated one", func(t *testing.T) {
		var r JsonRpcRequest
		require.NoError(t, r.UnmarshalJSON([]byte(`{"method":"m"}`)))
		require.NotNil(t, r.ID)
		require.Nil(t, r.IDRawBytes())
	})

	t.Run("an empty string id is replaced", func(t *testing.T) {
		var r JsonRpcRequest
		require.NoError(t, r.UnmarshalJSON([]byte(`{"id":"","method":"m"}`)))
		require.NotEqual(t, "", r.ID, "an empty id cannot key a multiplexed response")
	})
}

// TestJsonRpcRequest_UnmarshalJSON_DefaultsTheVersion — clients omit jsonrpc
// far more often than the spec suggests, and upstreams reject a request with a
// missing version.
func TestJsonRpcRequest_UnmarshalJSON_DefaultsTheVersion(t *testing.T) {
	t.Parallel()

	var r JsonRpcRequest
	require.NoError(t, r.UnmarshalJSON([]byte(`{"id":1,"method":"eth_call","params":[]}`)))
	require.Equal(t, "2.0", r.JSONRPC)
}

// TestJsonRpcRequest_UnmarshalJSON_RejectsAMalformedBody keeps a broken body
// from arriving downstream as a request with an empty method.
func TestJsonRpcRequest_UnmarshalJSON_RejectsAMalformedBody(t *testing.T) {
	t.Parallel()

	var r JsonRpcRequest
	require.Error(t, r.UnmarshalJSON([]byte(`{"id":1,"method":`)))
}

// TestJsonRpcRequest_SetID_DropsTheStaleWireBytes matters on the sub-request
// path: erpc rewrites the id of a derived request, and a leftover idRaw would
// make the response echo the PARENT's id back to the client.
func TestJsonRpcRequest_SetID_DropsTheStaleWireBytes(t *testing.T) {
	t.Parallel()

	var r JsonRpcRequest
	require.NoError(t, r.UnmarshalJSON([]byte(`{"id":999,"method":"m"}`)))
	require.Equal(t, "999", string(r.IDRawBytes()))

	require.NoError(t, r.SetID("child-1"))
	require.Equal(t, "child-1", r.ID)
	require.Nil(t, r.IDRawBytes(), "the typed id must win over captured wire bytes")
}

// TestJsonRpcRequest_Clone_CarriesTheRawIdForward keeps precision through the
// hedge and retry paths, which all work on clones.
func TestJsonRpcRequest_Clone_CarriesTheRawIdForward(t *testing.T) {
	t.Parallel()

	const bigID = "1755123456789012345"
	var r JsonRpcRequest
	require.NoError(t, r.UnmarshalJSON([]byte(`{"id":` + bigID + `,"method":"m","params":[1]}`)))

	c := r.Clone()
	require.Equal(t, bigID, string(c.IDRawBytes()))

	// It must be a copy, not the same backing array.
	raw := c.IDRawBytes()
	raw[0] = '0'
	require.Equal(t, bigID, string(r.IDRawBytes()))
}

// TestJsonRpcRequest_Clone_OfAProgrammaticRequestHasNoRawId — requests erpc
// builds itself (state poller, chainId probe) never had wire bytes.
func TestJsonRpcRequest_Clone_OfAProgrammaticRequestHasNoRawId(t *testing.T) {
	t.Parallel()

	r := NewJsonRpcRequest("eth_chainId", nil)
	require.Equal(t, "2.0", r.JSONRPC)
	require.Equal(t, "eth_chainId", r.Method)
	require.Nil(t, r.Params)

	c := r.Clone()
	require.Nil(t, c.IDRawBytes())
	require.Nil(t, c.Params, "a nil params list must stay nil, not become an empty array")
}

// TestJsonRpcRequest_CacheHash_SeparatesRequestsThatMustNotShareACacheEntry is
// the correctness core of the cache. A collision here serves one block's data
// for another block, which is the worst failure erpc can produce.
func TestJsonRpcRequest_CacheHash_SeparatesRequestsThatMustNotShareACacheEntry(t *testing.T) {
	t.Parallel()

	hash := func(method string, params []interface{}) string {
		t.Helper()
		h, err := NewJsonRpcRequest(method, params).CacheHash()
		require.NoError(t, err)
		require.NotEmpty(t, h)
		return h
	}

	base := hash("eth_getBlockByNumber", []interface{}{"0x1", false})

	require.NotEqual(t, base, hash("eth_getBlockByNumber", []interface{}{"0x2", false}),
		"a different block must not share a cache entry")
	require.NotEqual(t, base, hash("eth_getBlockByNumber", []interface{}{"0x1", true}),
		"the full-transactions flag changes the body")
	require.NotEqual(t, base, hash("eth_getBlockByHash", []interface{}{"0x1", false}),
		"a different method must not share a cache entry")
	require.Equal(t, base, hash("eth_getBlockByNumber", []interface{}{"0x1", false}),
		"the same request must hash the same")

	require.True(t, strings.HasPrefix(base, "eth_getBlockByNumber:"),
		"the method must be readable in the key so an operator can inspect the cache")
}

// TestJsonRpcRequest_CacheHash_FoldsHexCase pins deliberate normalization:
// clients disagree on address casing, and treating 0xAB and 0xab as different
// requests would halve the cache hit rate.
func TestJsonRpcRequest_CacheHash_FoldsHexCase(t *testing.T) {
	t.Parallel()

	upper, err := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xAABB", "latest"}).CacheHash()
	require.NoError(t, err)
	lower, err := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xaabb", "latest"}).CacheHash()
	require.NoError(t, err)
	require.Equal(t, upper, lower)
}

// TestJsonRpcRequest_CacheHash_IsStableAcrossObjectMemberOrder — a filter
// object built by a client library can serialize its members in any order.
func TestJsonRpcRequest_CacheHash_IsStableAcrossObjectMemberOrder(t *testing.T) {
	t.Parallel()

	// Six members, so an unsorted walk would land on the same order by chance
	// roughly once in seven hundred runs rather than once in two.
	filter := map[string]interface{}{
		"fromBlock": "0x1",
		"toBlock":   "0x2",
		"address":   "0xaa",
		"topics":    []interface{}{"0xbb"},
		"blockHash": "0xcc",
		"limit":     "0xdd",
	}
	same := map[string]interface{}{
		"limit":     "0xdd",
		"blockHash": "0xcc",
		"topics":    []interface{}{"0xbb"},
		"address":   "0xaa",
		"toBlock":   "0x2",
		"fromBlock": "0x1",
	}

	a, err := NewJsonRpcRequest("eth_getLogs", []interface{}{filter}).CacheHash()
	require.NoError(t, err)

	b, err := NewJsonRpcRequest("eth_getLogs", []interface{}{same}).CacheHash()
	require.NoError(t, err)
	require.Equal(t, a, b)

	// A different range must still differ, otherwise the sort is erasing data.
	changed := map[string]interface{}{}
	for k, v := range filter {
		changed[k] = v
	}
	changed["toBlock"] = "0x3"
	c, err := NewJsonRpcRequest("eth_getLogs", []interface{}{changed}).CacheHash()
	require.NoError(t, err)
	require.NotEqual(t, a, c)

	// Moving a value between two keys must change the key. Without the key
	// bytes in the hash, {"a":"0x1","b":"0x2"} and {"a":"0x2","b":"0x1"} would
	// collide and the cache would serve one filter's logs for the other.
	swapA, err := NewJsonRpcRequest("eth_getLogs", []interface{}{
		map[string]interface{}{"fromBlock": "0x1", "toBlock": "0x2"},
	}).CacheHash()
	require.NoError(t, err)
	swapB, err := NewJsonRpcRequest("eth_getLogs", []interface{}{
		map[string]interface{}{"fromBlock": "0x2", "toBlock": "0x1"},
	}).CacheHash()
	require.NoError(t, err)
	require.NotEqual(t, swapA, swapB)
}

// TestJsonRpcRequest_CacheHash_DistinguishesNullFromAbsent guards the nil
// branch of the hasher.
func TestJsonRpcRequest_CacheHash_DistinguishesNullFromAbsent(t *testing.T) {
	t.Parallel()

	withNull, err := NewJsonRpcRequest("m", []interface{}{nil}).CacheHash()
	require.NoError(t, err)
	empty, err := NewJsonRpcRequest("m", []interface{}{}).CacheHash()
	require.NoError(t, err)
	require.NotEqual(t, withNull, empty)
}

// TestJsonRpcRequest_CacheHash_RefusesAnUnhashableParam is the safe failure:
// erpc must decline to build a key it cannot compute rather than collapse the
// unknown value into a key it shares with a different request.
func TestJsonRpcRequest_CacheHash_RefusesAnUnhashableParam(t *testing.T) {
	t.Parallel()

	// int64 has no case in the hasher — this is the shape an internally built
	// request carries when a caller passes a Go integer literal as int64.
	h, err := NewJsonRpcRequest("m", []interface{}{int64(7)}).CacheHash()
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported type")
	require.Empty(t, h)
}

// TestJsonRpcRequest_InvalidateCacheHash_ForcesARecompute covers the hook the
// pre-forward block-tag interpolation relies on. Without it, a request whose
// "latest" was rewritten to a concrete block number would still be cached under
// the key for "latest" — pinning stale data at that key forever.
func TestJsonRpcRequest_InvalidateCacheHash_ForcesARecompute(t *testing.T) {
	t.Parallel()

	r := NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"latest", false})
	before, err := r.CacheHash()
	require.NoError(t, err)

	r.Params[0] = "0x1234"

	stale, err := r.CacheHash()
	require.NoError(t, err)
	require.Equal(t, before, stale, "the memoized value is deliberately sticky")

	r.InvalidateCacheHash()
	after, err := r.CacheHash()
	require.NoError(t, err)
	require.NotEqual(t, before, after, "after invalidation the new params must reach the key")

	// And the recomputed value must equal a freshly built request's key,
	// not merely differ from the old one.
	want, err := NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"0x1234", false}).CacheHash()
	require.NoError(t, err)
	require.Equal(t, want, after)
}

// TestJsonRpcRequest_InvalidateCacheHash_OfNilIsSafe — the hook runs on
// whatever the pre-forward stage holds, which may be nothing.
func TestJsonRpcRequest_InvalidateCacheHash_OfNilIsSafe(t *testing.T) {
	t.Parallel()

	var r *JsonRpcRequest
	require.NotPanics(t, func() { r.InvalidateCacheHash() })
	h, err := r.CacheHash()
	require.NoError(t, err)
	require.Empty(t, h)
}

// TestJsonRpcRequest_PeekByPath_ReadsNestedParams is how the EVM hooks find the
// block tag and the getLogs filter without unmarshalling the whole request.
func TestJsonRpcRequest_PeekByPath_ReadsNestedParams(t *testing.T) {
	t.Parallel()

	r := NewJsonRpcRequest("eth_getLogs", []interface{}{
		map[string]interface{}{
			"fromBlock": "0x1",
			"topics":    []interface{}{"0xaa", "0xbb"},
		},
	})

	v, err := r.PeekByPath(0, "fromBlock")
	require.NoError(t, err)
	require.Equal(t, "0x1", v)

	v, err = r.PeekByPath(0, "topics", 1)
	require.NoError(t, err)
	require.Equal(t, "0xbb", v)
}

// TestJsonRpcRequest_PeekByPath_ReportsWhyItFailed. Each failure mode must be
// distinguishable, because a hook that cannot tell "absent" from "wrong shape"
// either skips a safety check or rejects a valid request.
func TestJsonRpcRequest_PeekByPath_ReportsWhyItFailed(t *testing.T) {
	t.Parallel()

	r := NewJsonRpcRequest("eth_getLogs", []interface{}{
		map[string]interface{}{"fromBlock": "0x1"},
	})

	cases := []struct {
		name     string
		path     []interface{}
		contains string
	}{
		{"index past the end", []interface{}{5}, "out of bounds"},
		{"negative index", []interface{}{-1}, "out of bounds"},
		{"map key on an array", []interface{}{"fromBlock"}, "expected map"},
		{"array index on a map", []interface{}{0, 0}, "expected array"},
		{"missing key", []interface{}{0, "toBlock"}, "not found"},
		{"unsupported path element", []interface{}{0, 1.5}, "unsupported path element"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.PeekByPath(tc.path...)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.contains)
		})
	}

	t.Run("empty params", func(t *testing.T) {
		_, err := NewJsonRpcRequest("eth_blockNumber", nil).PeekByPath(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty params")
	})
}

// TestTranslateToJsonRpcException_MapsEachInternalErrorToItsClientCode is the
// contract with every client SDK: the numeric code is what a caller switches
// on. Asserting only "an error came back" would pass on every branch, so each
// case asserts the exact normalized code.
func TestTranslateToJsonRpcException_MapsEachInternalErrorToItsClientCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   error
		want JsonRpcErrorNumber
	}{
		{
			name: "auth rate limit",
			in:   NewErrAuthRateLimitRuleExceeded("p", "secret", "b", "r", "u", "1.2.3.4"),
			want: JsonRpcErrorCapacityExceeded,
		},
		{
			name: "project rate limit",
			in:   NewErrProjectRateLimitRuleExceeded("p", "b", "r"),
			want: JsonRpcErrorCapacityExceeded,
		},
		{
			name: "network rate limit",
			in:   NewErrNetworkRateLimitRuleExceeded("p", "evm:1", "b", "r"),
			want: JsonRpcErrorCapacityExceeded,
		},
		{
			name: "upstream rate limit",
			in:   NewErrUpstreamRateLimitRuleExceeded("up1", "b", "r"),
			want: JsonRpcErrorCapacityExceeded,
		},
		{
			name: "unauthorized",
			in:   NewErrAuthUnauthorized("secret", "bad token"),
			want: JsonRpcErrorUnauthorized,
		},
		{
			name: "method ignored by the upstream",
			in:   NewErrUpstreamMethodIgnored("eth_getLogs", "up1"),
			want: JsonRpcErrorUnsupportedException,
		},
		{
			name: "unparseable request body",
			in:   NewErrJsonRpcRequestUnmarshal(errors.New("bad json"), []byte(`{`)),
			want: JsonRpcErrorParseException,
		},
		{
			name: "invalid request",
			in:   NewErrInvalidRequest(errors.New("method is required")),
			want: JsonRpcErrorInvalidArgument,
		},
		{
			name: "invalid url path",
			in:   NewErrInvalidUrlPath("no project", "/x/y"),
			want: JsonRpcErrorInvalidArgument,
		},
		{
			name: "getLogs range too large",
			in:   NewErrGetLogsExceededMaxAllowedRange(5000, 1000),
			want: JsonRpcErrorEvmLargeRange,
		},
		{
			name: "anything else falls through to a server-side exception",
			in:   errors.New("something nobody classified"),
			want: JsonRpcErrorServerSideException,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := TranslateToJsonRpcException(tc.in)
			var jre *ErrJsonRpcExceptionInternal
			require.True(t, errors.As(out, &jre), "translation must yield a json-rpc exception")
			require.Equal(t, tc.want, jre.NormalizedCode(),
				"the normalized code is what the client library switches on")
		})
	}
}

// TestTranslateToJsonRpcException_PassesThroughAnUpstreamJsonRpcError proves an
// error that already carries the upstream's own code is not re-wrapped. Losing
// the upstream's -32000 would turn "header not found" into a generic 500 and
// hide the real cause from the caller.
func TestTranslateToJsonRpcException_PassesThroughAnUpstreamJsonRpcError(t *testing.T) {
	t.Parallel()

	orig := NewErrJsonRpcExceptionInternal(
		-32000, JsonRpcErrorEvmLargeRange, "query returned more than 10000 results", nil, nil)

	out := TranslateToJsonRpcException(orig)
	require.Same(t, error(orig), out, "the existing json-rpc error must be returned untouched")
}

// TestTranslateToJsonRpcException_CarriesTheDeepestMessageToTheClient — an
// operator reading a client's error report needs the innermost cause, not the
// outer "internal server error" wrapper.
func TestTranslateToJsonRpcException_CarriesTheDeepestMessageToTheClient(t *testing.T) {
	t.Parallel()

	inner := NewErrEndpointServerSideException(errors.New("node is syncing"), nil, 503)
	out := TranslateToJsonRpcException(inner)

	var jre *ErrJsonRpcExceptionInternal
	require.True(t, errors.As(out, &jre))
	require.Contains(t, jre.Message, "node is syncing")

	plain := TranslateToJsonRpcException(errors.New("raw failure"))
	require.True(t, errors.As(plain, &jre))
	require.Contains(t, jre.Message, "raw failure")
}

// TestTranslateToJsonRpcException_MethodIgnoredMessageIsSelfRepeating records
// what a client actually reads today. The constructor stores the method and the
// upstream id in Details, but the translation drops Details and prefixes the
// deepest message with a phrase the message already contains — so the caller
// gets a stutter and never learns which method or upstream was involved. The
// assertions pin today's string so a fix has something to break.
func TestTranslateToJsonRpcException_MethodIgnoredMessageIsSelfRepeating(t *testing.T) {
	t.Parallel()

	out := TranslateToJsonRpcException(NewErrUpstreamMethodIgnored("eth_getLogs", "alchemy-main"))
	var jre *ErrJsonRpcExceptionInternal
	require.True(t, errors.As(out, &jre))
	require.Equal(t,
		"method ignored by upstream: method ignored by upstream configuration",
		jre.Message)
	require.NotContains(t, jre.Message, "eth_getLogs",
		"DEFECT: the ignored method never reaches the client")
	require.NotContains(t, jre.Message, "alchemy-main",
		"DEFECT: the refusing upstream never reaches the client")
}
