package thirdparty

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
)

// FuzzVendorErrorFields drives the three fields a vendor normaliser actually
// reads — code, message and data — straight at every registered vendor. The
// whole-body target below spends most of its budget in the JSON parser; this
// one reaches the substring matchers each vendor keys on.
//
// The seeds are REAL codes and messages from vendor_errors_test.go.
func FuzzVendorErrorFields(f *testing.F) {
	seeds := []struct {
		status int
		code   int
		msg    string
		data   string
	}{
		{400, -32614, "range too wide", ""},
		{429, -32009, "too many requests", ""},
		{429, -32007, "out of credits", ""},
		{200, 3, "execution reverted", "0x08c379a0"},
		{200, -32000, "header not found", ""},
		{401, -32600, "token is invalid", ""},
		{400, -32602, "invalid block range", ""},
		{500, -32603, "ChainException: Unexpected error (code=40000)", ""},
		{429, 429, "Your app has exceeded its compute units per second capacity", ""},
		{200, -32001, "Resource not found", "archive"},
		{503, 0, "", ""},
	}
	for _, s := range seeds {
		f.Add(s.status, s.code, s.msg, s.data)
	}

	registry := NewVendorsRegistry()
	vendors := make([]common.Vendor, 0, len(registry.SupportedVendors()))
	for _, name := range registry.SupportedVendors() {
		vendors = append(vendors, registry.LookupByName(name))
	}

	f.Fuzz(func(t *testing.T, status, code int, msg, data string) {
		jrr, err := common.NewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(code, msg, data))
		if err != nil {
			return
		}

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`))
		req.SetLastUpstream(common.NewFakeUpstream("fuzz-upstream"))
		resp := &http.Response{StatusCode: status, Header: http.Header{}}

		for _, v := range vendors {
			details := map[string]interface{}{"statusCode": status}
			err := v.GetVendorSpecificErrorIfAny(req, resp, jrr, details)
			if err == nil {
				continue
			}
			_ = err.Error()
			_ = common.IsRetryableTowardNetwork(err)
			_ = common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded)
			_ = common.ErrorSummary(err)
		}
	})
}

// FuzzVendorErrorNormalisation drives an arbitrary upstream error body through
// EVERY registered vendor normaliser. The body arrives from a server eRPC does
// not control, so each normaliser has to survive any shape of it.
//
// The seeds are REAL error bodies taken from the vendor tests in this package
// (vendor_errors_test.go, quicknode_test.go, drpc_test.go, alchemy_test.go,
// blockdaemon_test.go, chainstack_test.go, conduit_test.go, goldsky_test.go).
func FuzzVendorErrorNormalisation(f *testing.F) {
	seeds := []struct {
		body   string
		status int
	}{
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32614,"message":"range too wide"}}`, 400},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32009,"message":"too many requests"}}`, 429},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32007,"message":"out of credits"}}`, 429},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":3,"message":"execution reverted","data":"0x08c379a0"}}`, 200},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"header not found"}}`, 200},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"token is invalid"}}`, 401},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid block range"}}`, 400},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"ChainException: Unexpected error (code=40000)"}}`, 500},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"Resource not found","data":{"reason":"archive"}}}`, 404},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":429,"message":"Your app has exceeded its compute units per second capacity"}}`, 429},
		{`{"jsonrpc":"2.0","id":1,"error":"plain string error"}`, 500},
		{`{"error":"upstream is draining"}`, 503},
		{`<html>502 Bad Gateway</html>`, 502},
	}
	for _, s := range seeds {
		f.Add([]byte(s.body), s.status)
	}

	registry := NewVendorsRegistry()
	vendors := make([]common.Vendor, 0, len(registry.SupportedVendors()))
	for _, name := range registry.SupportedVendors() {
		vendors = append(vendors, registry.LookupByName(name))
	}

	f.Fuzz(func(t *testing.T, body []byte, status int) {
		jrr := &common.JsonRpcResponse{}
		if err := jrr.ParseFromStream(nil, bytes.NewReader(body), len(body)); err != nil {
			return
		}
		// ExtractJsonRpcError only reaches a vendor when the response carries a
		// parsed error, so keep the same guard here — anything found below is
		// therefore reachable from a real upstream reply.
		if jrr.Error == nil {
			return
		}

		req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x2"}]}`))
		req.SetLastUpstream(common.NewFakeUpstream("fuzz-upstream"))
		resp := &http.Response{StatusCode: status, Header: http.Header{}}

		for _, v := range vendors {
			details := map[string]interface{}{"statusCode": status}
			err := v.GetVendorSpecificErrorIfAny(req, resp, jrr, details)
			if err == nil {
				continue
			}
			// The router asks these questions of whatever the vendor returns.
			_ = err.Error()
			_ = common.IsRetryableTowardNetwork(err)
			_ = common.HasErrorCode(err, common.ErrCodeEndpointCapacityExceeded)
			_ = common.ErrorSummary(err)
		}
	})
}
