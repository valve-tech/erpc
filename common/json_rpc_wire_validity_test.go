package common

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// An upstream that answers without an id member used to make WriteTo emit
// `{"jsonrpc":"2.0","id":,"result":…}`. No client can parse that, and eRPC
// reported no error — the operator saw a 200 and the client saw a syntax
// error. JSON-RPC 2.0 names null as the id of a response whose id cannot be
// determined, so that is what goes on the wire now.
//
// FuzzJsonRpcResponseParseFromStream found it and now guards it: every valid
// upstream body must re-serialise to valid JSON.
func TestJsonRpcResponse_WriteTo_AMissingIdBecomesNull(t *testing.T) {
	cases := []string{
		`{"jsonrpc":"2.0","result":"0x1"}`,
		`{"result":"0x1"}`,
		`{}`,
		`{"jsonrpc":"2.0","error":{"code":-1,"message":"x"}}`,
	}

	for _, body := range cases {
		t.Run(body, func(t *testing.T) {
			r := &JsonRpcResponse{}
			require.NoError(t, r.ParseFromStream(nil, bytes.NewReader([]byte(body)), len(body)))

			var wire bytes.Buffer
			_, err := r.WriteTo(&wire)
			require.NoError(t, err)
			require.True(t, json.Valid(wire.Bytes()),
				"the client must receive parseable JSON, got %s", wire.String())
			require.Contains(t, wire.String(), `"id":null`)
		})
	}
}

// An explicit id still goes out verbatim — the null above is a fallback, not a
// rewrite.
func TestJsonRpcResponse_WriteTo_AnExplicitIdSurvives(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":9007199254740993,"result":"0x1"}`
	r := &JsonRpcResponse{}
	require.NoError(t, r.ParseFromStream(nil, bytes.NewReader([]byte(body)), len(body)))

	var wire bytes.Buffer
	_, err := r.WriteTo(&wire)
	require.NoError(t, err)
	require.Contains(t, wire.String(), `"id":9007199254740993`)
}

// eRPC's parser is more permissive than the JSON spec: sonic accepts an
// unescaped control character inside a string, and the result bytes reach the
// client verbatim. So eRPC launders a non-conforming upstream body into a
// response that strict clients (encoding/json, JSON.parse, python json)
// reject, and it reports no error at any layer.
//
// This test pins the CURRENT behaviour. It fails once eRPC either rejects the
// body or escapes the character — see the entry in
// valve/upstream-bug-log.md before changing it.
func TestJsonRpcResponse_AnUnescapedControlCharacterPassesThrough(t *testing.T) {
	body := "{\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-32000,\"message\":\"boom\nnext line\"}}"
	require.False(t, json.Valid([]byte(body)), "the fixture itself must be non-conforming JSON")

	r := &JsonRpcResponse{}
	require.NoError(t, r.ParseFromStream(nil, bytes.NewReader([]byte(body)), len(body)),
		"eRPC accepts the body today")

	var wire bytes.Buffer
	_, err := r.WriteTo(&wire)
	require.NoError(t, err)
	require.False(t, json.Valid(wire.Bytes()),
		"eRPC forwards the raw control character, so the client gets invalid JSON")
}
