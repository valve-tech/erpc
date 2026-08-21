package evm

import (
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
)

// FuzzExtractJsonRpcError drives an arbitrary upstream complaint through the
// EVM error classifier — the long if/else chain that decides whether eRPC
// retries on a sibling upstream, cordons the endpoint, or gives up.
//
// The status code, JSON-RPC code and message all come from a server eRPC does
// not control. The seeds are REAL rows from
// error_normalizer_classification_test.go.
func FuzzExtractJsonRpcError(f *testing.F) {
	seeds := []struct {
		status int
		code   int
		msg    string
		method string
	}{
		{429, -32005, "rate limit exceeded", "eth_call"},
		{200, -32000, "header not found", "eth_getBlockByNumber"},
		{200, 3, "execution reverted", "eth_call"},
		{401, -32000, "unauthorized", "eth_blockNumber"},
		{400, -32602, "invalid argument 0", "eth_getLogs"},
		{413, -32000, "query returned more than 10000 results", "eth_getLogs"},
		{503, -32603, "internal error", "eth_chainId"},
		{200, -32601, "the method eth_traceBlock does not exist", "eth_traceBlock"},
		{500, 0, "", ""},
		{200, -32000, "missing trie node", "eth_getBalance"},
	}
	for _, s := range seeds {
		f.Add(s.status, s.code, s.msg, s.method)
	}

	f.Fuzz(func(t *testing.T, status, code int, msg, method string) {
		var nr *common.NormalizedResponse
		if method != "" {
			req := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":1,"params":[],"method":` + quoteJSON(method) + `}`))
			nr = common.NewNormalizedResponse().WithRequest(req)
		}

		jr, err := common.NewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(code, msg, ""))
		if err != nil {
			return
		}

		resp := &http.Response{StatusCode: status, Header: http.Header{}}
		out := ExtractJsonRpcError(resp, nr, jr, common.NewFakeUpstream("fuzz-upstream"))
		if out == nil {
			return
		}
		_ = out.Error()
		_ = common.IsRetryableTowardNetwork(out)
		_ = common.ErrorSummary(out)
	})
}

func quoteJSON(s string) string {
	b, err := common.SonicCfg.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
