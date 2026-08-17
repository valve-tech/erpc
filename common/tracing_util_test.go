package common

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ---------------------------------------------------------------------------
// StartSpan / StartDetailSpan
// ---------------------------------------------------------------------------

// With tracing off, both span helpers must return the no-op span and must
// survive every method a caller can invoke on it. eRPC runs untraced by
// default, so a panic here would take down every request.
func TestStartSpan_DisabledReturnsUsableNoopSpan(t *testing.T) {
	saveTracingGlobals(t)
	IsTracingEnabled = false
	IsTracingDetailed = false

	base := context.Background()

	for _, tc := range []struct {
		name  string
		start func(context.Context, string, ...trace.SpanStartOption) (context.Context, trace.Span)
	}{
		{"StartSpan", StartSpan},
		{"StartDetailSpan", StartDetailSpan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, span := tc.start(base, "Some.Operation")

			assert.Equal(t, base, ctx, "a no-op span must not replace the caller's context")
			assert.False(t, span.IsRecording(), "a no-op span must report that it does not record")
			assert.False(t, span.SpanContext().IsValid(), "a no-op span has no span context")
			assert.Nil(t, span.TracerProvider())

			assert.NotPanics(t, func() {
				span.SetAttributes(attribute.String("k", "v"))
				span.AddEvent("an event")
				span.SetStatus(codes.Error, "nope")
				span.SetName("renamed")
				span.RecordError(errors.New("boom"))
				span.End()
			})
		})
	}
}

// Detailed spans exist to carry high-cardinality data. When an operator turns
// `detailed` off they must stop being created, or the cost they were turned off
// to avoid comes back.
func TestStartDetailSpan_GatedByDetailedFlag(t *testing.T) {
	t.Run("detailed off suppresses the span", func(t *testing.T) {
		h := newTracingHarness(t, false)

		_, span := StartDetailSpan(context.Background(), "Detail.Op")
		span.End()

		assert.Empty(t, h.ended(), "no detail span may be recorded when detailed is off")
	})

	t.Run("detailed on creates the span", func(t *testing.T) {
		h := newTracingHarness(t, true)

		_, span := StartDetailSpan(context.Background(), "Detail.Op")
		span.End()

		require.NotNil(t, h.endedNamed("Detail.Op"))
	})
}

// StartSpan must ignore the detailed flag — it is the always-on tier.
func TestStartSpan_EnabledIgnoresDetailedFlag(t *testing.T) {
	h := newTracingHarness(t, false)

	ctx, span := StartSpan(context.Background(), "Major.Op", trace.WithSpanKind(trace.SpanKindClient))
	assert.True(t, span.IsRecording())
	assert.Equal(t, span.SpanContext().SpanID(), trace.SpanFromContext(ctx).SpanContext().SpanID(),
		"the returned context must carry the new span")
	span.End()

	rec := h.endedNamed("Major.Op")
	assert.Equal(t, trace.SpanKindClient, rec.SpanKind())
	assert.Empty(t, h.startedButNotEnded())
}

// ---------------------------------------------------------------------------
// HTTP trace context propagation
// ---------------------------------------------------------------------------

func TestExtractHTTPRequestTraceContext(t *testing.T) {
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const spanID = "00f067aa0ba902b7"

	newReq := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://erpc.local/main/evm/1", nil)
		r.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
		return r
	}

	t.Run("disabled returns the request context untouched", func(t *testing.T) {
		saveTracingGlobals(t)
		IsTracingEnabled = false

		ctx := ExtractHTTPRequestTraceContext(newReq())
		assert.False(t, trace.SpanContextFromContext(ctx).IsValid(),
			"no parent may be adopted when tracing is off")
	})

	t.Run("enabled adopts the incoming traceparent", func(t *testing.T) {
		saveTracingGlobals(t)
		IsTracingEnabled = true

		ctx := ExtractHTTPRequestTraceContext(newReq())
		sc := trace.SpanContextFromContext(ctx)
		require.True(t, sc.IsValid(), "the traceparent header must produce a valid parent")
		assert.Equal(t, traceID, sc.TraceID().String())
		assert.Equal(t, spanID, sc.SpanID().String())
	})
}

func TestInjectHTTPResponseTraceContext(t *testing.T) {
	t.Run("disabled writes no header", func(t *testing.T) {
		saveTracingGlobals(t)
		IsTracingEnabled = false

		w := httptest.NewRecorder()
		InjectHTTPResponseTraceContext(context.Background(), w)
		assert.Empty(t, w.Header().Get("traceparent"))
	})

	t.Run("enabled writes the active span as traceparent", func(t *testing.T) {
		h := newTracingHarness(t, false)

		ctx, span := StartSpan(context.Background(), "Http.ReceivedRequest")
		w := httptest.NewRecorder()
		InjectHTTPResponseTraceContext(ctx, w)
		span.End()
		_ = h

		tp := w.Header().Get("traceparent")
		require.NotEmpty(t, tp, "the client must be able to correlate on the response")
		assert.Contains(t, tp, span.SpanContext().TraceID().String())
		assert.Contains(t, tp, span.SpanContext().SpanID().String())
	})
}

// ---------------------------------------------------------------------------
// StartHTTPServerSpan / shouldForceTrace
// ---------------------------------------------------------------------------

func TestStartHTTPServerSpan_Disabled(t *testing.T) {
	saveTracingGlobals(t)
	IsTracingEnabled = false

	base := context.Background()
	r := httptest.NewRequest(http.MethodPost, "http://erpc.local/main/evm/1", nil)

	ctx, span := StartHTTPServerSpan(base, r)
	assert.Equal(t, base, ctx)
	assert.False(t, span.IsRecording())
}

// The server span carries the HTTP facts an operator searches on, and it must
// adopt the caller's trace so the request joins their trace, not a new one.
func TestStartHTTPServerSpan_AttributesAndParent(t *testing.T) {
	h := newTracingHarness(t, false)

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	r := httptest.NewRequest(http.MethodPost, "https://erpc.local/main/evm/1?x=1", nil)
	r.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	r.Header.Set("User-Agent", "erpc-test-agent/9")

	_, span := StartHTTPServerSpan(context.Background(), r)
	span.End()

	rec := h.endedNamed("Http.ReceivedRequest")
	attrs := spanAttrs(rec)

	assert.Equal(t, trace.SpanKindServer, rec.SpanKind(),
		"span.kind must be server so tracing back ends draw the entry edge")
	assert.Equal(t, traceID, rec.SpanContext().TraceID().String(),
		"the span must join the caller's trace")
	assert.Equal(t, http.MethodPost, attrs[attribute.Key("http.method")].AsString())
	assert.Equal(t, r.URL.String(), attrs[attribute.Key("http.url")].AsString())
	assert.Equal(t, "https", attrs[attribute.Key("http.scheme")].AsString())
	assert.Equal(t, "erpc-test-agent/9", attrs[attribute.Key("http.user_agent")].AsString())

	_, forced := attrs[attribute.Key("erpc.forced_trace_reason")]
	assert.False(t, forced, "an ordinary request must not be marked as force-traced")
}

// Force-tracing is the operator's escape hatch: add the header and the request
// is traced even at sampleRate 0. This drives the real sampler so the test
// fails if either the attribute or the sampler bypass breaks.
func TestStartHTTPServerSpan_ForceTraceBypassesNeverSample(t *testing.T) {
	for _, tc := range []struct {
		name       string
		header     string
		query      string
		wantForced bool
	}{
		{name: "no force marker", wantForced: false},
		{name: "header true", header: "true", wantForced: true},
		{name: "header 1", header: "1", wantForced: true},
		{name: "header yes", header: "yes", wantForced: true},
		{name: "header with an unrecognised value", header: "please", wantForced: false},
		{name: "query true", query: "true", wantForced: true},
		{name: "query 1", query: "1", wantForced: true},
		{name: "query yes", query: "yes", wantForced: true},
		{name: "query with an unrecognised value", query: "maybe", wantForced: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// sampleRate 0 means the base sampler drops everything, so the only
			// way a span survives is the force-trace bypass.
			h := newTracingHarnessWithSampler(t, false, createTracingSampler(&TracingConfig{SampleRate: 0}))

			url := "http://erpc.local/main/evm/1"
			if tc.query != "" {
				url += "?" + ForceTraceQueryParam + "=" + tc.query
			}
			r := httptest.NewRequest(http.MethodPost, url, nil)
			if tc.header != "" {
				r.Header.Set(ForceTraceHeader, tc.header)
			}

			_, span := StartHTTPServerSpan(context.Background(), r)
			span.End()

			if !tc.wantForced {
				assert.Empty(t, h.ended(), "an unforced request must be dropped at sampleRate 0")
				return
			}

			rec := h.endedNamed("Http.ReceivedRequest")
			attrs := spanAttrs(rec)
			assert.True(t, attrs[attribute.Key(forceTraceAttributeKey)].AsBool(),
				"the sampler-facing force attribute must be set")
			assert.Equal(t, "header_or_query", attrs[attribute.Key("erpc.forced_trace_reason")].AsString(),
				"the operator must be able to see why the span was kept")
		})
	}
}

// The header takes precedence: a present header short-circuits the query check.
func TestShouldForceTrace_HeaderShortCircuitsQuery(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost,
		"http://erpc.local/main/evm/1?"+ForceTraceQueryParam+"=true", nil)
	r.Header.Set(ForceTraceHeader, "false")

	assert.False(t, shouldForceTrace(r),
		"a present but negative header must win over a truthy query param")
}

// ---------------------------------------------------------------------------
// EnrichHTTPServerSpan
// ---------------------------------------------------------------------------

func TestEnrichHTTPServerSpan(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		err        error
		wantCode   codes.Code
		wantDesc   string
		wantEvents int
	}{
		{name: "success is Ok", statusCode: 200, wantCode: codes.Ok, wantDesc: ""},
		{name: "redirect is Ok", statusCode: 302, wantCode: codes.Ok, wantDesc: ""},
		{name: "client error is Error", statusCode: 400, wantCode: codes.Error, wantDesc: "Bad Request"},
		{name: "server error is Error", statusCode: 500, wantCode: codes.Error, wantDesc: "Internal Server Error"},
		{
			name: "an error wins over the status code", statusCode: 200,
			err: NewErrInvalidRequest(errors.New("bad json")), wantCode: codes.Error,
			wantDesc: string(ErrCodeInvalidRequest), wantEvents: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTracingHarness(t, false)

			ctx, span := StartSpan(context.Background(), "Http.ReceivedRequest")
			EnrichHTTPServerSpan(ctx, tc.statusCode, tc.err)
			span.End()

			rec := h.endedNamed("Http.ReceivedRequest")
			attrs := spanAttrs(rec)

			assert.Equal(t, int64(tc.statusCode), attrs[attribute.Key("http.status_code")].AsInt64())
			assert.Equal(t, tc.wantCode, rec.Status().Code)
			assert.Equal(t, tc.wantDesc, rec.Status().Description)
			assert.Len(t, rec.Events(), tc.wantEvents)
		})
	}
}

// A context with no recording span must be ignored rather than panic.
func TestEnrichHTTPServerSpan_NoRecordingSpan(t *testing.T) {
	saveTracingGlobals(t)
	IsTracingEnabled = false

	assert.NotPanics(t, func() {
		EnrichHTTPServerSpan(context.Background(), 500, errors.New("boom"))
	})
}

// ---------------------------------------------------------------------------
// StartRequestSpan / EndRequestSpan
// ---------------------------------------------------------------------------

func TestStartRequestSpan_Disabled(t *testing.T) {
	saveTracingGlobals(t)
	IsTracingEnabled = false

	base := context.Background()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))

	assert.Equal(t, base, StartRequestSpan(base, req))
}

// The request span names the JSON-RPC method. Detailed mode adds the request id
// and the verbatim params; plain mode must not, because params are unbounded.
func TestStartRequestSpan_MethodAlwaysDetailOnlyWhenDetailed(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":77,"method":"eth_getBalance","params":["0xabc","latest"]}`

	t.Run("plain mode carries the method only", func(t *testing.T) {
		h := newTracingHarness(t, false)

		ctx := StartRequestSpan(context.Background(), NewNormalizedRequest([]byte(body)))
		trace.SpanFromContext(ctx).End()

		attrs := spanAttrs(h.endedNamed("Request.Handle"))
		assert.Equal(t, "eth_getBalance", attrs[attribute.Key("request.method")].AsString())

		_, hasID := attrs[attribute.Key("request.id")]
		assert.False(t, hasID, "the request id is high cardinality and belongs to detailed mode")
		_, hasParams := attrs[attribute.Key("request.jsonrpc.params")]
		assert.False(t, hasParams, "verbatim params must never appear outside detailed mode")
	})

	t.Run("detailed mode adds the id and the params", func(t *testing.T) {
		h := newTracingHarness(t, true)

		ctx := StartRequestSpan(context.Background(), NewNormalizedRequest([]byte(body)))
		trace.SpanFromContext(ctx).End()

		rec := h.endedNamed("Request.Handle")
		assert.Equal(t, trace.SpanKindInternal, rec.SpanKind())
		attrs := spanAttrs(rec)
		assert.Equal(t, "eth_getBalance", attrs[attribute.Key("request.method")].AsString())
		assert.Equal(t, "77", attrs[attribute.Key("request.id")].AsString())
		assert.Equal(t, `["0xabc","latest"]`, attrs[attribute.Key("request.jsonrpc.params")].AsString())
	})
}

// Force-tracing by matcher is the config-driven twin of the header escape
// hatch. At sampleRate 0 only a matched request may survive.
func TestStartRequestSpan_ForceTraceByMatcher(t *testing.T) {
	for _, tc := range []struct {
		name       string
		matchers   []*ForceTraceMatcher
		network    string
		wantForced bool
		wantReason string
	}{
		{name: "no matcher, span dropped", matchers: nil, network: "evm:1", wantForced: false},
		{
			name: "method matcher forces the span", matchers: []*ForceTraceMatcher{{Method: "eth_call"}},
			network: "evm:1", wantForced: true, wantReason: "method:eth_call",
		},
		{
			name: "network matcher forces the span", matchers: []*ForceTraceMatcher{{Network: "evm:1"}},
			network: "evm:1", wantForced: true, wantReason: "network:evm:1",
		},
		{
			name: "network matcher misses when the context has no network",
			matchers: []*ForceTraceMatcher{{Network: "evm:1"}}, network: "",
			wantForced: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTracingHarnessWithSampler(t, false, createTracingSampler(&TracingConfig{SampleRate: 0}))
			forceTraceMatchers = tc.matchers

			ctx := SetForceTraceNetwork(context.Background(), tc.network)
			ctx = StartRequestSpan(ctx, NewNormalizedRequest(
				[]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`)))
			trace.SpanFromContext(ctx).End()

			if !tc.wantForced {
				assert.Empty(t, h.ended(), "an unmatched request must be dropped at sampleRate 0")
				return
			}

			attrs := spanAttrs(h.endedNamed("Request.Handle"))
			assert.True(t, attrs[attribute.Key(forceTraceAttributeKey)].AsBool())
			assert.Equal(t, tc.wantReason, attrs[attribute.Key("erpc.forced_trace_reason")].AsString())
		})
	}
}

func TestEndRequestSpan_Disabled(t *testing.T) {
	saveTracingGlobals(t)
	IsTracingEnabled = false

	assert.NotPanics(t, func() {
		EndRequestSpan(context.Background(), nil, nil)
	})
}

// EndRequestSpan must end the span on every path. An unended span never reaches
// the collector at all, so the operator sees the request simply vanish.
func TestEndRequestSpan_EndsSpanOnEveryPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp func() *NormalizedResponse
		err  interface{}
	}{
		{name: "error path", resp: func() *NormalizedResponse { return nil }, err: NewErrInvalidRequest(errors.New("bad"))},
		{name: "success path", resp: func() *NormalizedResponse { return NewNormalizedResponse() }},
		{name: "neither response nor error", resp: func() *NormalizedResponse { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newTracingHarness(t, false)

			ctx := StartRequestSpan(context.Background(), NewNormalizedRequest(
				[]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`)))
			EndRequestSpan(ctx, tc.resp(), tc.err)

			assert.Empty(t, h.startedButNotEnded(), "EndRequestSpan must end the span")
			require.NotNil(t, h.endedNamed("Request.Handle"))
		})
	}
}

// On the success path the span reports the cache verdict and the upstream that
// served the request — the two facts an operator reads first.
func TestEndRequestSpan_SuccessAttributes(t *testing.T) {
	h := newTracingHarness(t, false)

	ctx := StartRequestSpan(context.Background(), NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`)))

	resp := NewNormalizedResponse().WithFromCache(true)
	resp.SetUpstream(NewFakeUpstream("rpc-alpha"))
	EndRequestSpan(ctx, resp, nil)

	rec := h.endedNamed("Request.Handle")
	attrs := spanAttrs(rec)

	assert.Equal(t, codes.Ok, rec.Status().Code)
	assert.True(t, attrs[attribute.Key("cache.hit")].AsBool(),
		"cache.hit must follow the response, not a constant")
	assert.Equal(t, "rpc-alpha", attrs[attribute.Key("upstream.id")].AsString())
}

// A cache miss must be reported as a miss. This catches an inverted or
// hard-coded cache.hit.
func TestEndRequestSpan_CacheMissReportedAsMiss(t *testing.T) {
	h := newTracingHarness(t, false)

	ctx := StartRequestSpan(context.Background(), NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`)))
	EndRequestSpan(ctx, NewNormalizedResponse().WithFromCache(false), nil)

	attrs := spanAttrs(h.endedNamed("Request.Handle"))
	assert.False(t, attrs[attribute.Key("cache.hit")].AsBool())

	_, hasUpstream := attrs[attribute.Key("upstream.id")]
	assert.False(t, hasUpstream, "no upstream.id may be invented when none served the response")
}

// Detailed mode adds the execution counters and the result size. Each counter
// comes from its own accessor, so a copy-paste slip would tie them together.
func TestEndRequestSpan_DetailedAttributes(t *testing.T) {
	h := newTracingHarness(t, true)

	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	ctx := StartRequestSpan(context.Background(), req)

	jrr, err := NewJsonRpcResponse(1, "0x1234", nil)
	require.NoError(t, err)

	resp := NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	resp.SetUpstream(NewFakeUpstream("rpc-beta"))
	resp.SetAttempts(4)
	resp.SetRetries(3)
	resp.SetHedges(2)

	EndRequestSpan(ctx, resp, nil)

	attrs := spanAttrs(h.endedNamed("Request.Handle"))

	assert.Equal(t, int64(4), attrs[attribute.Key("execution.attempts")].AsInt64())
	assert.Equal(t, int64(3), attrs[attribute.Key("execution.retries")].AsInt64())
	assert.Equal(t, int64(2), attrs[attribute.Key("execution.hedges")].AsInt64())
	assert.Equal(t, int64(jrr.ResultLength()), attrs[attribute.Key("response.result_size")].AsInt64())

	_, hasUser := attrs[attribute.Key("user.id")]
	assert.True(t, hasUser, "detailed mode must record the caller identity")
	_, hasReqFinality := attrs[attribute.Key("request.finality")]
	assert.True(t, hasReqFinality)
	_, hasRespFinality := attrs[attribute.Key("response.finality")]
	assert.True(t, hasRespFinality)
}

// The error path must record the error and must not claim a cache verdict.
func TestEndRequestSpan_ErrorPath(t *testing.T) {
	h := newTracingHarness(t, false)

	ctx := StartRequestSpan(context.Background(), NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`)))
	EndRequestSpan(ctx, NewNormalizedResponse(), NewErrInvalidRequest(errors.New("bad json")))

	rec := h.endedNamed("Request.Handle")
	attrs := spanAttrs(rec)

	assert.Equal(t, codes.Error, rec.Status().Code)
	assert.Equal(t, string(ErrCodeInvalidRequest), rec.Status().Description)
	require.Len(t, rec.Events(), 1, "the error must be recorded on the span")

	_, hasCacheHit := attrs[attribute.Key("cache.hit")]
	assert.False(t, hasCacheHit, "the error path must not report a cache verdict")
}

// stubNetwork is the smallest Network a span can name. EndRequestSpan reads the
// id and asks the network to classify finality; every other method stays
// unimplemented so a new call site fails loudly instead of quietly.
type stubNetwork struct {
	Network
	id string
}

func (n *stubNetwork) Id() string { return n.id }

func (n *stubNetwork) GetFinality(context.Context, *NormalizedRequest, *NormalizedResponse) DataFinalityState {
	return DataFinalityStateFinalized
}

// Detailed mode names the network the request was served on.
func TestEndRequestSpan_DetailedRecordsTheNetwork(t *testing.T) {
	h := newTracingHarness(t, true)

	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	req.SetNetwork(&stubNetwork{id: "evm:42161"})
	ctx := StartRequestSpan(context.Background(), req)

	EndRequestSpan(ctx, NewNormalizedResponse().WithRequest(req), nil)

	attrs := spanAttrs(h.endedNamed("Request.Handle"))
	assert.Equal(t, "evm:42161", attrs[attribute.Key("network.id")].AsString())
}

// With sampling on, most spans do not record. EndRequestSpan must return early
// on those rather than pay for the attributes, and must not panic.
func TestEndRequestSpan_NonRecordingSpanIsIgnored(t *testing.T) {
	h := newTracingHarnessWithSampler(t, true, createTracingSampler(&TracingConfig{SampleRate: 0}))

	ctx := StartRequestSpan(context.Background(), NewNormalizedRequest(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`)))
	require.False(t, trace.SpanFromContext(ctx).IsRecording(), "the fixture needs a dropped span")

	assert.NotPanics(t, func() {
		EndRequestSpan(ctx, NewNormalizedResponse().WithFromCache(true), nil)
	})
	assert.Empty(t, h.ended())
}

// ---------------------------------------------------------------------------
// ForceFlushTraces
// ---------------------------------------------------------------------------

func TestForceFlushTraces(t *testing.T) {
	t.Run("no error when tracing is off", func(t *testing.T) {
		saveTracingGlobals(t)
		IsTracingEnabled = false
		tracerProvider = nil

		require.NoError(t, ForceFlushTraces(context.Background()))
	})

	t.Run("no error when enabled but no provider is installed", func(t *testing.T) {
		saveTracingGlobals(t)
		IsTracingEnabled = true
		tracerProvider = nil

		require.NoError(t, ForceFlushTraces(context.Background()))
	})

	t.Run("flushes through the installed provider", func(t *testing.T) {
		h := newTracingHarness(t, false)

		_, span := StartSpan(context.Background(), "Flush.Op")
		span.End()

		require.NoError(t, ForceFlushTraces(context.Background()))
		require.NotNil(t, h.endedNamed("Flush.Op"))
	})
}

// A sanity check on the harness itself: an unended span must be reported.
func TestTracingHarness_DetectsUnendedSpan(t *testing.T) {
	h := newTracingHarness(t, false)

	_, span := StartSpan(context.Background(), "Never.Ended")
	_ = span

	assert.Equal(t, []string{"Never.Ended"}, h.startedButNotEnded())
	span.End()
	assert.Empty(t, h.startedButNotEnded())
}
