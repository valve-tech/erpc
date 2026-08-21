package common

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubResultWriter stands in for the streaming result writers the upstream
// layer attaches when it does not want to materialize a body in memory. The
// response object must treat it as the source of truth for size, emptiness and
// wire bytes whenever `result` is still nil.
type stubResultWriter struct {
	payload  []byte
	emptyish bool
	released bool
	writes   int
	failAt   int // when > 0, WriteTo returns an error after writing failAt bytes
}

func (s *stubResultWriter) WriteTo(w io.Writer, trimSides bool) (int64, error) {
	s.writes++
	out := s.payload
	if trimSides {
		if len(out) <= 2 {
			return 0, nil
		}
		out = out[1 : len(out)-1]
	}
	if s.failAt > 0 {
		n, _ := w.Write(out[:s.failAt])
		return int64(n), errors.New("writer blew up mid-stream")
	}
	n, err := w.Write(out)
	return int64(n), err
}

func (s *stubResultWriter) IsResultEmptyish() bool { return s.emptyish }

func (s *stubResultWriter) Size(ctx ...context.Context) (int, error) { return len(s.payload), nil }

func (s *stubResultWriter) Release() { s.released = true }

// failingWriter fails once the caller has pushed more than `budget` bytes
// through it, mimicking a client that hangs up part-way through a response.
type failingWriter struct {
	budget  int
	written int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.written+len(p) > f.budget {
		allowed := f.budget - f.written
		if allowed < 0 {
			allowed = 0
		}
		f.written += allowed
		return allowed, errors.New("client hung up")
	}
	f.written += len(p)
	return len(p), nil
}

// TestJsonRpcResponse_WriteTo_EmitsTheResultEnvelope pins the exact bytes a
// client receives for a plain successful response. Any stray comma or missing
// field here is a protocol violation for every single request erpc serves.
func TestJsonRpcResponse_WriteTo_EmitsTheResultEnvelope(t *testing.T) {
	t.Parallel()

	jr, err := NewJsonRpcResponseFromBytes([]byte(`7`), []byte(`"0x1a"`), nil)
	require.NoError(t, err)

	var buf bytes.Buffer
	n, err := jr.WriteTo(&buf)
	require.NoError(t, err)

	require.Equal(t, `{"jsonrpc":"2.0","id":7,"result":"0x1a"}`, buf.String())
	require.Equal(t, int64(buf.Len()), n, "the reported count must equal the bytes actually written")
}

// TestJsonRpcResponse_WriteTo_MarshalsATypedErrorOnDemand covers the path
// internal eRPC errors take: nothing sets errBytes, only the typed Error field.
// If WriteTo skipped the lazy marshal the client would get a 200 with neither
// result nor error — a silent success that hides a real failure.
func TestJsonRpcResponse_WriteTo_MarshalsATypedErrorOnDemand(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{
		idBytes: []byte(`"abc"`),
		Error:   NewErrJsonRpcExceptionExternal(-32005, "limit exceeded", "retry later"),
	}

	var buf bytes.Buffer
	n, err := jr.WriteTo(&buf)
	require.NoError(t, err)

	out := buf.String()
	require.Equal(t, int64(len(out)), n)
	require.Contains(t, out, `"id":"abc"`)
	require.Contains(t, out, `"error":`)
	require.Contains(t, out, `-32005`, "the numeric code is what a client switches on")
	require.Contains(t, out, `limit exceeded`)
	require.NotContains(t, out, `"result"`, "an error response must not also carry a result")

	// The lazy marshal must NOT cache into the shared errBytes field. WriteTo
	// holds read locks only and many clients render one response at once, so a
	// write here is a data race. A re-send re-marshals and emits the same bytes.
	require.Empty(t, jr.errBytes, "WriteTo must not mutate shared state under a read lock")

	var again bytes.Buffer
	_, err = jr.WriteTo(&again)
	require.NoError(t, err)
	require.Equal(t, out, again.String(), "a re-send must emit identical bytes")
}

// TestJsonRpcResponse_WriteTo_PrefersTheUpstreamErrorBytes proves the upstream's
// own error object reaches the client verbatim, including vendor-specific extra
// members. Re-marshalling from the typed struct would drop them.
func TestJsonRpcResponse_WriteTo_PrefersTheUpstreamErrorBytes(t *testing.T) {
	t.Parallel()

	raw := `{"code":-32000,"message":"header not found","vendorHint":"try archive node"}`
	jr, err := NewJsonRpcResponseFromBytes([]byte(`1`), nil, []byte(raw))
	require.NoError(t, err)

	var buf bytes.Buffer
	_, err = jr.WriteTo(&buf)
	require.NoError(t, err)

	require.Equal(t, `{"jsonrpc":"2.0","id":1,"error":`+raw+`}`, buf.String(),
		"the upstream error object must survive byte-for-byte")
}

// TestJsonRpcResponse_WriteTo_ReportsOnlyTheBytesItActuallyWrote checks the
// io.WriterTo contract on a client disconnect. An inflated count makes the HTTP
// layer believe it sent a complete body and skip the error path.
func TestJsonRpcResponse_WriteTo_ReportsOnlyTheBytesItActuallyWrote(t *testing.T) {
	t.Parallel()

	const prefix = `{"jsonrpc":"2.0","id":`

	t.Run("fails on the very first write", func(t *testing.T) {
		jr, err := NewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0x1"`), nil)
		require.NoError(t, err)

		w := &failingWriter{budget: 0}
		n, err := jr.WriteTo(w)
		require.Error(t, err)
		require.Equal(t, int64(0), n)
	})

	t.Run("fails while writing the id", func(t *testing.T) {
		jr, err := NewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0x1"`), nil)
		require.NoError(t, err)

		w := &failingWriter{budget: len(prefix)}
		n, err := jr.WriteTo(w)
		require.Error(t, err)
		require.Equal(t, int64(len(prefix)), n,
			"the envelope prefix landed, the id did not")
	})

	t.Run("fails while writing the result", func(t *testing.T) {
		jr, err := NewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0x1"`), nil)
		require.NoError(t, err)

		budget := len(prefix) + len(`1`) + len(`,"result":`)
		w := &failingWriter{budget: budget}
		n, err := jr.WriteTo(w)
		require.Error(t, err)
		require.Equal(t, int64(budget), n)
	})
}

// TestJsonRpcResponse_WriteTo_ServesConcurrentClientsWithoutRacing covers the
// multiplexing case: one response object, many waiting clients, each rendering
// it at the same time. The response carries a typed Error and no errBytes, so
// every goroutine takes the lazy-marshal branch. WriteTo holds only READ locks
// there, so it must not write any shared field, and every client must receive
// the same bytes. Run with -race.
func TestJsonRpcResponse_WriteTo_ServesConcurrentClientsWithoutRacing(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{
		idBytes: []byte(`"abc"`),
		Error:   NewErrJsonRpcExceptionExternal(-32005, "limit exceeded", "retry later"),
	}

	const clients = 8
	start := make(chan struct{})
	outs := make([]string, clients)
	var wg sync.WaitGroup
	wg.Add(clients)
	for i := 0; i < clients; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			var buf bytes.Buffer
			n, err := jr.WriteTo(&buf)
			assert.NoError(t, err)
			assert.Equal(t, int64(buf.Len()), n)
			outs[i] = buf.String()
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 1; i < clients; i++ {
		require.Equal(t, outs[0], outs[i],
			"every multiplexed client must receive identical bytes")
	}
	require.Contains(t, outs[0], `-32005`)
}

// TestJsonRpcResponse_WriteTo_StreamsFromTheResultWriter covers the streaming
// branch: no materialized result, only a writer. This is the memory-saving path
// for large getLogs bodies, so a break here shows up as an empty result field.
func TestJsonRpcResponse_WriteTo_StreamsFromTheResultWriter(t *testing.T) {
	t.Parallel()

	rw := &stubResultWriter{payload: []byte(`[1,2,3]`)}
	jr := &JsonRpcResponse{idBytes: []byte(`9`)}
	jr.SetResultWriter(rw)

	var buf bytes.Buffer
	n, err := jr.WriteTo(&buf)
	require.NoError(t, err)

	require.Equal(t, `{"jsonrpc":"2.0","id":9,"result":[1,2,3]}`, buf.String())
	require.Equal(t, int64(buf.Len()), n)
	require.Equal(t, 1, rw.writes, "the writer must be consumed exactly once")
}

// TestJsonRpcResponse_WriteTo_PropagatesAStreamingFailure makes sure a failure
// inside the result writer is not swallowed and the byte count stays honest.
func TestJsonRpcResponse_WriteTo_PropagatesAStreamingFailure(t *testing.T) {
	t.Parallel()

	rw := &stubResultWriter{payload: []byte(`[1,2,3]`), failAt: 3}
	jr := &JsonRpcResponse{idBytes: []byte(`9`)}
	jr.SetResultWriter(rw)

	var buf bytes.Buffer
	n, err := jr.WriteTo(&buf)
	require.Error(t, err)
	require.Equal(t, int64(len(`{"jsonrpc":"2.0","id":9,"result":`)+3), n)
	require.False(t, strings.HasSuffix(buf.String(), "}"),
		"a truncated stream must not look like a closed envelope")
}

// TestJsonRpcResponse_WriteResultTo_TrimsExactlyOneByteEachSide covers the
// splice used to build batch and multiplexed bodies. Trimming the wrong amount
// produces JSON that no client can parse.
func TestJsonRpcResponse_WriteResultTo_TrimsExactlyOneByteEachSide(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		result    string
		trimSides bool
		want      string
	}{
		{"array untrimmed", `[1,2,3]`, false, `[1,2,3]`},
		{"array trimmed", `[1,2,3]`, true, `1,2,3`},
		{"object trimmed", `{"a":1}`, true, `"a":1`},
		// A single-element array is the common batch-splice shape; the
		// short-body guard must not swallow it.
		{"three-byte body trimmed keeps its one element", `[1]`, true, `1`},
		{"two-byte body trimmed yields nothing", `[]`, true, ``},
		{"one-byte body trimmed yields nothing", `1`, true, ``},
		{"two-byte body untrimmed is kept", `[]`, false, `[]`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jr := &JsonRpcResponse{result: []byte(tc.result)}
			var buf bytes.Buffer
			n, err := jr.WriteResultTo(&buf, tc.trimSides)
			require.NoError(t, err)
			require.Equal(t, tc.want, buf.String())
			require.Equal(t, int64(len(tc.want)), n)
		})
	}
}

// TestJsonRpcResponse_WriteResultTo_FallsBackToTheWriterAndToNothing pins the
// two non-materialized branches: delegate to the writer, or write nothing at
// all when neither source exists.
func TestJsonRpcResponse_WriteResultTo_FallsBackToTheWriterAndToNothing(t *testing.T) {
	t.Parallel()

	t.Run("delegates to the result writer", func(t *testing.T) {
		rw := &stubResultWriter{payload: []byte(`[7,8]`)}
		jr := &JsonRpcResponse{}
		jr.SetResultWriter(rw)

		var buf bytes.Buffer
		n, err := jr.WriteResultTo(&buf, true)
		require.NoError(t, err)
		require.Equal(t, `7,8`, buf.String())
		require.Equal(t, int64(3), n)
	})

	t.Run("empty response writes nothing", func(t *testing.T) {
		jr := &JsonRpcResponse{}
		var buf bytes.Buffer
		n, err := jr.WriteResultTo(&buf, false)
		require.NoError(t, err)
		require.Equal(t, int64(0), n)
		require.Equal(t, 0, buf.Len())
	})
}

// TestJsonRpcResponse_SetIDBytes_KeepsWireFidelityForLargeIds is the reason
// idBytes exists at all. A client that uses a nanosecond timestamp as its
// JSON-RPC id must get that exact id back; a float64 round-trip silently
// corrupts the last digits and the client can never match the response.
func TestJsonRpcResponse_SetIDBytes_KeepsWireFidelityForLargeIds(t *testing.T) {
	t.Parallel()

	const bigID = "1755123456789012345"

	jr := &JsonRpcResponse{}
	require.NoError(t, jr.SetIDBytes([]byte(bigID)))

	var buf bytes.Buffer
	_, err := jr.WriteTo(&buf)
	require.NoError(t, err)
	require.Contains(t, buf.String(), `"id":`+bigID,
		"the id must reach the client digit-for-digit")
}

// TestJsonRpcResponse_SetIDBytes_ParsesTheTypedView checks the typed id that
// internal code (batch demultiplexing) reads alongside the verbatim bytes.
func TestJsonRpcResponse_SetIDBytes_ParsesTheTypedView(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		bytes string
		want  interface{}
	}{
		{"integer", `42`, int64(42)},
		{"string", `"req-1"`, "req-1"},
		{"null stays nil", `null`, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jr := &JsonRpcResponse{}
			require.NoError(t, jr.SetIDBytes([]byte(tc.bytes)))
			require.Equal(t, tc.want, jr.ID())
		})
	}
}

// TestJsonRpcResponse_SetIDBytes_RejectsAnUnsupportedIdType keeps a malformed
// batch response from being demultiplexed against the wrong request.
func TestJsonRpcResponse_SetIDBytes_RejectsAnUnsupportedIdType(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{}
	err := jr.SetIDBytes([]byte(`{"nested":true}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported ID type",
		"the caller must be told the id shape is wrong, not something generic")
}

// TestJsonRpcResponse_SetID_ReplacesTheWireBytes proves the typed setter wins.
// erpc rewrites the response id to the client's id before sending; a stale
// idBytes would echo the upstream's id instead.
func TestJsonRpcResponse_SetID_ReplacesTheWireBytes(t *testing.T) {
	t.Parallel()

	jr, err := NewJsonRpcResponseFromBytes([]byte(`999`), []byte(`"0x0"`), nil)
	require.NoError(t, err)
	require.NoError(t, jr.SetID("client-7"))

	var buf bytes.Buffer
	_, err = jr.WriteTo(&buf)
	require.NoError(t, err)
	require.Contains(t, buf.String(), `"id":"client-7"`)
	require.NotContains(t, buf.String(), `999`)
}

// TestJsonRpcResponse_SizeAndResultLength_ReadTheWriter matters for the cache
// size guard and for metrics: a streaming response must not report zero bytes.
func TestJsonRpcResponse_SizeAndResultLength_ReadTheWriter(t *testing.T) {
	t.Parallel()

	t.Run("materialized result wins", func(t *testing.T) {
		jr := &JsonRpcResponse{result: []byte(`"0xabc"`)}
		jr.SetResultWriter(&stubResultWriter{payload: bytes.Repeat([]byte("x"), 100)})
		sz, err := jr.Size()
		require.NoError(t, err)
		require.Equal(t, 7, sz)
		require.Equal(t, 7, jr.ResultLength())
	})

	t.Run("writer supplies the size when nothing is materialized", func(t *testing.T) {
		jr := &JsonRpcResponse{}
		jr.SetResultWriter(&stubResultWriter{payload: bytes.Repeat([]byte("x"), 100)})
		sz, err := jr.Size()
		require.NoError(t, err)
		require.Equal(t, 100, sz)
		require.Equal(t, 100, jr.ResultLength())
	})

	t.Run("a bare response is zero", func(t *testing.T) {
		jr := &JsonRpcResponse{}
		sz, err := jr.Size()
		require.NoError(t, err)
		require.Equal(t, 0, sz)
		require.Equal(t, 0, jr.ResultLength())
	})

	t.Run("a nil response is zero", func(t *testing.T) {
		var jr *JsonRpcResponse
		sz, err := jr.Size()
		require.NoError(t, err)
		require.Equal(t, 0, sz)
	})
}

// TestJsonRpcResponse_IsResultEmptyish_ConsultsEverySource drives the
// empty-result retry decision. Getting this wrong either burns retries on a
// legitimately empty result or serves a hole in the chain data as a real answer.
func TestJsonRpcResponse_IsResultEmptyish_ConsultsEverySource(t *testing.T) {
	t.Parallel()

	t.Run("nil response is emptyish", func(t *testing.T) {
		var jr *JsonRpcResponse
		require.True(t, jr.IsResultEmptyish())
	})

	t.Run("materialized empty array", func(t *testing.T) {
		jr := &JsonRpcResponse{result: []byte(`[]`)}
		require.True(t, jr.IsResultEmptyish())
	})

	t.Run("materialized real value", func(t *testing.T) {
		jr := &JsonRpcResponse{result: []byte(`[{"a":1}]`)}
		require.False(t, jr.IsResultEmptyish())
	})

	t.Run("writer says not empty", func(t *testing.T) {
		jr := &JsonRpcResponse{}
		jr.SetResultWriter(&stubResultWriter{payload: []byte(`[1]`), emptyish: false})
		require.False(t, jr.IsResultEmptyish())
	})

	t.Run("writer says empty", func(t *testing.T) {
		jr := &JsonRpcResponse{}
		jr.SetResultWriter(&stubResultWriter{payload: []byte(`[]`), emptyish: true})
		require.True(t, jr.IsResultEmptyish())
	})

	t.Run("no result and no writer is emptyish", func(t *testing.T) {
		require.True(t, (&JsonRpcResponse{}).IsResultEmptyish())
	})
}

// TestJsonRpcResponse_PeekByPath_MaterializesFromTheWriter covers how the EVM
// hooks read blockNumber out of a streaming response. If the materialization
// step were skipped the hooks would read an empty document and treat every
// streamed block as unknown.
func TestJsonRpcResponse_PeekByPath_MaterializesFromTheWriter(t *testing.T) {
	t.Parallel()

	rw := &stubResultWriter{payload: []byte(`{"number":"0x10","hash":"0xdead"}`)}
	jr := &JsonRpcResponse{}
	jr.SetResultWriter(rw)

	got, err := jr.PeekStringByPath(context.Background(), "number")
	require.NoError(t, err)
	require.Equal(t, "0x10", got)

	raw, err := jr.PeekBytesByPath(context.Background(), "hash")
	require.NoError(t, err)
	require.Equal(t, `"0xdead"`, string(raw))

	require.Equal(t, 1, rw.writes, "the writer must be drained once, then cached")
	require.Nil(t, jr.resultWriter, "the writer is dropped after materialization")
}

// TestJsonRpcResponse_PeekByPath_ReportsAMissingPath makes sure a caller can
// tell "the field is absent" apart from "the field is empty".
func TestJsonRpcResponse_PeekByPath_ReportsAMissingPath(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{result: []byte(`{"number":"0x10"}`)}

	_, err := jr.PeekStringByPath(context.Background(), "missing")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing")

	_, err = jr.PeekBytesByPath(context.Background(), "missing")
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing")
}

// TestJsonRpcResponse_Clone_MaterializesTheWriterInsteadOfSharingIt guards the
// data race the comment in Clone warns about: two responses holding one writer
// would each drain it and one of them would end up with an empty body.
func TestJsonRpcResponse_Clone_MaterializesTheWriterInsteadOfSharingIt(t *testing.T) {
	t.Parallel()

	rw := &stubResultWriter{payload: []byte(`{"v":1}`)}
	src := &JsonRpcResponse{idBytes: []byte(`3`)}
	src.SetResultWriter(rw)

	clone, err := src.Clone()
	require.NoError(t, err)
	require.Nil(t, clone.resultWriter, "the clone must not share the writer")
	require.Equal(t, `{"v":1}`, string(clone.GetResultBytes()))

	var buf bytes.Buffer
	_, err = clone.WriteTo(&buf)
	require.NoError(t, err)
	require.Equal(t, `{"jsonrpc":"2.0","id":3,"result":{"v":1}}`, buf.String())
}

// TestJsonRpcResponse_Clone_DoesNotShareByteSlices proves the copy is deep. A
// shared backing array lets a mutation of one response corrupt the other, which
// on the cache path means serving one network's body to another.
func TestJsonRpcResponse_Clone_DoesNotShareByteSlices(t *testing.T) {
	t.Parallel()

	src, err := NewJsonRpcResponseFromBytes([]byte(`5`), []byte(`"0xaa"`), []byte(`{"code":-1,"message":"x"}`))
	require.NoError(t, err)

	clone, err := src.Clone()
	require.NoError(t, err)

	src.GetResultBytes()[1] = 'Z'
	src.idBytes[0] = '9'
	src.errBytes[1] = 'Z'

	require.Equal(t, `"0xaa"`, string(clone.GetResultBytes()))
	require.Equal(t, `5`, string(clone.idBytes))
	require.Equal(t, `{"code":-1,"message":"x"}`, string(clone.errBytes))
}

// TestJsonRpcResponse_Clone_CarriesTheCachedCanonicalHash keeps the consensus
// comparison cheap. Losing the hash is only a slowdown, but a WRONG carried
// hash would make two different bodies compare equal, so assert the value.
func TestJsonRpcResponse_Clone_CarriesTheCachedCanonicalHash(t *testing.T) {
	t.Parallel()

	src := &JsonRpcResponse{result: []byte(`{"a":"0x1"}`)}
	want, err := src.CanonicalHash()
	require.NoError(t, err)
	require.NotEmpty(t, want)

	clone, err := src.Clone()
	require.NoError(t, err)

	cached, ok := clone.canonicalHashWithIgnored.Load(defaultCanonicalHashPlaceholder)
	require.True(t, ok, "the clone must inherit the memoized hash")
	require.Equal(t, want, cached.(string))
}

// TestJsonRpcResponse_Clone_OfNilIsNil keeps the degenerate path safe; the
// consensus executor clones whatever it holds, including nothing.
func TestJsonRpcResponse_Clone_OfNilIsNil(t *testing.T) {
	t.Parallel()

	var jr *JsonRpcResponse
	clone, err := jr.Clone()
	require.NoError(t, err)
	require.Nil(t, clone)
}

// TestJsonRpcResponse_Free_ReleasesTheWriterAndDropsTheBody is the memory
// safety net: without the Release call a pooled upstream buffer is never
// returned and the process leaks one buffer per large response.
func TestJsonRpcResponse_Free_ReleasesTheWriterAndDropsTheBody(t *testing.T) {
	t.Parallel()

	rw := &stubResultWriter{payload: []byte(`{"v":1}`)}
	jr, err := NewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"0x1"`), []byte(`{"code":-1,"message":"x"}`))
	require.NoError(t, err)
	jr.SetResultWriter(rw)
	_, err = jr.CanonicalHash()
	require.NoError(t, err)

	jr.Free()

	require.True(t, rw.released, "a releasable writer must be handed back")
	require.Nil(t, jr.GetResultBytes())
	require.Nil(t, jr.idBytes)
	require.Nil(t, jr.errBytes)
	require.Nil(t, jr.resultWriter)

	_, ok := jr.canonicalHashWithIgnored.Load(defaultCanonicalHashPlaceholder)
	require.False(t, ok, "a freed response must not keep a hash for a body it no longer has")
}

// TestJsonRpcResponse_Free_OfNilIsSafe — Free runs on the release path where the
// response may already be gone.
func TestJsonRpcResponse_Free_OfNilIsSafe(t *testing.T) {
	t.Parallel()

	var jr *JsonRpcResponse
	require.NotPanics(t, func() { jr.Free() })
}

// TestJsonRpcResponse_ParseError_ClassifiesEveryUpstreamErrorShape walks the
// fallback ladder. Vendors do not agree on an error envelope, so each shape must
// still yield a code and a message an operator can act on. Every case asserts
// BOTH the code and the message, because "an error came back" is true on every
// branch and would not tell the branches apart.
func TestJsonRpcResponse_ParseError_ClassifiesEveryUpstreamErrorShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		raw         string
		wantCode    int
		wantMessage string
		wantData    interface{}
	}{
		{
			name:        "standard json-rpc error object",
			raw:         `{"code":-32000,"message":"header not found"}`,
			wantCode:    -32000,
			wantMessage: "header not found",
		},
		{
			name:        "code only, no message",
			raw:         `{"code":-32601}`,
			wantCode:    -32601,
			wantMessage: "",
		},
		{
			name:        "message only, no code",
			raw:         `{"message":"rate limited"}`,
			wantCode:    0,
			wantMessage: "rate limited",
		},
		{
			name:        "data-only object falls to the string-data special case",
			raw:         `{"data":"execution reverted"}`,
			wantCode:    0,
			wantMessage: "",
			wantData:    "execution reverted",
		},
		{
			name:        "nested error string member",
			raw:         `{"error":"upstream is draining"}`,
			wantCode:    int(JsonRpcErrorServerSideException),
			wantMessage: "upstream is draining",
		},
		{
			name:        "plain text body becomes the message",
			raw:         `Service Unavailable`,
			wantCode:    int(JsonRpcErrorServerSideException),
			wantMessage: "Service Unavailable",
		},
		{
			name:        "empty body is reported as an empty upstream response",
			raw:         ``,
			wantCode:    int(JsonRpcErrorServerSideException),
			wantMessage: "unexpected empty response from upstream endpoint",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jr := &JsonRpcResponse{}
			require.NoError(t, jr.ParseError(tc.raw))
			require.NotNil(t, jr.Error)
			require.Equal(t, tc.wantCode, jr.Error.Code, "the numeric code drives the client's retry decision")
			require.Equal(t, tc.wantMessage, jr.Error.Message)
			if tc.wantData != nil {
				require.Equal(t, tc.wantData, jr.Error.Data)
			}
		})
	}
}

// TestJsonRpcResponse_ParseError_KeepsRawBytesOnlyForWellFormedErrors matters
// for the wire: errBytes is written verbatim, so it must be valid JSON. A
// synthesized error must leave errBytes empty so WriteTo re-marshals instead.
func TestJsonRpcResponse_ParseError_KeepsRawBytesOnlyForWellFormedErrors(t *testing.T) {
	t.Parallel()

	wellFormed := &JsonRpcResponse{idBytes: []byte(`1`)}
	require.NoError(t, wellFormed.ParseError(`{"code":-32000,"message":"boom"}`))
	require.Equal(t, `{"code":-32000,"message":"boom"}`, string(wellFormed.errBytes))

	plainText := &JsonRpcResponse{idBytes: []byte(`1`)}
	require.NoError(t, plainText.ParseError(`Bad Gateway`))
	require.Empty(t, plainText.errBytes, "raw non-JSON must never be spliced into the response body")

	var buf bytes.Buffer
	_, err := plainText.WriteTo(&buf)
	require.NoError(t, err)
	require.True(t, IsSemiValidJson(buf.Bytes()), "the emitted envelope must still be JSON")
	require.Contains(t, buf.String(), `"error":{`)
	require.Contains(t, buf.String(), `Bad Gateway`)
}

// TestNewJsonRpcResponseFromBytes_TreatsNullErrorAsSuccess and its sibling
// checks below lock the constructors' error handling.
func TestNewJsonRpcResponseFromBytes_RejectsAnUnparseableId(t *testing.T) {
	t.Parallel()

	_, err := NewJsonRpcResponseFromBytes([]byte(`{`), []byte(`"0x1"`), nil)
	require.Error(t, err, "a malformed id must not produce a half-built response")
}

// TestNewJsonRpcResponse_MarshalsIdAndResult covers the programmatic
// constructor used by every internally synthesized response.
func TestNewJsonRpcResponse_MarshalsIdAndResult(t *testing.T) {
	t.Parallel()

	jr, err := NewJsonRpcResponse(int64(4), map[string]string{"k": "v"}, nil)
	require.NoError(t, err)
	require.Equal(t, `4`, string(jr.idBytes))
	require.Equal(t, `{"k":"v"}`, string(jr.GetResultBytes()))
	require.Equal(t, int64(4), jr.ID())
}

// TestNewJsonRpcResponse_RefusesAnUnmarshalableResult keeps a programming error
// from becoming a panic in the request path.
func TestNewJsonRpcResponse_RefusesAnUnmarshalableResult(t *testing.T) {
	t.Parallel()

	_, err := NewJsonRpcResponse(1, make(chan int), nil)
	require.Error(t, err)

	require.Panics(t, func() { MustNewJsonRpcResponse(1, make(chan int), nil) },
		"the Must variant is documented to panic")
	require.Panics(t, func() { MustNewJsonRpcResponseFromBytes([]byte(`{`), nil, nil) })
}

// TestMustNewJsonRpcResponse_ReturnsTheResponseOnTheHappyPath keeps the Must
// helpers usable — a panic-only implementation would still pass the check above.
func TestMustNewJsonRpcResponse_ReturnsTheResponseOnTheHappyPath(t *testing.T) {
	t.Parallel()

	jr := MustNewJsonRpcResponse(int64(2), "0x1", nil)
	require.Equal(t, int64(2), jr.ID())

	jr2 := MustNewJsonRpcResponseFromBytes([]byte(`2`), []byte(`"0x1"`), nil)
	require.Equal(t, int64(2), jr2.ID())
}

// TestJsonRpcResponse_ParseFromStream_StashesTheBodyWhenParsingFails gives the
// operator the offending payload in the log. The copy also matters: the read
// buffer goes back to the pool, so a retained slice would later show another
// request's bytes.
func TestJsonRpcResponse_ParseFromStream_StashesTheBodyWhenParsingFails(t *testing.T) {
	t.Parallel()

	bad := `<html>502 Bad Gateway</html>`
	jr := &JsonRpcResponse{}
	err := jr.ParseFromStream(nil, strings.NewReader(bad), len(bad))
	require.Error(t, err)
	require.Equal(t, bad, string(jr.GetResultBytes()),
		"the raw upstream body must be available for diagnosis")
}

// TestJsonRpcResponse_ParseFromStream_ReportsAReaderFailure — a mid-body TCP
// reset must surface, not be mistaken for an empty result.
func TestJsonRpcResponse_ParseFromStream_ReportsAReaderFailure(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{}
	err := jr.ParseFromStream(nil, iotestErrReader{}, 16)
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection reset")
}

type iotestErrReader struct{}

func (iotestErrReader) Read(p []byte) (int, error) { return 0, errors.New("connection reset by peer") }

// TestJsonRpcResponse_ParseFromStream_RejectsAMalformedIdWithoutLosingTheResult
// keeps a batch response with a broken id from being silently matched to the
// wrong request.
func TestJsonRpcResponse_ParseFromStream_RejectsAMalformedIdWithoutLosingTheResult(t *testing.T) {
	t.Parallel()

	body := `{"jsonrpc":"2.0","id":{"weird":true},"result":"0x1"}`
	jr := &JsonRpcResponse{}
	err := jr.ParseFromStream(nil, strings.NewReader(body), len(body))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported ID type")
}

// TestJsonRpcResponse_MarshalJSON_IsRefused pins the deliberate refusal: the
// whole design avoids buffering a full response in memory, so an accidental
// json.Marshal of a response must fail loudly rather than blow up the heap.
func TestJsonRpcResponse_MarshalJSON_IsRefused(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{result: []byte(`"0x1"`)}
	b, err := jr.MarshalJSON()
	require.Error(t, err)
	require.Nil(t, b)
}

// TestJsonRpcResponse_ID_LazilyParsesTheWireBytes covers the read path used by
// batch demultiplexing when only idBytes were set.
func TestJsonRpcResponse_ID_LazilyParsesTheWireBytes(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{idBytes: []byte(`"batch-3"`)}
	require.Equal(t, "batch-3", jr.ID())

	empty := &JsonRpcResponse{}
	require.Nil(t, empty.ID())
}

// TestJsonRpcResponse_GetResultString_MirrorsTheBytes — the string view is used
// by cache keys and log lines, so it must not diverge from the bytes.
func TestJsonRpcResponse_GetResultString_MirrorsTheBytes(t *testing.T) {
	t.Parallel()

	jr := &JsonRpcResponse{}
	jr.SetResult([]byte(`"0xfeed"`))
	require.Equal(t, `"0xfeed"`, jr.GetResultString())
	assert.Equal(t, []byte(`"0xfeed"`), jr.GetResultBytes())
}
