package erpc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// httpServerSpan returns the single "Http.ReceivedRequest" span the exporter
// collected. The handler emits exactly one per request, so more than one means
// the test drove more traffic than it meant to.
func httpServerSpan(t *testing.T, exp *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	var found []tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		if s.Name == "Http.ReceivedRequest" {
			found = append(found, s)
		}
	}
	require.Len(t, found, 1, "the handler must emit exactly one HTTP server span")
	return found[0]
}

// recordSpans installs an in-memory tracer provider for the duration of one
// test and hands back the exporter.
func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	common.SetTracerProviderForTest(tp)
	t.Cleanup(func() {
		common.IsTracingEnabled = false
		common.IsTracingDetailed = false
	})
	return exp
}

// TestRequestHandler_AFatalPostClosesItsSpanAsAnError pins the trace, not the
// body. A JSON-RPC POST keeps transport 200 by design, so the span status is
// the ONLY place an operator learns that the server died on this request. See
// entry 130 in valve/upstream-bug-log.md.
func TestRequestHandler_AFatalPostClosesItsSpanAsAnError(t *testing.T) {
	exp := recordSpans(t)

	// erpc is nil, so the project lookup panics inside the handler and the
	// top-level recovery routes the panic into writeFatalError.
	s := faultHandlerServer(t)
	h := s.createRequestHandler()

	w := newFaultResponseWriter()
	r := httptest.NewRequest(http.MethodPost, "http://erpc.test/proj/evm/123",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))

	require.NotPanics(t, func() { h.ServeHTTP(w, r) })

	// The body was always right. Assert it so a regression here is not read as
	// a tracing-only change.
	require.Equal(t, http.StatusOK, w.Status())
	require.Contains(t, w.Body(), "panic")

	span := httpServerSpan(t, exp)
	assert.Equal(t, codes.Error, span.Status.Code,
		"a request that panicked must not close its span as OK")
	assert.Contains(t, span.Status.Description, "panic",
		"the span must name the fault, not an empty description")
	require.NotEmpty(t, span.Events, "the fatal error must be recorded on the span")
}

// TestRequestHandler_AFatalPostKeepsTheOriginalMessageInTheBody pins the other
// consumer of the same variable: the JSON body must carry the fault eRPC was
// given, never an encoder's own complaint about it.
func TestRequestHandler_AFatalPostKeepsTheOriginalMessageInTheBody(t *testing.T) {
	s := faultHandlerServer(t)
	h := s.createRequestHandler()

	w := newFaultResponseWriter()
	r := httptest.NewRequest(http.MethodPost, "http://erpc.test/proj/evm/123",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))
	h.ServeHTTP(w, r)

	assert.Contains(t, w.Body(), "unexpected server panic on top-level handler")
	assert.NotContains(t, w.Body(), "invalid character",
		"the body must not carry a marshal complaint in place of the fault")
}
