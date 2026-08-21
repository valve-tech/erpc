package evm

import (
	"bytes"
	"context"
	"testing"

	"github.com/erpc/erpc/common"
)

// The seeds are REAL request bodies taken from the tests in this package
// (block_ref_test.go, eth_getLogs_test.go, eth_blockNumber_test.go) plus the
// composite block-parameter shapes EIP-1898 allows.
var fuzzBlockRefRequestSeeds = []string{
	`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1b4",false]}`,
	`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`,
	`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["finalized",true]}`,
	`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`,
	`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x2","address":"0xabc"}]}`,
	`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc","latest"]}`,
	`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc",{"blockHash":"0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}]}`,
	`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc",{"blockNumber":"0x1b4"}]}`,
	`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0xabc",{"blockTag":"safe"}]}`,
	`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xdead"]}`,
	`{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}`,
	`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":"0x1","data":"0x2"},"0xffffffffffffffffff"]}`,
	`{"jsonrpc":"2.0","id":1,"method":"unknown_method","params":[1,2,3]}`,
}

// FuzzExtractBlockReferenceFromRequest drives an arbitrary client request body
// through block-reference extraction: method-config lookup, params peeking,
// composite block-parameter parsing and hex decoding.
func FuzzExtractBlockReferenceFromRequest(f *testing.F) {
	for _, s := range fuzzBlockRefRequestSeeds {
		f.Add([]byte(s))
	}

	ctx := context.Background()

	f.Fuzz(func(t *testing.T, body []byte) {
		req := common.NewNormalizedRequest(body)
		if _, err := req.JsonRpcRequest(ctx); err != nil {
			return
		}
		_, _, _ = ExtractBlockReferenceFromRequest(ctx, req)
	})
}

// FuzzExtractBlockReferenceFromResponse drives an arbitrary upstream response
// body through the response side of block-reference extraction, paired with a
// real request so the method config resolves.
func FuzzExtractBlockReferenceFromResponse(f *testing.F) {
	seeds := []struct {
		req  string
		resp string
	}{
		{
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`,
			`{"jsonrpc":"2.0","id":1,"result":{"number":"0x1b4","hash":"0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef","timestamp":"0x64"}}`,
		},
		{
			`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionReceipt","params":["0xdead"]}`,
			`{"jsonrpc":"2.0","id":1,"result":{"blockNumber":"0x1b4","status":"0x1"}}`,
		},
		{
			`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`,
			`{"jsonrpc":"2.0","id":1,"result":"0x1b4"}`,
		},
		{
			`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["latest",false]}`,
			`{"jsonrpc":"2.0","id":1,"result":null}`,
		},
	}
	for _, s := range seeds {
		f.Add([]byte(s.req), []byte(s.resp))
	}

	ctx := context.Background()

	f.Fuzz(func(t *testing.T, reqBody, respBody []byte) {
		req := common.NewNormalizedRequest(reqBody)
		if _, err := req.JsonRpcRequest(ctx); err != nil {
			return
		}

		jrr := &common.JsonRpcResponse{}
		if err := jrr.ParseFromStream(nil, bytes.NewReader(respBody), len(respBody)); err != nil {
			return
		}

		resp := common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
		_, _, _ = ExtractBlockReferenceFromResponse(ctx, resp)
		_, _ = ExtractBlockTimestampFromResponse(ctx, resp)
		_, _, _ = ResolveCacheBlockRef(ctx, req, resp)
	})
}

// FuzzParseCompositeBlockParam drives a single block parameter — the value a
// client puts in params[N] — through the composite parser and the hex decoder
// behind it.
func FuzzParseCompositeBlockParam(f *testing.F) {
	seeds := []string{
		`"0x1b4"`,
		`"latest"`,
		`"earliest"`,
		`"pending"`,
		`"0x"`,
		`"0x0"`,
		`"0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"`,
		`123`,
		`-1`,
		`1e308`,
		`{"blockHash":"0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}`,
		`{"blockNumber":"0x1b4"}`,
		`{"blockTag":"safe"}`,
		`{}`,
		`null`,
		`[1,2]`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, raw []byte) {
		var param interface{}
		if err := common.SonicCfg.Unmarshal(raw, &param); err != nil {
			return
		}
		_, _, _ = parseCompositeBlockParam(param)
	})
}
