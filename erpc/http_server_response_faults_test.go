package erpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// These tests drive the real request handler — routing, forwarding, and the
// final write — with a ResponseWriter the test can break at any byte. No
// listener and no client are involved, so the failure lands exactly where the
// test asks for it instead of whenever the kernel happens to drop a socket.

// faultResponseHandler builds a working project with one mocked upstream and
// returns its handler. Call it, then hand the handler a faultResponseWriter.
func faultResponseHandler(t *testing.T) http.Handler {
	t.Helper()
	return faultResponseHandlerWithLogger(t, log.Logger)
}

// faultResponseHandlerWithLogger is faultResponseHandler with the server's
// logger supplied, so a test can read what the handler reported.
func faultResponseHandlerWithLogger(t *testing.T, logger zerolog.Logger) http.Handler {
	t.Helper()

	util.ResetGock()
	t.Cleanup(util.ResetGock)
	gock.New("http://rpc-fault.localhost").
		Post("/").
		Persist().
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x7b"})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{
			{
				Id: "faulty",
				Networks: []*common.NetworkConfig{
					{
						Architecture: common.ArchitectureEvm,
						Evm:          &common.EvmNetworkConfig{ChainId: 123},
					},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc-fault.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}
	require.NoError(t, cfg.SetDefaults(nil))

	ssr, err := data.NewSharedStateRegistry(ctx, &logger, &common.SharedStateConfig{
		Connector: &common.ConnectorConfig{
			Driver: "memory",
			Memory: &common.MemoryConnectorConfig{MaxItems: 100_000, MaxTotalSize: "1GB"},
		},
	})
	require.NoError(t, err)

	erpcInstance, err := NewERPC(ctx, &logger, ssr, nil, nil, cfg)
	require.NoError(t, err)
	erpcInstance.Bootstrap(ctx)

	srv, err := NewHttpServer(ctx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, cfg.Indexer, erpcInstance)
	require.NoError(t, err)
	return srv.createRequestHandler()
}

func faultRequest(body string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "http://erpc.test/faulty/evm/123", strings.NewReader(body))
}

const singleCall = `{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`
const batchCall = `[{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]},` +
	`{"jsonrpc":"2.0","id":2,"method":"eth_chainId","params":[]}]`

// TestRequestHandler_DeliversTheAnswerWhenTheSocketHolds is the control. Every
// fault test below asserts that something did NOT happen, and that assertion is
// worthless unless the undamaged path really does happen.
func TestRequestHandler_DeliversTheAnswerWhenTheSocketHolds(t *testing.T) {
	h := faultResponseHandler(t)

	t.Run("single", func(t *testing.T) {
		w := newFaultResponseWriter()
		h.ServeHTTP(w, faultRequest(singleCall))

		require.Equal(t, http.StatusOK, w.Status())
		require.Equal(t, 1, w.HeaderWrites())
		var got struct {
			Id     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		require.NoError(t, json.Unmarshal(w.Bytes(), &got), "body was %q", w.Body())
		require.Equal(t, 1, got.Id)
		require.JSONEq(t, `"0x7b"`, string(got.Result))
	})

	t.Run("batch", func(t *testing.T) {
		w := newFaultResponseWriter()
		h.ServeHTTP(w, faultRequest(batchCall))

		require.Equal(t, http.StatusOK, w.Status())
		var entries []struct {
			Id     int             `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		require.NoError(t, json.Unmarshal(w.Bytes(), &entries), "body was %q", w.Body())
		require.Len(t, entries, 2, "one answer per call, or every later answer is mispaired")
		// Order is the contract a batch client relies on: it pairs answers to
		// calls by position first and by id second, so a reordered array hands
		// each call another call's result without any client-visible error.
		require.Equal(t, 1, entries[0].Id)
		require.Equal(t, 2, entries[1].Id)
		require.JSONEq(t, `"0x7b"`, string(entries[0].Result))
	})
}

// TestRequestHandler_StopsWritingAnAnswerWhenTheClientHangsUp covers the write
// failure eRPC can do nothing about. The one thing it must not do is keep
// serialising into a socket that has already refused: on a large batch that
// costs the full encode for a client that is gone.
//
// One write after the refusal is expected and correct — that is writeFatalError
// making its single attempt. Anything more is the response body still streaming.
func TestRequestHandler_StopsWritingAnAnswerWhenTheClientHangsUp(t *testing.T) {
	h := faultResponseHandler(t)

	for _, tc := range []struct{ name, body string }{
		{"single", singleCall},
		{"batch", batchCall},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFaultResponseWriter()
			w.HangUp()

			require.NotPanics(t, func() { h.ServeHTTP(w, faultRequest(tc.body)) })

			require.Equal(t, http.StatusOK, w.Status(),
				"the status went out before the body failed, and a JSON-RPC POST stays 200")
			require.Empty(t, w.Bytes(), "a refused socket holds none of the answer")
			require.LessOrEqual(t, w.WritesAfterFailure(), 1,
				"after the socket refused, only writeFatalError may try again; eRPC wrote %d more times",
				w.WritesAfterFailure())
		})
	}
}

// TestRequestHandler_TruncatesRatherThanReordersWhenTheSocketDiesMidBody walks
// the hang-up across the body, one byte offset at a time. Whatever the client
// received must be the opening of the real answer, byte for byte: a client that
// reads a prefix of the wrong body cannot tell, because a truncated JSON
// document fails to parse either way.
func TestRequestHandler_TruncatesRatherThanReordersWhenTheSocketDiesMidBody(t *testing.T) {
	h := faultResponseHandler(t)

	whole := newFaultResponseWriter()
	h.ServeHTTP(whole, faultRequest(batchCall))
	full := whole.Bytes()
	require.NotEmpty(t, full)

	for limit := 0; limit < len(full); limit++ {
		w := newFaultResponseWriter()
		w.FailAfterBytes(limit)
		require.NotPanics(t, func() { h.ServeHTTP(w, faultRequest(batchCall)) })

		got := w.Bytes()
		require.LessOrEqual(t, len(got), limit,
			"the socket accepted %d bytes but was only offered %d", len(got), limit)
		require.Equal(t, string(full[:len(got)]), string(got),
			"cut at %d bytes, the client received a different body, not a shorter one", limit)
	}
}

// TestRequestHandler_WritesNothingWhenTheRequestContextIsAlreadyDone covers the
// exit after the forwarding work completes. A client that disconnected while
// eRPC was waiting on an upstream leaves a response nobody will read; eRPC must
// release it and write nothing, because writing to a closed request is what
// net/http reports as a superfluous write.
func TestRequestHandler_WritesNothingWhenTheRequestContextIsAlreadyDone(t *testing.T) {
	h := faultResponseHandler(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := newFaultResponseWriter()
	require.NotPanics(t, func() { h.ServeHTTP(w, faultRequest(singleCall).WithContext(ctx)) })

	require.Zero(t, w.Writes(), "eRPC wrote %d times to a request that is already over", w.Writes())
	require.Zero(t, w.HeaderWrites(), "committing a status here would be a superfluous write")
	require.Empty(t, w.Bytes())
}

// lockedBuffer collects log output from the handler goroutine and the response
// releasers it spawns.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRequestHandler_ReportsWhyTheRequestContextEnded is the other half of the
// test above. eRPC writes nothing, which is correct, so the only thing an
// operator debugging "the client received nothing" can find is the log line.
// httpCtx.Err() only ever says "canceled" or "deadline exceeded"; the reason
// lives in the cause the canceller attached, so the handler has to read it.
func TestRequestHandler_ReportsWhyTheRequestContextEnded(t *testing.T) {
	// The test harness disables logging globally; this test reads a log line.
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })

	var out lockedBuffer
	h := faultResponseHandlerWithLogger(t, zerolog.New(&out).Level(zerolog.DebugLevel))

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("upstream pool drained mid-flight"))

	w := newFaultResponseWriter()
	require.NotPanics(t, func() { h.ServeHTTP(w, faultRequest(singleCall).WithContext(ctx)) })

	// Discriminating: "context canceled" alone is what Err() already said and
	// tells the operator nothing. Only the cause names what happened.
	require.Contains(t, out.String(), "upstream pool drained mid-flight",
		"the cancellation cause the caller attached never reached the operator")
	require.Zero(t, w.Writes(), "reporting the cause must not start writing a body")
	require.Zero(t, w.HeaderWrites())
}
