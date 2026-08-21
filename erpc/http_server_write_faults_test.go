package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// The last thing eRPC does with a request is write it to a socket, and that is
// the one step ordinary tests never fail. These tests hand the handler a
// ResponseWriter that dies on command (see fault_transport_test.go) and check
// what eRPC does about it: the status it commits to, whether it keeps writing,
// and whether a panic on the way out still produces a JSON-RPC body.

// discardLogger keeps the fault tests quiet without hiding behaviour: nothing
// asserted below reads a log line.
func discardLogger() *zerolog.Logger {
	lg := zerolog.New(newFaultSink()).Level(zerolog.Disabled)
	return &lg
}

// fatalCall records one writeFatalError invocation.
type fatalCall struct {
	statusCode int
	err        error
}

// fatalRecorder stands in for the handler's writeFatalError callback. Counting
// the calls matters as much as reading them: eRPC must reach for it exactly
// when the normal body could not be written, and not otherwise.
type fatalRecorder struct {
	calls []fatalCall
}

func (f *fatalRecorder) fn(_ context.Context, statusCode int, err error) {
	f.calls = append(f.calls, fatalCall{statusCode: statusCode, err: err})
}

// TestHandleErrorResponse_MapsAFailureToTheTransportStatusAClientActsOn pins
// the status switch. A client library reads only this number to decide whether
// to retry, to back off, or to re-authenticate, so every arm is a contract.
//
// Note that this switch is NOT the same as each error's own ErrorStatusCode():
// the JSON-RPC transport keeps 200 for anything a client should read out of the
// body, and reserves non-200 for transport-level faults. The last two cases pin
// that deliberate divergence.
func TestHandleErrorResponse_MapsAFailureToTheTransportStatusAClientActsOn(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"invalid url path", common.NewErrInvalidUrlPath("no network", "/x"), http.StatusBadRequest},
		{"unparseable body", common.NewErrJsonRpcRequestUnmarshal(errors.New("eof"), []byte("{")), http.StatusBadRequest},
		{"invalid request", common.NewErrInvalidRequest(errors.New("bad params")), http.StatusBadRequest},
		{"auth unauthorized", common.NewErrAuthUnauthorized("secret", "bad token"), http.StatusUnauthorized},
		{"endpoint unauthorized", common.NewErrEndpointUnauthorized(errors.New("bad key")), http.StatusUnauthorized},
		{"project not found", common.NewErrProjectNotFound("p"), http.StatusNotFound},
		{"network not found", common.NewErrNetworkNotFound("evm:1"), http.StatusNotFound},
		{"network not supported", common.NewErrNetworkNotSupported("p", "evm:1"), http.StatusNotFound},
		{"auth budget", common.NewErrAuthRateLimitRuleExceeded("p", "secret", "b", "r", "u", "1.2.3.4"), http.StatusTooManyRequests},
		{"project budget", common.NewErrProjectRateLimitRuleExceeded("p", "b", "r"), http.StatusTooManyRequests},
		{"network budget", common.NewErrNetworkRateLimitRuleExceeded("p", "evm:1", "b", "r"), http.StatusTooManyRequests},
		{"endpoint capacity", common.NewErrEndpointCapacityExceeded(errors.New("429 from vendor")), http.StatusTooManyRequests},
		// Deliberately the last arm of the switch: a more specific verdict
		// already on the cause chain describes the failure better.
		{"no upstreams available", common.NewErrNetworkNoUpstreamsAvailable("p", "evm:1"), http.StatusNotFound},
		// Divergence from ErrorStatusCode(), on purpose: the client reads
		// these out of the JSON-RPC error body, not the status line.
		{"upstream timeout", common.NewErrRequestTimeout(5 * time.Second), http.StatusOK},
		{"billing issue", common.NewErrEndpointBillingIssue(errors.New("plan exhausted")), http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newFaultResponseWriter()
			fatal := &fatalRecorder{}
			startedAt := time.Now()

			handleErrorResponse(
				context.Background(), discardLogger(), &startedAt, nil, tc.err,
				w, common.SonicCfg.NewEncoder(w), fatal.fn, &common.TRUE,
				common.ExecutionHeadersAll,
			)

			require.Equal(t, tc.want, w.Status())
			require.Equal(t, 1, w.HeaderWrites(), "the status line must be committed exactly once")
			require.Empty(t, fatal.calls, "a body that encoded cleanly must not reach the fatal writer")

			var body struct {
				Jsonrpc string `json:"jsonrpc"`
				Error   struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(w.Bytes(), &body), "body was %q", w.Body())
			require.Equal(t, "2.0", body.Jsonrpc)
			require.NotZero(t, body.Error.Code, "a client with a 200 status has only this code to go on")
		})
	}
}

// TestHandleErrorResponse_HandsOverToTheFatalWriterWhenTheBodyCannotBeEncoded
// covers the half nobody sees: the client hangs up between the header and the
// body. eRPC must notice, must not keep encoding, and must report the transport
// error rather than the original failure.
func TestHandleErrorResponse_HandsOverToTheFatalWriterWhenTheBodyCannotBeEncoded(t *testing.T) {
	w := newFaultResponseWriter()
	w.HangUp()
	fatal := &fatalRecorder{}
	startedAt := time.Now()

	handleErrorResponse(
		context.Background(), discardLogger(), &startedAt, nil,
		common.NewErrProjectNotFound("gone"),
		w, common.SonicCfg.NewEncoder(w), fatal.fn, &common.TRUE,
		common.ExecutionHeadersAll,
	)

	require.Equal(t, http.StatusNotFound, w.Status(),
		"the status was already on the wire before the body failed")
	require.Len(t, fatal.calls, 1, "an unencodable body must reach the fatal writer exactly once")
	require.Equal(t, http.StatusInternalServerError, fatal.calls[0].statusCode)
	require.ErrorIs(t, fatal.calls[0].err, errPeerHungUp,
		"the fatal writer must be told what actually went wrong")
	require.Zero(t, w.WritesAfterFailure(),
		"the encoder kept writing %d times after the socket refused", w.WritesAfterFailure())
	require.Empty(t, w.Bytes())
}

// TestHandleErrorResponse_StillReportsWhenTheClientCancelled covers the same
// hand-over for a cancelled request, which eRPC logs quietly rather than as a
// server fault. The client is gone either way, so the fatal writer still runs.
func TestHandleErrorResponse_StillReportsWhenTheClientCancelled(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			w := newFaultResponseWriter()
			w.FailWith(cause).HangUp()
			fatal := &fatalRecorder{}
			startedAt := time.Now()

			handleErrorResponse(
				context.Background(), discardLogger(), &startedAt, nil,
				common.NewErrInvalidRequest(errors.New("bad params")),
				w, common.SonicCfg.NewEncoder(w), fatal.fn, &common.TRUE,
				common.ExecutionHeadersAll,
			)

			require.Len(t, fatal.calls, 1)
			require.ErrorIs(t, fatal.calls[0].err, cause)
			require.Zero(t, w.WritesAfterFailure())
		})
	}
}

// TestHandleErrorResponse_ObeysTheExecutionHeadersMode checks that the headers
// go out BEFORE the status line. Once WriteHeader fires the header map is
// sealed, so a header set afterwards is silently lost — an operator sees the
// diagnostic headers on success responses and nothing on failures.
func TestHandleErrorResponse_ObeysTheExecutionHeadersMode(t *testing.T) {
	nq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))
	nq.ExecState()

	run := func(mode common.ExecutionHeadersMode) *faultResponseWriter {
		w := newFaultResponseWriter()
		startedAt := time.Now()
		handleErrorResponse(
			context.Background(), discardLogger(), &startedAt, nq,
			common.NewErrInvalidRequest(errors.New("bad params")),
			w, common.SonicCfg.NewEncoder(w), (&fatalRecorder{}).fn, &common.TRUE,
			mode,
		)
		return w
	}

	off := run(common.ExecutionHeadersOff)
	for name := range off.SentHeader() {
		require.False(t, strings.HasPrefix(strings.ToUpper(name), "X-ERPC-"),
			"executionHeaders:off must suppress %s", name)
	}

	// SentHeader is the snapshot taken at WriteHeader. Reading it, rather than
	// the live map, is what makes this an ordering test: a header set after
	// WriteHeader never reaches the client.
	all := run(common.ExecutionHeadersAll)
	var erpcHeaders int
	for name := range all.SentHeader() {
		if strings.HasPrefix(strings.ToUpper(name), "X-ERPC-") {
			erpcHeaders++
		}
	}
	require.NotZero(t, erpcHeaders,
		"the diagnostic headers must be set before WriteHeader seals the map")
}

//
// --- Whole-handler write faults ---
//

// faultHandlerServer builds an HttpServer whose handler can be called directly.
// No listener, no client: the test owns the ResponseWriter, so it can break the
// socket at any point and read back exactly what eRPC committed.
func faultHandlerServer(t *testing.T) *HttpServer {
	t.Helper()
	return &HttpServer{
		logger:    discardLogger(),
		appCtx:    context.Background(),
		serverCfg: &common.ServerConfig{IncludeErrorDetails: &common.TRUE},
		draining:  &atomic.Bool{},
	}
}

// TestRequestHandler_TurnsAPanicIntoAJsonRpcErrorBody covers the top-level
// recovery. A panic that escaped would leave the client with an empty 200 and
// no error at all, which every JSON-RPC client reports as a parse failure
// rather than as the server fault it is.
func TestRequestHandler_TurnsAPanicIntoAJsonRpcErrorBody(t *testing.T) {
	// erpc is nil, so the project lookup panics inside the handler.
	s := faultHandlerServer(t)
	h := s.createRequestHandler()

	w := newFaultResponseWriter()
	r := httptest.NewRequest(http.MethodPost, "http://erpc.test/proj/evm/123",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))

	require.NotPanics(t, func() { h.ServeHTTP(w, r) },
		"a panic must not escape the handler and kill the whole server")

	require.Equal(t, http.StatusOK, w.Status(),
		"a JSON-RPC POST keeps transport 200; the fault belongs in the body")
	var body struct {
		Jsonrpc string `json:"jsonrpc"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Bytes(), &body), "body was %q", w.Body())
	require.Equal(t, "2.0", body.Jsonrpc)
	require.Equal(t, -32603, body.Error.Code)
	require.Contains(t, body.Error.Message, "panic")
}

// TestRequestHandler_SurvivesAResponseWriterThatPanicsWhileReportingAPanic
// covers the second recovery block. The fatal writer is the last thing standing
// between a broken request and a dead server process, so it has its own
// recover; without it, a client that aborts mid-write takes the handler with it.
func TestRequestHandler_SurvivesAResponseWriterThatPanicsWhileReportingAPanic(t *testing.T) {
	s := faultHandlerServer(t)
	h := s.createRequestHandler()

	w := newFaultResponseWriter()
	w.PanicOnWrite(http.ErrAbortHandler)
	r := httptest.NewRequest(http.MethodPost, "http://erpc.test/proj/evm/123",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))

	require.NotPanics(t, func() { h.ServeHTTP(w, r) },
		"the fatal error writer must absorb a panicking socket")
	require.Equal(t, http.StatusOK, w.Status())
	require.Empty(t, w.Bytes(), "nothing reached a socket that aborted")
}

// TestRequestHandler_ReportsABadUrlPathWithoutTouchingTheProject checks the
// earliest exit in the handler: parseUrlPath fails, so the client gets 400 and
// eRPC never looks a project up. With erpc nil, a lookup would panic — which is
// exactly what makes the assertion sharp.
func TestRequestHandler_ReportsABadUrlPathWithoutTouchingTheProject(t *testing.T) {
	s := faultHandlerServer(t)
	h := s.createRequestHandler()

	w := newFaultResponseWriter()
	r := httptest.NewRequest(http.MethodPost, "http://erpc.test/a/b/c/d/e",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))
	h.ServeHTTP(w, r)

	require.Equal(t, http.StatusBadRequest, w.Status())
	require.Contains(t, w.Body(), "must only provide")
}

// TestRequestHandler_ReportsABadUrlPathEvenWhenTheClientIsGone runs the same
// early exit against a socket that refuses everything. eRPC must fall through
// to the fatal writer and stop, not spin on the dead socket.
func TestRequestHandler_ReportsABadUrlPathEvenWhenTheClientIsGone(t *testing.T) {
	s := faultHandlerServer(t)
	h := s.createRequestHandler()

	w := newFaultResponseWriter()
	w.HangUp()
	r := httptest.NewRequest(http.MethodPost, "http://erpc.test/a/b/c/d/e",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"eth_chainId"}`))

	require.NotPanics(t, func() { h.ServeHTTP(w, r) })
	require.Empty(t, w.Bytes())
	require.LessOrEqual(t, w.WritesAfterFailure(), 1,
		"after the socket refused, only the fatal writer may try again; it tried %d times",
		w.WritesAfterFailure())
}

//
// --- The conditional gzip writer ---
//

// TestConditionalGzipWriter_SendsSmallBodiesUncompressedAndDefersTheStatus
// pins the deferral. The writer holds sub-threshold bytes back so it can still
// set Content-Encoding, which means the status code has to wait too — a status
// sent early would seal the header map before the decision is made.
func TestConditionalGzipWriter_SendsSmallBodiesUncompressedAndDefersTheStatus(t *testing.T) {
	inner := newFaultResponseWriter()
	w := &conditionalGzipWriter{ResponseWriter: inner}

	w.WriteHeader(http.StatusTeapot)
	require.Zero(t, inner.HeaderWrites(), "the status must not be committed while undecided")
	n, err := w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	require.NoError(t, err)
	require.Equal(t, 39, n, "an undecided write must still report the full length to its caller")
	require.Empty(t, inner.Bytes(), "the body is buffered, not sent")

	require.NoError(t, w.Close())
	require.Equal(t, http.StatusTeapot, inner.Status())
	require.Equal(t, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`, inner.Body())
	require.Empty(t, inner.SentHeader().Get("Content-Encoding"),
		"a sub-threshold body must not claim to be gzipped")
}

// TestConditionalGzipWriter_CommitsToPassthroughOnFlush covers the streaming
// case: a handler that flushes wants the bytes on the wire now, so the writer
// gives up on compressing them.
func TestConditionalGzipWriter_CommitsToPassthroughOnFlush(t *testing.T) {
	inner := newFaultResponseWriter()
	w := &conditionalGzipWriter{ResponseWriter: inner}

	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(`{"partial":true}`))
	require.NoError(t, err)
	w.Flush()

	require.Equal(t, http.StatusOK, inner.Status())
	require.Equal(t, `{"partial":true}`, inner.Body(), "a flush must put the buffered bytes on the wire")
	require.Equal(t, 1, inner.Flushes(), "the flush must reach the socket underneath")

	// After the decision the writer is a straight pass-through.
	_, err = w.Write([]byte(`{"more":true}`))
	require.NoError(t, err)
	require.Equal(t, `{"partial":true}{"more":true}`, inner.Body())

	w.WriteHeader(http.StatusInternalServerError)
	require.Equal(t, http.StatusOK, inner.Status(),
		"the first status wins; a later one is a superfluous WriteHeader")
	require.NoError(t, w.Close())
}

// TestConditionalGzipWriter_PutsTheBufferedBytesOutBeforeTheFlushReachesTheSocket
// uses the flush fault: the peer resets on the flush itself. The buffered bytes
// must already be on the wire by then — the writer commits to pass-through and
// drains BEFORE it forwards the flush — and the handler must learn about the
// dead peer on its next write.
func TestConditionalGzipWriter_PutsTheBufferedBytesOutBeforeTheFlushReachesTheSocket(t *testing.T) {
	inner := newFaultResponseWriter()
	inner.FailOnFlush(1)
	w := &conditionalGzipWriter{ResponseWriter: inner}

	_, err := w.Write([]byte(`{"partial":true}`))
	require.NoError(t, err)

	w.Flush()
	require.Equal(t, `{"partial":true}`, inner.Body(),
		"the drain must happen before the flush is forwarded, or the buffered bytes are lost")

	_, err = w.Write([]byte(`{"more":true}`))
	require.ErrorIs(t, err, errPeerHungUp,
		"the handler must hear about the dead peer on its next write, not at Close")
	// Exactly one: the write that discovered the flush-time failure. A second
	// would mean the writer kept streaming after the socket told it to stop.
	require.Equal(t, 1, inner.WritesAfterFailure())
	require.NoError(t, w.Close(), "an already-decided pass-through has nothing left to drain")
}

// TestConditionalGzipWriter_CompressesOnceTheBodyIsWorthIt covers the other
// branch of the decision, including the header rewrite. Content-Length must go:
// it describes the uncompressed body and would truncate the response.
func TestConditionalGzipWriter_CompressesOnceTheBodyIsWorthIt(t *testing.T) {
	inner := newFaultResponseWriter()
	inner.Header().Set("Content-Length", "999999")
	w := &conditionalGzipWriter{ResponseWriter: inner, pool: util.NewGzipWriterPool()}

	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(`{"head":true}`))
	require.NoError(t, err)
	_, err = w.Write([]byte(strings.Repeat("a", compressionThreshold)))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	// The snapshot, not the live map: Content-Encoding set after WriteHeader
	// never reaches the client, and the body would then be unreadable.
	require.Equal(t, "gzip", inner.SentHeader().Get("Content-Encoding"))
	require.Equal(t, "application/json", inner.SentHeader().Get("Content-Type"),
		"a compressed body with no declared type is unreadable to a JSON-RPC client")
	require.Empty(t, inner.SentHeader().Get("Content-Length"),
		"the stale uncompressed length would truncate the response")
	require.Equal(t, http.StatusOK, inner.Status())
	require.Less(t, len(inner.Bytes()), compressionThreshold,
		"a repeated byte must actually compress")
	require.Contains(t, string(gunzip(t, inner.Bytes())), `{"head":true}`,
		"the bytes buffered before the decision must still reach the client, in order")
}

// TestConditionalGzipWriter_ReportsASocketThatDiesWhileItDrainsItsBuffer
// covers the error return from decide(). The buffered bytes are written at
// Close, long after the handler stopped looking, so this is the only place the
// failure can surface.
func TestConditionalGzipWriter_ReportsASocketThatDiesWhileItDrainsItsBuffer(t *testing.T) {
	inner := newFaultResponseWriter()
	inner.HangUp()
	w := &conditionalGzipWriter{ResponseWriter: inner}

	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(`{"small":true}`))
	require.NoError(t, err, "an undecided write is buffered, so it cannot fail yet")

	require.ErrorIs(t, w.Close(), errPeerHungUp,
		"the drain failed and Close is the only caller that can hear about it")
	require.Zero(t, inner.WritesAfterFailure())
}

// TestConditionalGzipWriter_PassesADeadSocketStraightBackToTheHandler covers
// the decided pass-through path, where the handler is still writing and must
// learn immediately.
func TestConditionalGzipWriter_PassesADeadSocketStraightBackToTheHandler(t *testing.T) {
	inner := newFaultResponseWriter()
	w := &conditionalGzipWriter{ResponseWriter: inner}
	w.Flush() // commit to pass-through while the socket is still alive

	inner.HangUp()
	_, err := w.Write([]byte(`{"a":1}`))
	require.ErrorIs(t, err, errPeerHungUp)
	require.Zero(t, inner.WritesAfterFailure())
	require.NoError(t, w.Close(), "an already-decided pass-through has nothing left to drain")
}

// TestGzipHandler_StepsAsideForAWebSocketUpgrade pins the bypass. Wrapping the
// writer hides http.Hijacker, and gorilla asserts that interface directly, so
// the upgrade would answer 500 instead of 101.
func TestGzipHandler_StepsAsideForAWebSocketUpgrade(t *testing.T) {
	var got http.ResponseWriter
	h := gzipHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = w
	}))

	upgrade := httptest.NewRequest(http.MethodGet, "http://erpc.test/proj/evm/123", nil)
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	upgrade.Header.Set("Accept-Encoding", "gzip")
	w := newFaultResponseWriter()
	h.ServeHTTP(w, upgrade)

	_, wrapped := got.(*conditionalGzipWriter)
	require.False(t, wrapped, "an upgrade must reach the handler unwrapped or it cannot hijack")
	_, hijackable := got.(http.Hijacker)
	require.True(t, hijackable, "gorilla asserts http.Hijacker directly")
	require.Empty(t, w.Header().Get("Vary"), "an upgrade has no representation to vary")

	// A plain POST that accepts gzip is wrapped, and one that does not is not.
	plain := httptest.NewRequest(http.MethodPost, "http://erpc.test/proj/evm/123", strings.NewReader("{}"))
	plain.Header.Set("Accept-Encoding", "gzip")
	w2 := newFaultResponseWriter()
	h.ServeHTTP(w2, plain)
	_, wrapped = got.(*conditionalGzipWriter)
	require.True(t, wrapped)
	require.Equal(t, "Accept-Encoding", w2.Header().Get("Vary"),
		"a cache that ignores this serves a gzipped body to a client that cannot read it")

	noGzip := httptest.NewRequest(http.MethodPost, "http://erpc.test/proj/evm/123", strings.NewReader("{}"))
	w3 := newFaultResponseWriter()
	h.ServeHTTP(w3, noGzip)
	_, wrapped = got.(*conditionalGzipWriter)
	require.False(t, wrapped, "a client that did not ask for gzip must not be wrapped")
	require.Equal(t, "Accept-Encoding", w3.Header().Get("Vary"))
}
