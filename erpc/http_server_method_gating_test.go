package erpc

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A project can name three lists in its config: which methods it refuses
// (ignoreMethods), which methods it re-admits (allowMethods), and which client
// headers it passes to the upstream (forwardHeaders). All three live only in
// the HTTP request handler, and all three fail silently when they break. An
// ignore list that stops matching turns a method an operator withdrew back on;
// an allow list that stops matching withdraws a method the operator sells; a
// forward list that stops matching drops the header a provider bills or
// authenticates on, and the request still succeeds.
//
// Each test below asserts the effect an operator can see: the answer the client
// gets, and whether the upstream was called at all.

// gatingCfg builds a one-upstream project and lets the caller set the three
// lists. rpc1.localhost is the host the shared state-poller mocks answer on.
func gatingCfg(tune func(*common.ProjectConfig)) *common.Config {
	prj := &common.ProjectConfig{
		Id: "test_project",
		Networks: []*common.NetworkConfig{
			{
				Architecture: common.ArchitectureEvm,
				Evm:          &common.EvmNetworkConfig{ChainId: 123},
				Failsafe:     []*common.FailsafeConfig{{}},
			},
		},
		Upstreams: []*common.UpstreamConfig{
			{
				Id:       "rpc1",
				Type:     common.UpstreamTypeEvm,
				Endpoint: "http://rpc1.localhost",
				Evm:      &common.EvmUpstreamConfig{ChainId: 123},
				Failsafe: []*common.FailsafeConfig{{}},
			},
		},
	}
	if tune != nil {
		tune(prj)
	}
	return &common.Config{
		Server: &common.ServerConfig{
			MaxTimeout: common.Duration(10 * time.Second).Ptr(),
		},
		Projects:     []*common.ProjectConfig{prj},
		RateLimiters: &common.RateLimiterConfig{},
	}
}

// jsonRpcError reads the error object out of a JSON-RPC response body.
func gatingJsonRpcError(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var obj map[string]interface{}
	require.NoError(t, sonic.UnmarshalString(body, &obj), "response body is not JSON: %s", body)
	errObj, ok := obj["error"].(map[string]interface{})
	require.True(t, ok, "expected a JSON-RPC error, got: %s", body)
	return errObj
}

// TestHttpServer_IgnoreMethodsRefusesTheMethodWithoutCallingAnUpstream is the
// withdrawal case. The operator removed eth_getBalance from what this project
// serves, so the client must be told the method is not supported and the
// upstream must never see the call. The upstream mock stays pending, which is
// the proof no request left eRPC — a test that only read the error body would
// still pass if the request had been forwarded and the answer thrown away.
func TestHttpServer_IgnoreMethodsRefusesTheMethodWithoutCallingAnUpstream(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	// One user mock must remain pending: the eth_getBalance reply nobody asked for.
	defer util.AssertNoPendingMocks(t, 1)

	gock.New("http://rpc1.localhost").
		Post("/").
		Times(1).
		Filter(func(request *http.Request) bool {
			return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xdeadbeef"})

	cfg := gatingCfg(func(p *common.ProjectConfig) {
		p.IgnoreMethods = []string{"eth_getBalance"}
	})
	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	statusCode, _, body := sendRequest(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0","latest"]}`, nil, nil)

	assert.Equal(t, http.StatusOK, statusCode,
		"a refused method is a JSON-RPC error, not a transport failure")
	errObj := gatingJsonRpcError(t, body)
	assert.Equal(t, float64(-32601), errObj["code"])
	assert.Equal(t, "method not supported: eth_getBalance", errObj["message"])
}

// TestHttpServer_IgnoreMethodsLeavesEveryOtherMethodAlone is the control for
// the test above. Without it, an ignore list that matched everything would look
// identical: both tests would see "method not supported" and pass.
func TestHttpServer_IgnoreMethodsLeavesEveryOtherMethodAlone(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	gock.New("http://rpc1.localhost").
		Post("/").
		Times(1).
		Filter(func(request *http.Request) bool {
			return strings.Contains(util.SafeReadBody(request), "eth_getTransactionCount")
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x7"})

	cfg := gatingCfg(func(p *common.ProjectConfig) {
		p.IgnoreMethods = []string{"eth_getBalance"}
	})
	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	statusCode, _, body := sendRequest(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionCount","params":["0x0","latest"]}`, nil, nil)

	assert.Equal(t, http.StatusOK, statusCode)
	assert.Contains(t, body, "0x7",
		"a method outside the ignore list must be served by the upstream")
	assert.NotContains(t, body, "method not supported")
}

// TestHttpServer_AllowMethodsReadmitsAMethodTheIgnoreListMatched pins the order
// of the two lists. eRPC evaluates ignoreMethods first and allowMethods second,
// so an allow entry wins over a broad ignore entry. An operator writes
// `ignoreMethods: ["*"]` plus a short allow list to run a deny-by-default
// project; if the order flipped, that project would serve nothing.
func TestHttpServer_AllowMethodsReadmitsAMethodTheIgnoreListMatched(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	gock.New("http://rpc1.localhost").
		Post("/").
		Times(1).
		Filter(func(request *http.Request) bool {
			return strings.Contains(util.SafeReadBody(request), "eth_getBalance")
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0xabc"})

	cfg := gatingCfg(func(p *common.ProjectConfig) {
		p.IgnoreMethods = []string{"*"}
		p.AllowMethods = []string{"eth_getBalance"}
	})
	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	statusCode, _, body := sendRequest(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0","latest"]}`, nil, nil)

	assert.Equal(t, http.StatusOK, statusCode)
	assert.Contains(t, body, "0xabc",
		"allowMethods must re-admit a method that ignoreMethods matched")
	assert.NotContains(t, body, "method not supported")
}

// TestHttpServer_AllowMethodsStillRefusesWhatItDoesNotName is the other half of
// the deny-by-default project. With `ignoreMethods: ["*"]` and one allow entry,
// every method outside that entry must stay refused. Without this case, an
// allowMethods loop that set shouldHandleMethod = true unconditionally would
// pass the test above and open the whole project.
func TestHttpServer_AllowMethodsStillRefusesWhatItDoesNotName(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 1)

	gock.New("http://rpc1.localhost").
		Post("/").
		Times(1).
		Filter(func(request *http.Request) bool {
			return strings.Contains(util.SafeReadBody(request), "eth_getTransactionCount")
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x9"})

	cfg := gatingCfg(func(p *common.ProjectConfig) {
		p.IgnoreMethods = []string{"*"}
		p.AllowMethods = []string{"eth_getBalance"}
	})
	sendRequest, _, _, shutdown, _ := createServerTestFixtures(cfg, t)
	defer shutdown()

	statusCode, _, body := sendRequest(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getTransactionCount","params":["0x0","latest"]}`, nil, nil)

	assert.Equal(t, http.StatusOK, statusCode)
	errObj := gatingJsonRpcError(t, body)
	assert.Equal(t, float64(-32601), errObj["code"])
	assert.Equal(t, "method not supported: eth_getTransactionCount", errObj["message"])
}

// TestHttpServer_ForwardHeadersSendsOnlyTheHeadersTheProjectNamed covers the
// third list. A provider often bills, authenticates or routes on a header the
// client sets, so a named header has to arrive at the upstream verbatim. The
// unnamed header is the half that matters for a leak: eRPC must not hand the
// upstream a client header the operator did not list.
func TestHttpServer_ForwardHeadersSendsOnlyTheHeadersTheProjectNamed(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	util.SetupMocksForEvmStatePoller()
	defer util.AssertNoPendingMocks(t, 0)

	var mu sync.Mutex
	var sawForwarded, sawSecret string
	gock.New("http://rpc1.localhost").
		Post("/").
		Times(1).
		Filter(func(request *http.Request) bool {
			if !strings.Contains(util.SafeReadBody(request), "eth_getBalance") {
				return false
			}
			mu.Lock()
			sawForwarded = request.Header.Get("X-Forward-Me")
			sawSecret = request.Header.Get("X-Keep-Me-Private")
			mu.Unlock()
			return true
		}).
		Reply(200).
		JSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": "0x1"})

	cfg := gatingCfg(func(p *common.ProjectConfig) {
		p.ForwardHeaders = []string{"X-Forward-Me"}
	})
	sendRequest, _, _, shutdown, shutdownInstance := createServerTestFixtures(cfg, t)
	_ = shutdownInstance
	defer shutdown()

	statusCode, _, body := sendRequest(
		`{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x0","latest"]}`,
		map[string]string{
			"X-Forward-Me":      "carried",
			"X-Keep-Me-Private": "secret",
		}, nil)

	require.Equal(t, http.StatusOK, statusCode)
	require.Contains(t, body, "0x1", "the request must reach the upstream")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "carried", sawForwarded,
		"a header named in forwardHeaders must reach the upstream")
	assert.Empty(t, sawSecret,
		"a header the project did not name must not reach the upstream")
}
