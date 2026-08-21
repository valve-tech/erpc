package common

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// The seeds below are REAL bodies lifted from the existing tests in this
// package (json_rpc_test.go, json_rpc_parse_test.go, json_rpc_wire_test.go,
// json_rpc_null_error_test.go) and from the upstream fixtures under
// architecture/evm. They are the shapes eRPC actually sees on the wire.

var fuzzResponseSeeds = []string{
	`{"jsonrpc":"2.0","id":7,"result":"0x1a"}`,
	`{"jsonrpc":"2.0","id":9,"result":[1,2,3]}`,
	`{"jsonrpc":"2.0","id":3,"result":{"v":1}}`,
	`{"jsonrpc":"2.0","id":1,"result":{"number":"0x10","hash":"0xdead"}}`,
	`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"header not found"}}`,
	`{"jsonrpc":"2.0","id":1,"error":null,"result":"0x0"}`,
	`{"jsonrpc":"2.0","id":{"weird":true},"result":"0x1"}`,
	`{"jsonrpc":"2.0","id":99999999999999999999,"result":null}`,
	`{"jsonrpc":"2.0","id":"abc","result":{"logs":[{"blockTimestamp":"0x1","data":"0x00"}]}}`,
	`{"jsonrpc":"2.0","id":1,"result":{"baseFeePerGas":"0x137d0bb9e","gasUsed":"0x15cfc87","hash":"0xe27565f06f04fe79d3c3bb4dc9749a0318c520d7f784545be4d1a65bbcac21db","number":"0x7f04f1","transactions":[]}}`,
	`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"result":"0x2"}]`,
	`<html><body>502 Bad Gateway</body></html>`,
	``,
}

// FuzzJsonRpcResponseParseFromStream drives every byte an upstream can send
// through the response parser and then through each of the accessors the rest
// of eRPC calls on a parsed response.
func FuzzJsonRpcResponseParseFromStream(f *testing.F) {
	for _, s := range fuzzResponseSeeds {
		f.Add([]byte(s))
	}

	ctx := context.Background()
	sink := zerolog.New(io.Discard)

	f.Fuzz(func(t *testing.T, data []byte) {
		r := &JsonRpcResponse{}
		err := r.ParseFromStream(nil, bytes.NewReader(data), len(data))
		if err != nil {
			// A parse failure is a legitimate outcome for hostile bytes, but the
			// object must still be safe to touch: eRPC logs it on the error path.
			sink.Info().Object("resp", r).Msg("parse failed")
			return
		}

		_ = r.ID()
		_, _ = r.Size(ctx)
		_ = r.IsResultEmptyish(ctx)
		_ = r.ResultLength()
		_, _ = r.PeekStringByPath(ctx, "number")
		_, _ = r.PeekBytesByPath(ctx, "logs", 0, "blockTimestamp")
		_, _ = r.CanonicalHash(ctx)
		_, _ = r.CanonicalHashWithIgnoredFields([]string{"status", "logs.*.blockTimestamp"}, ctx)
		// A VALID body must go back out as JSON the client can parse. This
		// caught `{"jsonrpc":"2.0","id":,"result":…}` — the envelope WriteTo
		// produced for an upstream reply that carried no id member.
		//
		// The invariant is scoped to valid input on purpose. sonic accepts
		// unescaped control characters inside strings and eRPC forwards the
		// result bytes verbatim, so an already-invalid upstream body comes out
		// invalid as well. That is a separate defect, recorded in
		// valve/upstream-bug-log.md, not a regression this target should own.
		var wire bytes.Buffer
		if _, err := r.WriteTo(&wire); err == nil && json.Valid(data) {
			if !json.Valid(wire.Bytes()) {
				t.Fatalf("WriteTo emitted invalid JSON %q for accepted body %q", wire.String(), string(data))
			}
		}
		_, _ = r.WriteResultTo(io.Discard, true)
		sink.Info().Object("resp", r).Msg("parsed")

		clone, err := r.Clone()
		if err != nil {
			return
		}
		_, _ = clone.WriteTo(io.Discard)
		clone.Free()
		r.Free()
	})
}

// FuzzJsonRpcResponseFromBytes feeds the three response members separately.
// The whole-envelope target above spends most of its budget inside the JSON
// parser; this one reaches the id, result and error handling directly, which
// is also how the cache layer rebuilds a stored response.
func FuzzJsonRpcResponseFromBytes(f *testing.F) {
	seeds := []struct {
		id     string
		result string
		errRaw string
	}{
		{`1`, `"0x1a"`, ``},
		{`"abc"`, `{"number":"0x10","hash":"0xdead"}`, ``},
		{`null`, `null`, `{"code":-32000,"message":"header not found"}`},
		{`99999999999999999999`, `[1,2,3]`, ``},
		{`1.5`, `"0x0"`, `null`},
		{`{"weird":true}`, `{}`, ``},
		{``, ``, `{"error":"upstream is draining"}`},
	}
	for _, s := range seeds {
		f.Add([]byte(s.id), []byte(s.result), []byte(s.errRaw))
	}

	ctx := context.Background()
	sink := zerolog.New(io.Discard)

	f.Fuzz(func(t *testing.T, id, result, errRaw []byte) {
		r, err := NewJsonRpcResponseFromBytes(id, result, errRaw)
		if err != nil {
			return
		}
		_ = r.ID()
		_ = r.IsResultEmptyish(ctx)
		_, _ = r.PeekStringByPath(ctx, "number")
		_, _ = r.CanonicalHash(ctx)
		_, _ = r.WriteTo(io.Discard)
		sink.Info().Object("resp", r).Msg("built")

		if err := r.SetIDBytes(id); err != nil {
			return
		}
		_, _ = r.WriteTo(io.Discard)
	})
}

// FuzzJsonRpcResponseParseError drives the error member of an upstream
// response through ParseError, which walks four different error shapes before
// falling back to "treat the raw data as the message".
func FuzzJsonRpcResponseParseError(f *testing.F) {
	seeds := []string{
		`{"code":-32000,"message":"header not found"}`,
		`{"code":-32601}`,
		`{"message":"rate limited"}`,
		`{"data":"execution reverted"}`,
		`{"error":"upstream is draining"}`,
		`{"code":-32000,"message":"header not found","vendorHint":"try archive node"}`,
		`{"code":3,"message":"execution reverted","data":{"reason":"x"}}`,
		`null`,
		``,
		`"plain string"`,
		`[1,2,3]`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	sink := zerolog.New(io.Discard)

	f.Fuzz(func(t *testing.T, raw string) {
		r := &JsonRpcResponse{}
		if err := r.ParseError(raw); err != nil {
			return
		}
		if r.Error == nil {
			t.Fatalf("ParseError returned nil error for %q with no failure", raw)
		}
		_ = r.Error.Error()
		_ = r.Error.CodeChain()
		_ = r.Error.DeepestMessage()
		_, _ = r.WriteTo(io.Discard)
		sink.Info().Object("resp", r).Msg("parsed error")
	})
}

// FuzzJsonRpcRequestUnmarshal drives every byte a client can post through the
// request parser and the derived-value paths (cache hash, clone, peek).
func FuzzJsonRpcRequestUnmarshal(f *testing.F) {
	seeds := []string{
		`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1b4",true]}`,
		`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x2","address":"0xabc"}]}`,
		`{"jsonrpc":"2.0","id":"str-id","method":"eth_call","params":[{"to":"0x1","data":"0x2"},"latest"]}`,
		`{"jsonrpc":"2.0","id":9007199254740993,"method":"eth_chainId","params":[]}`,
		`{"jsonrpc":"2.0","id":1.5,"method":"eth_chainId"}`,
		`{"jsonrpc":"2.0","id":null,"method":"eth_chainId","params":null}`,
		`{"method":"eth_getBalance","params":["0xabc",{"blockHash":"0xdead"}]}`,
		`{}`,
		`[]`,
		``,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	sink := zerolog.New(io.Discard)

	f.Fuzz(func(t *testing.T, data []byte) {
		r := &JsonRpcRequest{}
		if err := r.UnmarshalJSON(data); err != nil {
			return
		}

		_ = r.IDRawBytes()
		_, _ = r.CacheHash()
		r.InvalidateCacheHash()
		_, _ = r.CacheHash()
		_, _ = r.PeekByPath(0)
		_, _ = r.PeekByPath(0, "blockHash")
		_, _ = r.PeekByPath(1)
		sink.Info().Object("req", r).Msg("parsed request")

		clone := r.Clone()
		if clone == nil {
			t.Fatal("Clone returned nil for a successfully parsed request")
		}
		_, _ = clone.CacheHash()

		// A NormalizedRequest is what the HTTP server actually builds per batch
		// entry, so run the same bytes through it too.
		nr := NewNormalizedRequest(data)
		_, _ = nr.Method()
		_, _ = nr.JsonRpcRequest()
	})
}

// FuzzRemoveFieldsByPaths drives the consensus ignore-fields path: an
// arbitrary upstream result plus an operator-supplied list of dotted paths.
func FuzzRemoveFieldsByPaths(f *testing.F) {
	type seed struct {
		body  string
		paths string
	}
	seeds := []seed{
		{`{"status":"0x1","logs":[{"blockTimestamp":"0x1","data":"0x2"}]}`, "status,logs.*.blockTimestamp"},
		{`{"receipt":{"status":"0x1","gasUsed":"0x5"}}`, "receipt.status"},
		{`{"a":{"b":{"c":1}}}`, "a,a.b.c"},
		{`{"a":{"b":{"c":1}}}`, "a.b.c,a"},
		{`[{"x":1},{"x":2}]`, "*.x"},
		{`{"a":1}`, ""},
		{`{"a":1}`, "."},
		{`{"a":1}`, "...."},
		{`{"a":1}`, "*"},
		{`{"a":1}`, "*.*.*"},
	}
	for _, s := range seeds {
		f.Add([]byte(s.body), s.paths)
	}

	f.Fuzz(func(t *testing.T, body []byte, paths string) {
		var obj interface{}
		if err := SonicCfg.Unmarshal(body, &obj); err != nil {
			return
		}
		var list []string
		if paths != "" {
			list = strings.Split(paths, ",")
		}
		out := removeFieldsByPaths(obj, list)
		var buf bytes.Buffer
		_, _ = canonicalizeTo(&buf, out)
	})
}
