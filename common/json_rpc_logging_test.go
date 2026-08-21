package common

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A JSON-RPC request and response are logged verbatim on the debug and error
// paths. That log line is what an operator reads when a request misbehaves, so
// what it holds — and what it refuses to hold — is behaviour, not decoration.

// logLine renders one zerolog event carrying obj and returns the JSON it wrote.
func logLine(t *testing.T, obj zerolog.LogObjectMarshaler) string {
	t.Helper()

	prevLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prevLevel) })

	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	logger.Info().EmbedObject(obj).Msg("under test")
	return buf.String()
}

// ---------------------------------------------------------------------------
// JsonRpcResponse.MarshalZerologObject
// ---------------------------------------------------------------------------

// A nil response must not panic. Response logging happens on the error path,
// where a nil response is exactly what the caller has.
func TestJsonRpcResponse_MarshalZerologObject_NilIsSafe(t *testing.T) {
	var r *JsonRpcResponse
	assert.NotPanics(t, func() { _ = logLine(t, r) })
}

// A well-formed result must be embedded as JSON, not as a quoted string, so the
// operator's log tooling can query into it.
func TestJsonRpcResponse_MarshalZerologObject_EmbedsValidJsonVerbatim(t *testing.T) {
	r := MustNewJsonRpcResponseFromBytes([]byte(`7`), []byte(`{"number":"0x1"}`), nil)

	line := logLine(t, r)

	assert.Contains(t, line, `"id":7`)
	assert.Contains(t, line, `"result":{"number":"0x1"}`, "valid JSON must stay JSON in the log")
	assert.Contains(t, line, `"resultSize":16`)
}

// A result that is not JSON must still reach the log, as a string. Dropping it
// would hide the very payload that caused the problem.
func TestJsonRpcResponse_MarshalZerologObject_FallsBackToAStringForNonJson(t *testing.T) {
	r := MustNewJsonRpcResponseFromBytes([]byte(`7`), []byte(`<html>502 Bad Gateway</html>`), nil)

	line := logLine(t, r)

	assert.Contains(t, line, `"result":"<html>502 Bad Gateway</html>"`,
		"an upstream that answered with HTML must still be visible")
}

// An error member is always rendered when present, from the raw bytes when the
// upstream sent a well-formed JSON-RPC error, and from the normalised exception
// when it sent something else.
//
// Note: the "raw bytes are not JSON" arm of this function cannot run.
// ParseError (common/json_rpc.go:421) is the only writer of errBytes, and it
// writes only after the payload parsed into a JSON-RPC error object, so those
// bytes always start with '{'.
func TestJsonRpcResponse_MarshalZerologObject_RendersTheErrorMember(t *testing.T) {
	t.Run("a well-formed error is embedded verbatim", func(t *testing.T) {
		r := MustNewJsonRpcResponseFromBytes([]byte(`7`), nil, []byte(`{"code":-32000,"message":"nope"}`))
		line := logLine(t, r)
		assert.Contains(t, line, `"error":{"code":-32000,"message":"nope"}`)
	})

	t.Run("a bare string error is logged as the exception it was normalised into", func(t *testing.T) {
		r := MustNewJsonRpcResponseFromBytes([]byte(`7`), nil, []byte(`rate limited`))
		line := logLine(t, r)
		assert.Contains(t, line, `"message":"rate limited"`,
			"the upstream's own words must survive normalisation")
		assert.Contains(t, line, `"code":-32603`)
	})
}

// A result under the 300KB threshold is logged whole; a result over it is
// logged as a head and a tail. The cap is what stops one oversized response
// from filling the operator's log pipeline.
func TestJsonRpcResponse_MarshalZerologObject_TruncatesAnOversizedResult(t *testing.T) {
	const head = 150 * 1024

	t.Run("just under the threshold is logged whole", func(t *testing.T) {
		body := []byte(`"` + strings.Repeat("a", 300*1024-3) + `"`)
		require.Len(t, body, 300*1024-1)

		line := logLine(t, MustNewJsonRpcResponseFromBytes([]byte(`1`), body, nil))

		assert.Contains(t, line, `"result":`)
		assert.NotContains(t, line, "resultHead")
		assert.NotContains(t, line, "resultTail")
	})

	t.Run("at and over the threshold is split into a head and a tail", func(t *testing.T) {
		// Distinct first and last bytes so the test can tell which end is which.
		body := []byte(`"H` + strings.Repeat("a", 400*1024) + `T"`)

		line := logLine(t, MustNewJsonRpcResponseFromBytes([]byte(`1`), body, nil))

		require.NotContains(t, line, `"result":`, "the whole body must not be logged")
		assert.Contains(t, line, `"resultHead":"\"H`, "the head starts at the first byte")
		assert.Contains(t, line, `T\""`, "the tail ends at the last byte")

		headStart := strings.Index(line, `"resultHead":"`)
		tailStart := strings.Index(line, `"resultTail":"`)
		require.Positive(t, headStart)
		require.Positive(t, tailStart)
		require.Less(t, headStart, tailStart, "head is logged before tail")

		// The rendered head is JSON-escaped, so it is at least `head` bytes and
		// nowhere near the 400KB body.
		rendered := tailStart - headStart
		assert.Greater(t, rendered, head, "the head must carry the configured 150KB")
		assert.Less(t, rendered, len(body), "the head must not carry the whole body")
	})
}

// ---------------------------------------------------------------------------
// JsonRpcRequest.MarshalZerologObject
// ---------------------------------------------------------------------------

// The request line must carry the three fields an operator correlates on:
// method, params and id. A nil request must not panic.
func TestJsonRpcRequest_MarshalZerologObject(t *testing.T) {
	t.Run("nil is safe", func(t *testing.T) {
		var r *JsonRpcRequest
		assert.NotPanics(t, func() { _ = logLine(t, r) })
	})

	t.Run("carries method, params and id", func(t *testing.T) {
		r := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xabc", "latest"})
		r.ID = 42

		line := logLine(t, r)

		assert.Contains(t, line, `"method":"eth_getBalance"`)
		assert.Contains(t, line, `"params":["0xabc","latest"]`)
		assert.Contains(t, line, `"id":42`)
	})
}

// ---------------------------------------------------------------------------
// Traced locking
// ---------------------------------------------------------------------------

// LockWithTrace and RLockWithTrace exist so a stalled lock shows up as a span
// an operator can find. They must take the lock they name, and they must emit
// the span only when detailed tracing is on.
func TestJsonRpcRequest_LockWithTrace(t *testing.T) {
	t.Run("with detailed tracing the spans are recorded", func(t *testing.T) {
		h := newTracingHarness(t, true)
		r := NewJsonRpcRequest("eth_call", nil)

		r.LockWithTrace(context.Background())
		r.Unlock()
		r.RLockWithTrace(context.Background())
		r.RUnlock()

		require.NotNil(t, h.endedNamed("JsonRpcRequest.Lock"))
		require.NotNil(t, h.endedNamed("JsonRpcRequest.RLock"))
		assert.Empty(t, h.startedButNotEnded())
	})

	t.Run("without detailed tracing the locks still work and emit nothing", func(t *testing.T) {
		h := newTracingHarness(t, false)
		r := NewJsonRpcRequest("eth_call", nil)

		r.LockWithTrace(context.Background())
		r.Unlock()
		r.RLockWithTrace(context.Background())
		r.RUnlock()

		assert.Empty(t, h.ended(), "detail spans must stay off when the operator turned them off")
	})
}
