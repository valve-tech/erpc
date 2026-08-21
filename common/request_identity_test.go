package common

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// labelledNetwork is the smallest Network the request accessors need. The
// alias and the id are separate so the label-vs-id fallback is observable.
type labelledNetwork struct {
	id       string
	label    string
	finality DataFinalityState
	calls    int
}

func (n *labelledNetwork) Id() string                        { return n.id }
func (n *labelledNetwork) Label() string                     { return n.label }
func (n *labelledNetwork) ProjectId() string                 { return "p" }
func (n *labelledNetwork) Architecture() NetworkArchitecture { return ArchitectureEvm }
func (n *labelledNetwork) Config() *NetworkConfig            { return nil }
func (n *labelledNetwork) Logger() *zerolog.Logger           { lg := zerolog.Nop(); return &lg }
func (n *labelledNetwork) GetMethodMetrics(string) TrackedMetrics {
	return nil
}
func (n *labelledNetwork) Forward(context.Context, *NormalizedRequest) (*NormalizedResponse, error) {
	return nil, nil
}
func (n *labelledNetwork) GetFinality(context.Context, *NormalizedRequest, *NormalizedResponse) DataFinalityState {
	n.calls++
	return n.finality
}

// TestNormalizedRequest_Validate_RejectsEveryUnusableShape guards the front
// door. Each rejected shape must carry the invalid-request code so the HTTP
// layer answers 400 instead of routing an unusable request to an upstream.
func TestNormalizedRequest_Validate_RejectsEveryUnusableShape(t *testing.T) {
	t.Parallel()

	t.Run("nil request", func(t *testing.T) {
		var r *NormalizedRequest
		err := r.Validate()
		require.Error(t, err)
		require.True(t, HasErrorCode(err, ErrCodeInvalidRequest))
		require.Contains(t, err.Error(), "request is nil")
	})

	t.Run("no body at all", func(t *testing.T) {
		err := NewNormalizedRequest(nil).Validate()
		require.Error(t, err)
		require.True(t, HasErrorCode(err, ErrCodeInvalidRequest))
		require.Contains(t, err.Error(), "request body is nil")
	})

	// The next two shapes both end in a rejection, but for different reasons.
	// Asserting only the outer invalid-request code cannot tell them apart, so
	// each also asserts the inner cause an operator would read in the log.
	t.Run("body that is not json", func(t *testing.T) {
		err := NewNormalizedRequest([]byte(`not json at all`)).Validate()
		require.Error(t, err)
		require.True(t, HasErrorCode(err, ErrCodeInvalidRequest))
		require.True(t, HasErrorCode(err, ErrCodeJsonRpcRequestUnmarshal),
			"a parse failure must be reported as a parse failure, not as a missing method")
		require.NotContains(t, err.Error(), "method is required")
	})

	t.Run("json with no method member", func(t *testing.T) {
		err := NewNormalizedRequest([]byte(`{"id":1,"params":[]}`)).Validate()
		require.Error(t, err)
		require.True(t, HasErrorCode(err, ErrCodeInvalidRequest))
	})

	t.Run("json with an empty method", func(t *testing.T) {
		err := NewNormalizedRequest([]byte(`{"id":1,"method":"","params":[]}`)).Validate()
		require.Error(t, err)
		require.True(t, HasErrorCode(err, ErrCodeInvalidRequest))
		require.Contains(t, err.Error(), "method is required")
	})
}

// TestNormalizedRequest_Validate_AcceptsAUsableRequest — the accept path must
// stay open, otherwise the rejections above would pass on a always-reject bug.
func TestNormalizedRequest_Validate_AcceptsAUsableRequest(t *testing.T) {
	t.Parallel()

	require.NoError(t, NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)).Validate())

	// A request built from a parsed JSON-RPC object has no raw body at all.
	require.NoError(t, NewNormalizedRequestFromJsonRpcRequest(
		NewJsonRpcRequest("eth_chainId", nil)).Validate())
}

// TestNormalizedRequest_CacheHash_MatchesTheUnderlyingJsonRpcKey checks the
// request-level wrapper delegates rather than inventing its own key. Two keys
// for one request means the cache never hits.
func TestNormalizedRequest_CacheHash_MatchesTheUnderlyingJsonRpcKey(t *testing.T) {
	t.Parallel()

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xaa","latest"]}`)
	r := NewNormalizedRequest(body)

	got, err := r.CacheHash()
	require.NoError(t, err)

	want, err := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xaa", "latest"}).CacheHash()
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestNormalizedRequest_CacheHash_FailsOnAnUnparseableBody — a cache key
// derived from a body erpc could not parse would be shared by every malformed
// request, so the failure must propagate.
func TestNormalizedRequest_CacheHash_FailsOnAnUnparseableBody(t *testing.T) {
	t.Parallel()

	h, err := NewNormalizedRequest([]byte(`{"method":`)).CacheHash()
	require.Error(t, err)
	require.True(t, HasErrorCode(err, ErrCodeJsonRpcRequestUnmarshal))
	require.Empty(t, h)
}

// TestNormalizedRequest_JsonRpcRequest_ParsesOnceAndReleasesTheBody covers the
// memory behaviour the parse path depends on: the raw upstream read buffer must
// stop being retained after a successful parse.
func TestNormalizedRequest_JsonRpcRequest_ParsesOnceAndReleasesTheBody(t *testing.T) {
	t.Parallel()

	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":3,"method":"eth_call","params":[]}`))
	require.NotNil(t, r.Body())

	first, err := r.JsonRpcRequest()
	require.NoError(t, err)
	require.Equal(t, "eth_call", first.Method)
	require.Nil(t, r.Body(), "the raw body must be dropped after a successful parse")

	second, err := r.JsonRpcRequest()
	require.NoError(t, err)
	require.Same(t, first, second, "re-parsing would lose in-place param rewrites")
}

// TestNormalizedRequest_JsonRpcRequest_RejectsABodyWithoutAMethod keeps a
// method-less request from reaching upstream routing, where it would match no
// method config at all and take a silent default path.
func TestNormalizedRequest_JsonRpcRequest_RejectsABodyWithoutAMethod(t *testing.T) {
	t.Parallel()

	_, err := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"params":[]}`)).JsonRpcRequest()
	require.Error(t, err)
	require.True(t, HasErrorCode(err, ErrorCode("ErrJsonRpcRequestUnresolvableMethod")))
}

// TestNormalizedRequest_Method_ReadsTheBodyWithoutFullyParsingIt covers the
// cheap path used before routing decisions.
func TestNormalizedRequest_Method_ReadsTheBodyWithoutFullyParsingIt(t *testing.T) {
	t.Parallel()

	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{}]}`))
	m, err := r.Method()
	require.NoError(t, err)
	require.Equal(t, "eth_getLogs", m)
	require.NotNil(t, r.Body(), "the peek must not consume the body")

	// The second call is served from the memo.
	m2, err := r.Method()
	require.NoError(t, err)
	require.Equal(t, "eth_getLogs", m2)
}

// TestNormalizedRequest_Method_OnAnEmptyRequest — no body and no parsed object
// must be an error, not the empty string treated as a valid method.
func TestNormalizedRequest_Method_OnAnEmptyRequest(t *testing.T) {
	t.Parallel()

	_, err := NewNormalizedRequest(nil).Method()
	require.Error(t, err)
	require.True(t, HasErrorCode(err, ErrorCode("ErrJsonRpcRequestUnresolvableMethod")))

	var nilReq *NormalizedRequest
	m, err := nilReq.Method()
	require.NoError(t, err)
	require.Equal(t, "", m)
}

// TestNormalizedRequest_ID_ComesFromTheParsedJsonRpcRequest — the id is how a
// multiplexed response finds its caller.
func TestNormalizedRequest_ID_ComesFromTheParsedJsonRpcRequest(t *testing.T) {
	t.Parallel()

	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":88,"method":"eth_call"}`))
	require.Equal(t, int64(88), r.ID())

	var nilReq *NormalizedRequest
	require.Nil(t, nilReq.ID())

	// An unparseable body has no id, and asking must not panic.
	require.Nil(t, NewNormalizedRequest([]byte(`{`)).ID())
}

// TestNormalizedRequest_NetworkIdAndLabel_FallBackToNotAvailable keeps metric
// cardinality bounded and keeps a nil network from panicking on a log line.
func TestNormalizedRequest_NetworkIdAndLabel_FallBackToNotAvailable(t *testing.T) {
	t.Parallel()

	var nilReq *NormalizedRequest
	require.Equal(t, "n/a", nilReq.NetworkId())
	require.Equal(t, "n/a", nilReq.NetworkLabel())
	require.Nil(t, nilReq.Network())

	unattached := NewNormalizedRequest([]byte(`{"method":"m"}`))
	require.Equal(t, "n/a", unattached.NetworkId())
	require.Equal(t, "n/a", unattached.NetworkLabel())
}

// TestNormalizedRequest_NetworkLabel_PrefersTheAliasOverTheCanonicalId is what
// an operator reads on a dashboard: "ethereum-mainnet", not "evm:1". The
// fallback matters just as much, since an unaliased network must not report an
// empty label and collapse every network into one metric series.
func TestNormalizedRequest_NetworkLabel_PrefersTheAliasOverTheCanonicalId(t *testing.T) {
	t.Parallel()

	aliased := NewNormalizedRequest([]byte(`{"method":"m"}`))
	aliased.SetNetwork(&labelledNetwork{id: "evm:1", label: "ethereum-mainnet"})
	require.Equal(t, "ethereum-mainnet", aliased.NetworkLabel())
	require.Equal(t, "evm:1", aliased.NetworkId())

	unaliased := NewNormalizedRequest([]byte(`{"method":"m"}`))
	unaliased.SetNetwork(&labelledNetwork{id: "evm:42", label: ""})
	require.Equal(t, "evm:42", unaliased.NetworkLabel())
}

// TestNormalizedRequest_Finality_CachesOnlyADefiniteAnswer. Caching "unknown"
// would freeze a request at unknown finality for its whole lifetime, and the
// cache TTL chosen from finality would then be wrong for every retry.
func TestNormalizedRequest_Finality_CachesOnlyADefiniteAnswer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("a definite answer is computed once", func(t *testing.T) {
		n := &labelledNetwork{id: "evm:1", finality: DataFinalityStateFinalized}
		r := NewNormalizedRequest([]byte(`{"method":"m"}`))
		r.SetNetwork(n)

		require.Equal(t, DataFinalityStateFinalized, r.Finality(ctx))
		require.Equal(t, DataFinalityStateFinalized, r.Finality(ctx))
		require.Equal(t, 1, n.calls, "the second read must come from the memo")
	})

	t.Run("unknown is asked again", func(t *testing.T) {
		n := &labelledNetwork{id: "evm:1", finality: DataFinalityStateUnknown}
		r := NewNormalizedRequest([]byte(`{"method":"m"}`))
		r.SetNetwork(n)

		require.Equal(t, DataFinalityStateUnknown, r.Finality(ctx))
		require.Equal(t, DataFinalityStateUnknown, r.Finality(ctx))
		require.Equal(t, 2, n.calls, "an unknown answer must not be cached")
	})

	t.Run("no network means unknown", func(t *testing.T) {
		require.Equal(t, DataFinalityStateUnknown,
			NewNormalizedRequest([]byte(`{"method":"m"}`)).Finality(ctx))

		var nilReq *NormalizedRequest
		require.Equal(t, DataFinalityStateUnknown, nilReq.Finality(ctx))
	})
}

// TestNormalizedRequest_CompositeType_DefaultsToNone drives the split-request
// machinery. A request wrongly reported as composite is re-split forever.
func TestNormalizedRequest_CompositeType_DefaultsToNone(t *testing.T) {
	t.Parallel()

	r := NewNormalizedRequest([]byte(`{"method":"eth_getLogs"}`))
	require.Equal(t, CompositeTypeNone, r.CompositeType())
	require.False(t, r.IsCompositeRequest())

	r.SetCompositeType(CompositeTypeLogsSplitOnError)
	require.Equal(t, CompositeTypeLogsSplitOnError, r.CompositeType())
	require.True(t, r.IsCompositeRequest())

	// An empty string must not clear the marker — that would silently turn a
	// split sub-request back into a top-level one mid-flight.
	r.SetCompositeType("")
	require.Equal(t, CompositeTypeLogsSplitOnError, r.CompositeType())

	var nilReq *NormalizedRequest
	require.False(t, nilReq.IsCompositeRequest())
	require.Equal(t, "", nilReq.CompositeType())
}

// TestNormalizedRequest_ParentRequestId_LinksASubRequestToItsParent — without
// the link, a split sub-request's logs and metrics cannot be attributed.
func TestNormalizedRequest_ParentRequestId_LinksASubRequestToItsParent(t *testing.T) {
	t.Parallel()

	r := NewNormalizedRequest([]byte(`{"method":"eth_getLogs"}`))
	require.Nil(t, r.ParentRequestId())

	r.SetParentRequestId(int64(42))
	require.Equal(t, int64(42), r.ParentRequestId())

	// A nil parent must not wipe an existing link.
	r.SetParentRequestId(nil)
	require.Equal(t, int64(42), r.ParentRequestId())

	var nilReq *NormalizedRequest
	require.Nil(t, nilReq.ParentRequestId())
	require.NotPanics(t, func() { nilReq.SetParentRequestId(int64(1)) })
}

// TestNormalizedRequest_EvmBlockRefAndNumber_AreStickyOnceSet — the block ref
// feeds the cache key and the finality decision, so a later nil must not erase
// what an earlier hook resolved.
func TestNormalizedRequest_EvmBlockRefAndNumber_AreStickyOnceSet(t *testing.T) {
	t.Parallel()

	r := NewNormalizedRequest([]byte(`{"method":"eth_getBlockByNumber"}`))
	require.Nil(t, r.EvmBlockRef())
	require.Nil(t, r.EvmBlockNumber())

	r.SetEvmBlockRef("0x1234")
	r.SetEvmBlockNumber(int64(4660))
	require.Equal(t, "0x1234", r.EvmBlockRef())
	require.Equal(t, int64(4660), r.EvmBlockNumber())

	r.SetEvmBlockRef(nil)
	r.SetEvmBlockNumber(nil)
	require.Equal(t, "0x1234", r.EvmBlockRef())
	require.Equal(t, int64(4660), r.EvmBlockNumber())

	var nilReq *NormalizedRequest
	require.Nil(t, nilReq.EvmBlockRef())
	require.Nil(t, nilReq.EvmBlockNumber())
	require.NotPanics(t, func() {
		nilReq.SetEvmBlockRef("0x1")
		nilReq.SetEvmBlockNumber(int64(1))
	})
}

// TestNormalizedRequest_ClientIP_ReportsNotAvailableRatherThanEmpty keeps the
// rate-limit and audit labels bounded: an empty string as a metric label value
// is indistinguishable from a missing label.
func TestNormalizedRequest_ClientIP_ReportsNotAvailableRatherThanEmpty(t *testing.T) {
	t.Parallel()

	r := NewNormalizedRequest([]byte(`{"method":"m"}`))
	require.Equal(t, "n/a", r.ClientIP())

	r.SetClientIP("203.0.113.7")
	require.Equal(t, "203.0.113.7", r.ClientIP())

	// Storing an empty value must not turn the label into "".
	r.SetClientIP("")
	require.Equal(t, "n/a", r.ClientIP())

	var nilReq *NormalizedRequest
	require.Equal(t, "n/a", nilReq.ClientIP())
	require.NotPanics(t, func() { nilReq.SetClientIP("1.2.3.4") })
}

// TestNormalizedRequest_MarshalJSON_PrefersTheRawBody so a debug dump shows the
// request the client actually sent, params and all.
func TestNormalizedRequest_MarshalJSON_PrefersTheRawBody(t *testing.T) {
	t.Parallel()

	body := `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xaa"}]}`
	r := NewNormalizedRequest([]byte(body))

	b, err := r.MarshalJSON()
	require.NoError(t, err)
	require.Equal(t, body, string(b))
}

// TestNormalizedRequest_MarshalJSON_FallsBackToTheMethodAlone records what an
// operator sees once the raw body has been released: the method survives, the
// params do not. That is a real diagnostic gap worth knowing about.
func TestNormalizedRequest_MarshalJSON_FallsBackToTheMethodAlone(t *testing.T) {
	t.Parallel()

	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0xaa"}]}`))
	_, err := r.JsonRpcRequest() // releases r.body
	require.NoError(t, err)

	b, err := r.MarshalJSON()
	require.NoError(t, err)
	require.Equal(t, `{"method":"eth_call"}`, string(b))

	// Nothing at all marshals to nothing rather than to an error.
	empty := NewNormalizedRequest(nil)
	b, err = empty.MarshalJSON()
	require.NoError(t, err)
	require.Nil(t, b)
}

// TestNormalizedRequest_MarshalZerologObject_NamesTheUpstreamAndNetwork is the
// log line an operator reads during an incident. It must identify which
// upstream last served the request, and must not blow up when nothing did.
func TestNormalizedRequest_MarshalZerologObject_NamesTheUpstreamAndNetwork(t *testing.T) {

	r := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	r.SetNetwork(&labelledNetwork{id: "evm:137", label: "polygon"})

	line := renderZerolog(t, r)
	require.Contains(t, line, `"networkId":"evm:137"`)
	require.Contains(t, line, `"lastUpstream":"nil"`)
	require.Contains(t, line, `eth_call`)

	r.SetLastUpstream(NewFakeUpstream("alchemy-main"))
	line = renderZerolog(t, r)
	require.Contains(t, line, `"lastUpstream":"alchemy-main"`)
}

// TestNormalizedRequest_MarshalZerologObject_QuotesABodyThatIsPlainlyNotJson
// pins a DEFECT. IsSemiValidJson only looks at the first byte, so any body
// starting with n, t or f (say "not json", or a proxy's plain-text
// "failed to connect") is spliced into the log line with RawJSON. The result is
// a log EVENT that no JSON parser accepts, so a log pipeline drops the whole
// record — including the lastUpstream and networkId an operator needs. A body
// that starts with any other letter takes the quoted path and is fine, which is
// why this only bites some of the time.
func TestNormalizedRequest_MarshalZerologObject_QuotesABodyThatIsPlainlyNotJson(t *testing.T) {
	// A body starting with a letter outside n/t/f is quoted correctly.
	safe := renderZerolog(t, NewNormalizedRequest([]byte("garbage")))
	require.Contains(t, safe, `"body":"garbage"`)
	require.True(t, json.Valid([]byte(safe)), "this log line is well-formed")

	// The same body starting with 'n' corrupts the whole record.
	broken := renderZerolog(t, NewNormalizedRequest([]byte("not json")))
	require.Contains(t, broken, `"body":not json`)
	require.False(t, json.Valid([]byte(broken)),
		"DEFECT: a client body beginning with n/t/f makes the log event unparseable")

	var nilReq *NormalizedRequest
	require.NotPanics(t, func() { renderZerolog(t, nilReq) })
}

// renderZerolog renders a zerolog object marshaller into a single log line so
// the assertions above can read what an operator would see.
func renderZerolog(t *testing.T, obj zerolog.LogObjectMarshaler) string {
	t.Helper()
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })

	var buf bytes.Buffer
	lg := zerolog.New(&buf).Level(zerolog.DebugLevel)
	lg.Debug().Object("rq", obj).Msg("request")
	return buf.String()
}
