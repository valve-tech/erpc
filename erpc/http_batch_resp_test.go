package erpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// A JSON-RPC batch client matches answers to calls by position and by id. So
// every entry eRPC drops, merges or mangles on the way out shifts every later
// answer onto the wrong call — the client does not error, it just reads the
// wrong data. BatchResponseWriter streams straight to the socket without
// buffering the array first, which is fast and leaves no place to check the
// result before it ships. These tests are that check.

// countingWriter reports how many bytes actually reached it, so a test can
// compare that against the count WriteTo returns to its caller.
type countingWriter struct {
	buf bytes.Buffer
}

func (c *countingWriter) Write(p []byte) (int, error) { return c.buf.Write(p) }

// failAfterWriter accepts limit bytes and then fails, standing in for a client
// that hangs up mid-batch. It keeps what it accepted so a test can see how far
// the writer got, and counts the writes attempted AFTER the first failure —
// eRPC must abandon the response, not keep serialising into a dead socket.
type failAfterWriter struct {
	limit       int
	got         bytes.Buffer
	failed      bool
	extraWrites int
}

var errClientHungUp = errors.New("client hung up")

func (f *failAfterWriter) Write(p []byte) (int, error) {
	if f.failed {
		f.extraWrites++
	}
	remaining := f.limit - f.got.Len()
	if remaining <= 0 {
		f.failed = true
		return 0, errClientHungUp
	}
	if len(p) <= remaining {
		return f.got.Write(p)
	}
	n, _ := f.got.Write(p[:remaining])
	f.failed = true
	return n, errClientHungUp
}

// jsonRpcResponse builds a successful NormalizedResponse the batch writer can
// stream, the way a served upstream answer arrives.
func jsonRpcResponse(t *testing.T, id interface{}, result interface{}) *common.NormalizedResponse {
	t.Helper()
	jrr, err := common.NewJsonRpcResponse(id, result, nil)
	require.NoError(t, err)
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrr)
}

// TestBatchResponseWriter_WritesOneArrayEntryPerRequest is the shape contract.
// Three calls in, three answers out, in order, as one parseable array — anything
// else and the client silently reads another call's result.
func TestBatchResponseWriter_WritesOneArrayEntryPerRequest(t *testing.T) {
	w := &countingWriter{}
	brw := NewBatchResponseWriter([]interface{}{
		jsonRpcResponse(t, 1, "0x1"),
		&HttpJsonRpcErrorResponse{
			Jsonrpc: "2.0",
			Id:      2,
			Error:   &common.ErrJsonRpcExceptionExternal{Code: -32000, Message: "upstream refused"},
		},
		jsonRpcResponse(t, 3, "0x3"),
	})

	n, err := brw.WriteTo(w)
	require.NoError(t, err)
	require.Equal(t, int64(w.buf.Len()), n,
		"the returned count must be the bytes that really reached the socket")

	var entries []json.RawMessage
	require.NoError(t, json.Unmarshal(w.buf.Bytes(), &entries),
		"a batch body that does not parse as an array is unusable to every client")
	require.Len(t, entries, 3)

	var first struct {
		Id     int             `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	require.NoError(t, json.Unmarshal(entries[0], &first))
	require.Equal(t, 1, first.Id)
	require.JSONEq(t, `"0x1"`, string(first.Result))

	var second struct {
		Jsonrpc string `json:"jsonrpc"`
		Id      int    `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(entries[1], &second))
	require.Equal(t, "2.0", second.Jsonrpc)
	require.Equal(t, 2, second.Id,
		"an errored entry must keep its own id or the client pairs it with the wrong call")
	require.Equal(t, -32000, second.Error.Code)
	require.Equal(t, "upstream refused", second.Error.Message)
}

// TestBatchResponseWriter_WritesAnEmptyArrayForNoResponses guards the degenerate
// case. Writing nothing at all would leave the client waiting on a body that
// never parses.
func TestBatchResponseWriter_WritesAnEmptyArrayForNoResponses(t *testing.T) {
	w := &countingWriter{}
	n, err := NewBatchResponseWriter(nil).WriteTo(w)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)
	require.Equal(t, "[]", w.buf.String())
}

// TestBatchResponseWriter_TurnsABareErrorIntoAJsonRpcEntry covers the entry type
// eRPC produces when it cannot even work out which call failed. It must still
// occupy its slot in the array, with a null id, so the entries after it stay
// aligned with the calls that produced them.
func TestBatchResponseWriter_TurnsABareErrorIntoAJsonRpcEntry(t *testing.T) {
	w := &countingWriter{}
	brw := NewBatchResponseWriter([]interface{}{
		errors.New("could not parse request"),
		jsonRpcResponse(t, 9, "0x9"),
	})

	_, err := brw.WriteTo(w)
	require.NoError(t, err)

	var entries []json.RawMessage
	require.NoError(t, json.Unmarshal(w.buf.Bytes(), &entries))
	require.Len(t, entries, 2, "an unattributable error must still take one slot")

	var first struct {
		Jsonrpc string      `json:"jsonrpc"`
		Id      interface{} `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(entries[0], &first))
	require.Equal(t, "2.0", first.Jsonrpc)
	require.Nil(t, first.Id, "eRPC cannot invent an id it never read")
	require.Equal(t, int(common.JsonRpcErrorServerSideException), first.Error.Code)
	require.Equal(t, "could not parse request", first.Error.Message)

	var second struct {
		Id int `json:"id"`
	}
	require.NoError(t, json.Unmarshal(entries[1], &second))
	require.Equal(t, 9, second.Id, "the entry after an error must keep its own id")
}

// TestBatchResponseWriter_EncodesAnUnrecognisedEntryRatherThanDroppingIt covers
// the fallthrough. A type nobody anticipated must still produce one array entry;
// skipping it would shorten the array and shift every later answer.
func TestBatchResponseWriter_EncodesAnUnrecognisedEntryRatherThanDroppingIt(t *testing.T) {
	w := &countingWriter{}
	brw := NewBatchResponseWriter([]interface{}{
		map[string]interface{}{"jsonrpc": "2.0", "id": 7, "result": "unusual"},
		jsonRpcResponse(t, 8, "0x8"),
	})

	_, err := brw.WriteTo(w)
	require.NoError(t, err)

	var entries []json.RawMessage
	require.NoError(t, json.Unmarshal(w.buf.Bytes(), &entries))
	require.Len(t, entries, 2)
	require.Contains(t, string(entries[0]), `"unusual"`)
}

// TestBatchResponseWriter_ReportsAnUnserialisableEntry checks the marshal error
// path. eRPC must surface it instead of writing half an entry and calling the
// batch complete.
func TestBatchResponseWriter_ReportsAnUnserialisableEntry(t *testing.T) {
	w := &countingWriter{}
	// A channel has no JSON form, so the encoder must refuse it.
	_, err := NewBatchResponseWriter([]interface{}{make(chan int)}).WriteTo(w)
	require.Error(t, err)
}

// TestBatchResponseWriter_ReportsAResponseItCannotStream covers an entry whose
// own WriteTo fails — an already-released or never-populated response. Returning
// nil here would hand the client a truncated array with no signal at all.
func TestBatchResponseWriter_ReportsAResponseItCannotStream(t *testing.T) {
	w := &countingWriter{}
	brw := NewBatchResponseWriter([]interface{}{
		jsonRpcResponse(t, 1, "0x1"),
		common.NewNormalizedResponse(), // nothing to write
	})

	n, err := brw.WriteTo(w)
	require.Error(t, err, "a batch that cannot be completed must not report success")
	require.Contains(t, err.Error(), "unexpected empty response")
	require.Greater(t, n, int64(0), "the count must still cover the bytes already sent")
	require.NotContains(t, w.buf.String(), "]",
		"the array is left unterminated, which is exactly why the error must reach the caller")
}

// TestBatchResponseWriter_StopsAndReportsWhenTheClientHangsUp walks the write
// failure across every position in the entry loop. Each stop must return the
// transport error, and the count must never claim more bytes than the socket
// took — that count feeds response-size metrics and access logs.
func TestBatchResponseWriter_StopsAndReportsWhenTheClientHangsUp(t *testing.T) {
	build := func() *BatchResponseWriter {
		return NewBatchResponseWriter([]interface{}{
			jsonRpcResponse(t, 1, "0x1"),
			&HttpJsonRpcErrorResponse{
				Jsonrpc: "2.0",
				Id:      2,
				Error:   &common.ErrJsonRpcExceptionExternal{Code: -32000, Message: "boom"},
			},
			jsonRpcResponse(t, 3, "0x3"),
		})
	}

	full := &countingWriter{}
	total, err := build().WriteTo(full)
	require.NoError(t, err)

	// limit 0 fails on the opening bracket; the rest land inside an entry, on a
	// separator, or on the closing bracket.
	for limit := 0; limit < int(total); limit++ {
		w := &failAfterWriter{limit: limit}
		n, err := build().WriteTo(w)
		require.Error(t, err, "a write cut short at %d bytes must be reported", limit)
		require.LessOrEqual(t, n, int64(limit),
			"WriteTo reported %d bytes at a socket that accepted at most %d", n, limit)
		require.Zero(t, w.extraWrites,
			"after the socket refused at %d bytes eRPC kept writing %d more times; "+
				"a large batch would then serialise in full for a client that is gone",
			limit, w.extraWrites)
	}

	// One byte past the end there is nothing left to refuse, so the batch
	// completes — the loop above therefore tests failure, not a writer that
	// always fails.
	ok := &failAfterWriter{limit: int(total)}
	n, err := build().WriteTo(ok)
	require.NoError(t, err)
	require.Equal(t, total, n)
}

// TestWriteJsonRpcError_KeepsTheRequestIdItWasGiven covers the hand-rolled error
// encoder directly. It writes the envelope field by field rather than marshaling
// a struct, so an id type it mishandles produces a body that parses but pairs
// with the wrong call.
func TestWriteJsonRpcError_KeepsTheRequestIdItWasGiven(t *testing.T) {
	for _, id := range []interface{}{nil, 42, "req-abc"} {
		var buf bytes.Buffer
		n, err := writeJsonRpcError(&buf, &HttpJsonRpcErrorResponse{
			Jsonrpc: "2.0",
			Id:      id,
			Error:   &common.ErrJsonRpcExceptionExternal{Code: -32603, Message: "internal"},
		})
		require.NoError(t, err)
		require.Equal(t, int64(buf.Len()), n)

		var got struct {
			Jsonrpc string      `json:"jsonrpc"`
			Id      interface{} `json:"id"`
			Error   struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(buf.Bytes(), &got), "body was %s", buf.String())
		require.Equal(t, "2.0", got.Jsonrpc)
		require.Equal(t, -32603, got.Error.Code)
		require.Equal(t, "internal", got.Error.Message)

		switch want := id.(type) {
		case nil:
			require.Nil(t, got.Id)
		case int:
			require.Equal(t, float64(want), got.Id)
		case string:
			require.Equal(t, want, got.Id)
		}
	}
}

// TestWriteJsonRpcError_ReportsAShortWriteAtEveryFieldBoundary walks the same
// cut-short sweep over the encoder's own field writes. It writes in seven
// separate calls, and an unchecked one would let a half-written error escape as
// a success.
func TestWriteJsonRpcError_ReportsAShortWriteAtEveryFieldBoundary(t *testing.T) {
	resp := &HttpJsonRpcErrorResponse{
		Jsonrpc: "2.0",
		Id:      7,
		Error:   &common.ErrJsonRpcExceptionExternal{Code: -32603, Message: "internal"},
	}

	var full bytes.Buffer
	total, err := writeJsonRpcError(&full, resp)
	require.NoError(t, err)

	for limit := 0; limit < int(total); limit++ {
		w := &failAfterWriter{limit: limit}
		n, err := writeJsonRpcError(w, resp)
		require.Error(t, err, "a write cut short at %d bytes must be reported", limit)
		require.LessOrEqual(t, n, int64(limit))
		require.Zero(t, w.extraWrites,
			"the encoder wrote %d more fields after the socket refused at %d bytes; "+
				"one unchecked write here lets a half-written error look like a success",
			w.extraWrites, limit)
	}
}

var _ io.Writer = (*failAfterWriter)(nil)
