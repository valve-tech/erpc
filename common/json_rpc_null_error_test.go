package common

import (
	"bytes"

	"testing"
)

// A JSON-RPC response that carries `"error": null` is a SUCCESS. JSON-RPC 1.0
// requires the member on every response, so bitcoind sends it on every single
// call; plenty of JSON-RPC 2.0 servers send it too.
//
// eRPC used to read the four bytes `null` as an error object, fall through every
// special case in ParseError and end up reporting an upstream exception whose
// message was the literal string "null". No fixture in the repo carried the
// member, so nothing caught it until a real bitcoind envelope went through the
// request path.

func TestJsonRpcResponse_NullErrorMemberIsNotAnError(t *testing.T) {
	body := `{"result":"besthash","error":null,"id":1}`

	r := &JsonRpcResponse{}
	if err := r.ParseFromStream(nil, bytes.NewReader([]byte(body)), len(body)); err != nil {
		t.Fatalf("ParseFromStream: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("a null error member parsed as an error: %v", r.Error)
	}
	if got := r.GetResultString(); got != `"besthash"` {
		t.Fatalf("result = %s, want \"besthash\"", got)
	}
}

func TestJsonRpcResponse_NullErrorMemberFromBytesIsNotAnError(t *testing.T) {
	// The batch path extracts the members by name and hands the raw bytes over,
	// so it needs the same answer as the streaming parser.
	r, err := NewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"besthash"`), []byte(`null`))
	if err != nil {
		t.Fatalf("NewJsonRpcResponseFromBytes: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("a null error member parsed as an error: %v", r.Error)
	}
}

func TestJsonRpcResponse_RealErrorObjectsStillParse(t *testing.T) {
	// The negative control. Loosening the null case must not loosen anything
	// else — an upstream error has to keep arriving as an error.
	body := `{"result":null,"error":{"code":-8,"message":"Block height out of range"},"id":1}`

	r := &JsonRpcResponse{}
	if err := r.ParseFromStream(nil, bytes.NewReader([]byte(body)), len(body)); err != nil {
		t.Fatalf("ParseFromStream: %v", err)
	}
	if r.Error == nil {
		t.Fatal("a real JSON-RPC error object parsed as success")
	}
	if r.Error.Message != "Block height out of range" {
		t.Fatalf("error message = %q, want bitcoind's reason", r.Error.Message)
	}

	// And the non-standard shapes ParseError special-cases must survive too.
	r2 := &JsonRpcResponse{}
	if err := r2.ParseError(`{"error":"reverted"}`); err != nil {
		t.Fatalf("ParseError: %v", err)
	}
	if r2.Error == nil || r2.Error.Message != "reverted" {
		t.Fatalf("string-shaped error lost: %v", r2.Error)
	}
}
