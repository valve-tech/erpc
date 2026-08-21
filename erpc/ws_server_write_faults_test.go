package erpc

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// A WebSocket client has no status code to read. Everything it learns about a
// failure it learns from the frames eRPC sends, or from the frames it never
// gets. These tests drive the four write helpers over a socket that dies on
// command (see fault_transport_test.go) and check two things each time: what
// the client receives, and whether eRPC keeps writing into a dead socket.

// wsFrames decodes the unmasked frames a server wrote onto the wire and returns
// the text payloads. A frame the fault cut short is returned as far as it got,
// so a test can see truncation rather than silently skipping it.
func wsFrames(t *testing.T, raw []byte) []string {
	t.Helper()
	var out []string
	for len(raw) >= 2 {
		opcode := raw[0] & 0x0f
		require.Zero(t, raw[1]&0x80, "a server frame must never be masked")
		size := int(raw[1] & 0x7f)
		i := 2
		switch size {
		case 126:
			require.GreaterOrEqual(t, len(raw), i+2, "truncated 16-bit length")
			size = int(binary.BigEndian.Uint16(raw[i : i+2]))
			i += 2
		case 127:
			require.GreaterOrEqual(t, len(raw), i+8, "truncated 64-bit length")
			size = int(binary.BigEndian.Uint64(raw[i : i+8]))
			i += 8
		}
		isText := opcode == 0x1 || opcode == 0x0 // text, or a continuation of one
		if len(raw) < i+size {
			if isText {
				out = append(out, string(raw[i:]))
			}
			break
		}
		if isText {
			out = append(out, string(raw[i:i+size]))
		}
		raw = raw[i+size:]
	}
	return out
}

// TestWsFrames_ReadsBackWhatTheServerWrote checks the decoder itself. A decoder
// that silently returned nothing would make every assertion below vacuous.
func TestWsFrames_ReadsBackWhatTheServerWrote(t *testing.T) {
	wsc, sock := newFaultWsConnection(t)

	require.NoError(t, wsc.writeJSON(map[string]string{"hello": "world"}))

	frames := wsFrames(t, sock.Bytes())
	require.Len(t, frames, 1, "one writeJSON must produce exactly one text frame")
	require.JSONEq(t, `{"hello":"world"}`, frames[0])
}

// TestWsWriteNormalizedResponse_DeliversTheAnswerAndStopsOnceTheClientIsGone
// walks the helper through its three outcomes on one connection: a delivered
// answer, a socket that refuses, and the call after that refusal. The last one
// is the load-bearing case — eRPC must not serialise a second response into a
// socket it already knows is dead.
func TestWsWriteNormalizedResponse_DeliversTheAnswerAndStopsOnceTheClientIsGone(t *testing.T) {
	wsc, sock := newFaultWsConnection(t)

	wsc.writeNormalizedResponse(jsonRpcResponse(t, 1, "0x1"))

	frames := wsFrames(t, sock.Bytes())
	require.Len(t, frames, 1)
	var first struct {
		Id     int             `json:"id"`
		Result json.RawMessage `json:"result"`
	}
	require.NoError(t, json.Unmarshal([]byte(frames[0]), &first), "frame was %q", frames[0])
	require.Equal(t, 1, first.Id, "a client pairs the answer with its call by id")
	require.JSONEq(t, `"0x1"`, string(first.Result))

	// The client hangs up. The next answer must be attempted once and abandoned.
	sock.Reset()
	sock.HangUp()
	wsc.writeNormalizedResponse(jsonRpcResponse(t, 2, "0x2"))
	require.NotZero(t, sock.Writes(), "eRPC must at least try to deliver the answer")
	require.Zero(t, sock.WritesAfterFailure(),
		"the socket refused, and eRPC wrote %d more times", sock.WritesAfterFailure())
	require.Empty(t, sock.Bytes(), "a refused socket must hold none of the answer")

	// The connection now carries a recorded write error, so the helper must
	// give up before it touches the socket at all.
	sock.Reset()
	wsc.writeNormalizedResponse(jsonRpcResponse(t, 3, "0x3"))
	require.Zero(t, sock.Writes(),
		"after a failed write eRPC still tried the socket %d times; a busy subscription "+
			"would then serialise every pending answer for a client that is gone", sock.Writes())
}

// TestWsWriteNormalizedResponse_WritesNothingOnAClosedConnection covers the
// guard. Close() has already sent the close frame, so a late answer written
// after it would be a protocol violation, not just wasted work.
func TestWsWriteNormalizedResponse_WritesNothingOnAClosedConnection(t *testing.T) {
	wsc, sock := newFaultWsConnection(t)
	wsc.closed.Store(true)

	wsc.writeNormalizedResponse(jsonRpcResponse(t, 1, "0x1"))
	require.Zero(t, sock.Writes(), "a closed connection must produce no frames")
	require.Empty(t, sock.Bytes())
}

// TestWsWriteNormalizedResponse_SendsAnEmptyFrameWhenItCannotSerialise records
// what the client actually gets when the response cannot be written: a complete
// text frame with an empty payload.
//
// This is a defect, characterised here rather than endorsed. The client reads a
// message that is not JSON and carries no id, so the call it belongs to never
// resolves — it hangs until the client's own timeout. The HTTP path answers the
// same failure with a JSON-RPC error envelope. See the upstream bug log.
func TestWsWriteNormalizedResponse_SendsAnEmptyFrameWhenItCannotSerialise(t *testing.T) {
	wsc, sock := newFaultWsConnection(t)

	// A response that was never populated cannot be streamed.
	wsc.writeNormalizedResponse(common.NewNormalizedResponse())

	frames := wsFrames(t, sock.Bytes())
	require.Len(t, frames, 1, "the client is sent exactly one frame")
	require.Empty(t, frames[0],
		"the payload is empty: no id, no error code, nothing the client can act on")
	require.Zero(t, sock.WritesAfterFailure())
}

// TestWsWriteBatchResponse_AbandonsTheBatchWhenTheSocketDiesMidFrame drives a
// batch large enough that gorilla flushes it in several socket writes, then
// kills the socket after the first flush.
//
// The assertion that carries weight is the prefix check: whatever the client
// did receive must be byte-for-byte the opening of the undamaged batch. A
// client matches batch answers to calls by position, so an entry emitted out of
// order — or one dropped on the way — pairs every later answer with the wrong
// call, and truncation is the one case where the client cannot re-read the
// array to notice.
//
// eRPC reports nothing at all here: writeBatchResponse discards the error (see
// upstream bug 44). The error propagation itself is pinned one layer down, in
// http_batch_resp_test.go.
func TestWsWriteBatchResponse_AbandonsTheBatchWhenTheSocketDiesMidFrame(t *testing.T) {
	// Each entry is ~700 bytes, so the batch far exceeds gorilla's 4 KiB write
	// buffer and reaches the socket in more than one write.
	build := func() []interface{} {
		responses := make([]interface{}, 0, 40)
		for i := 0; i < 40; i++ {
			responses = append(responses, jsonRpcResponse(t, i, fmt.Sprintf("0x%0700d", i%10)))
		}
		return responses
	}

	// Measure the undamaged batch first, so the fault below lands inside it.
	wsc, sock := newFaultWsConnection(t)
	wsc.writeBatchResponse(build())
	full := sock.Bytes()
	require.Greater(t, sock.Writes(), 1,
		"the fixture needs a batch big enough to reach the socket more than once")

	// The undamaged batch answers call n in slot n. This is checked against the
	// ids in the JSON, not against the writer's own output, so a writer that
	// emitted the entries in another order would fail here.
	var entries []struct {
		Id int `json:"id"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.Join(wsFrames(t, full), "")), &entries),
		"the whole batch must parse as an array")
	require.Len(t, entries, 40)
	for i := range entries {
		require.Equal(t, i, entries[i].Id, "slot %d holds the answer to call %d", i, entries[i].Id)
	}

	wsc2, sock2 := newFaultWsConnection(t)
	sock2.FailAfterBytes(len(full) / 2)
	wsc2.writeBatchResponse(build())

	require.True(t, sock2.Failed(), "the fault must have fired inside the batch")
	require.Zero(t, sock2.WritesAfterFailure(),
		"the socket refused mid-batch and eRPC wrote %d more times", sock2.WritesAfterFailure())
	require.Len(t, sock2.Bytes(), len(full)/2, "the socket holds exactly what it accepted")
	require.True(t, bytes.HasPrefix(full, sock2.Bytes()),
		"the truncated batch is not a prefix of the whole one, so the entries the "+
			"client did receive are not the entries it would have received")
}

// TestWsWriteBatchResponse_WritesNothingOnAClosedConnection covers the guard,
// and TestWsWriteBatchResponse_GivesUpBeforeTouchingADeadSocket covers the
// NextWriter refusal after an earlier failure.
func TestWsWriteBatchResponse_WritesNothingOnAClosedConnection(t *testing.T) {
	wsc, sock := newFaultWsConnection(t)
	wsc.closed.Store(true)

	wsc.writeBatchResponse([]interface{}{jsonRpcResponse(t, 1, "0x1")})
	require.Zero(t, sock.Writes())
}

func TestWsWriteBatchResponse_GivesUpBeforeTouchingADeadSocket(t *testing.T) {
	wsc, sock := newFaultWsConnection(t)

	sock.HangUp()
	wsc.writeBatchResponse([]interface{}{jsonRpcResponse(t, 1, "0x1")})
	require.True(t, sock.Failed())

	sock.Reset()
	wsc.writeBatchResponse([]interface{}{jsonRpcResponse(t, 2, "0x2")})
	require.Zero(t, sock.Writes(),
		"the connection already recorded a write error, so the batch must not be encoded again")
}

// TestWsWriteJSON_ReportsWhatTheCallerMustActuponCovers the one write helper
// that returns its error. WriteSubscriptionNotification is its only production
// caller that acts on the result, so an error swallowed here becomes a
// subscription that looks healthy while it delivers nothing.
func TestWsWriteJSON_ReportsWhatTheCallerMustActUpon(t *testing.T) {
	t.Run("a closed connection is refused before any write", func(t *testing.T) {
		wsc, sock := newFaultWsConnection(t)
		wsc.closed.Store(true)

		err := wsc.writeJSON(map[string]string{"a": "b"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "connection closed")
		require.Zero(t, sock.Writes())
	})

	t.Run("a dead socket surfaces its own error", func(t *testing.T) {
		wsc, sock := newFaultWsConnection(t)
		sock.HangUp()

		err := wsc.writeJSON(map[string]string{"a": "b"})
		require.ErrorIs(t, err, errPeerHungUp,
			"the transport error must reach the caller unchanged")
		require.Zero(t, sock.WritesAfterFailure())
	})

	t.Run("a subscription notification reports the delivery failure", func(t *testing.T) {
		wsc, sock := newFaultWsConnection(t)

		require.NoError(t, wsc.WriteSubscriptionNotification("0xsub", json.RawMessage(`{"number":"0x1"}`)))
		frames := wsFrames(t, sock.Bytes())
		require.Len(t, frames, 1)
		var note struct {
			Method string `json:"method"`
			Params struct {
				Subscription string          `json:"subscription"`
				Result       json.RawMessage `json:"result"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal([]byte(frames[0]), &note), "frame was %q", frames[0])
		require.Equal(t, "eth_subscription", note.Method)
		require.Equal(t, "0xsub", note.Params.Subscription,
			"a client routes the event by this id; the wrong id delivers it to the wrong handler")
		require.JSONEq(t, `{"number":"0x1"}`, string(note.Params.Result))

		sock.Reset()
		sock.HangUp()
		require.ErrorIs(t, wsc.WriteSubscriptionNotification("0xsub", json.RawMessage(`{"number":"0x2"}`)),
			errPeerHungUp,
			"the subscription manager only learns the client is gone from this error")
	})
}

// TestWsWriteMessage_RefusesOnAClosedConnectionAndReportsATransportError
// covers the raw-frame helper the same way.
func TestWsWriteMessage_RefusesOnAClosedConnectionAndReportsATransportError(t *testing.T) {
	wsc, sock := newFaultWsConnection(t)
	require.NoError(t, wsc.writeMessage(1, []byte(`{"ok":true}`)))
	require.Equal(t, []string{`{"ok":true}`}, wsFrames(t, sock.Bytes()))

	sock.Reset()
	sock.HangUp()
	require.ErrorIs(t, wsc.writeMessage(1, []byte(`{"ok":true}`)), errPeerHungUp)

	wsc.closed.Store(true)
	sock.Reset()
	err := wsc.writeMessage(1, []byte(`{"ok":true}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection closed")
	require.Zero(t, sock.Writes())
}

// TestWsWriteErrorResponse_SendsAJsonRpcEnvelopeCarryingTheRequestId is the
// error path a client depends on to fail a call instead of waiting for it. The
// id must survive: without it the client cannot tell which call failed.
func TestWsWriteErrorResponse_SendsAJsonRpcEnvelopeCarryingTheRequestId(t *testing.T) {
	wsc, sock := newFaultWsConnection(t)

	nq := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":77,"method":"eth_getBalance","params":[]}`))
	wsc.writeErrorResponse(nq, common.NewErrInvalidRequest(fmt.Errorf("bad params")), nil, &common.TRUE)

	frames := wsFrames(t, sock.Bytes())
	require.Len(t, frames, 1)
	var got struct {
		Jsonrpc string      `json:"jsonrpc"`
		Id      interface{} `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(frames[0]), &got), "frame was %q", frames[0])
	require.Equal(t, "2.0", got.Jsonrpc)
	require.Equal(t, float64(77), got.Id, "the client pairs the failure with its call by id")
	require.NotZero(t, got.Error.Code)

	// A dead socket must not leave a half-frame behind.
	sock.Reset()
	sock.HangUp()
	wsc.writeErrorResponse(nq, common.NewErrInvalidRequest(fmt.Errorf("bad params")), nil, &common.TRUE)
	require.Zero(t, sock.WritesAfterFailure())
	require.Empty(t, sock.Bytes())
}
