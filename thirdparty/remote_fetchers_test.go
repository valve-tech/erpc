package thirdparty

import (
	"context"
	"io"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive the vendor remote fetchers over httptest. A fetcher that
// mishandles a 500, a truncated body or malformed JSON fails the same way for
// every vendor built on it, so the transport paths are covered before the
// per-vendor chain tables.

const fetcherTestWait = 5 * time.Second

// quietLogger keeps the deliberate connection abort out of the test output.
func quietLogger() *stdlog.Logger {
	return stdlog.New(io.Discard, "", 0)
}

// jsonServer answers every request with status and body, and counts the hits.
func jsonServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// truncatedServer promises more bytes than it sends, then drops the
// connection. The client sees the body end early.
func truncatedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"1":{"endpoints":["https://a`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	srv.Config.ErrorLog = quietLogger()
	t.Cleanup(srv.Close)
	return srv
}

// hangingServer never answers until the test finishes. Callers must supply a
// context deadline, which is the point of the test.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	// Cleanup runs last-registered-first, so the handler is released before
	// Close waits on it.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })
	return srv
}

// -----------------------------------------------------------------------------
// repository: fetchRemoteData
// -----------------------------------------------------------------------------

func TestFetchRemoteData_BuildsTheChainMapAndSkipsNonNumericKeys(t *testing.T) {
	srv, hits := jsonServer(t, http.StatusOK, `{
		"1":{"endpoints":["https://one.example","https://one-b.example"]},
		"137":{"endpoints":["https://poly.example"]},
		"mainnet":{"endpoints":["https://named.example"]}
	}`)

	data, err := fetchRemoteData(context.Background(), srv.URL)

	require.NoError(t, err)
	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, []string{"https://one.example", "https://one-b.example"}, data[1])
	assert.Equal(t, []string{"https://poly.example"}, data[137])
	assert.Len(t, data, 2, "a key that is not an integer must be dropped, not coerced")
}

func TestFetchRemoteData_ReportsTheStatusCodeOnANon2xx(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusInternalServerError, `{"1":{"endpoints":["https://one.example"]}}`)

	data, err := fetchRemoteData(context.Background(), srv.URL)

	require.Error(t, err)
	assert.Nil(t, data, "a 500 must not yield a partially built map")
	assert.Contains(t, err.Error(), "500", "the operator needs the status code in the message")
	assert.NotContains(t, err.Error(), "failed to parse", "a 500 must not be reported as a parse failure")
}

func TestFetchRemoteData_ATwoHundredWithAnHtmlErrorPageIsAParseFailure(t *testing.T) {
	// Captive portals and CDN error pages answer 200 with HTML.
	srv, _ := jsonServer(t, http.StatusOK, `<html><body>Bad Gateway</body></html>`)

	data, err := fetchRemoteData(context.Background(), srv.URL)

	require.Error(t, err)
	assert.Nil(t, data)
	assert.Contains(t, err.Error(), "failed to parse remote repository data")
}

func TestFetchRemoteData_ATruncatedBodyIsAnErrorNotAnEmptyMap(t *testing.T) {
	srv := truncatedServer(t)

	data, err := fetchRemoteData(context.Background(), srv.URL)

	require.Error(t, err, "a half-delivered body must never read as a successful empty fetch")
	assert.Nil(t, data)
}

func TestFetchRemoteData_HonoursTheCallersDeadline(t *testing.T) {
	srv := hangingServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	data, err := fetchRemoteData(ctx, srv.URL)

	require.Error(t, err)
	assert.Nil(t, data)
	assert.Less(t, time.Since(start), fetcherTestWait,
		"the caller's deadline must win over the fetcher's own 10s timeout")
	assert.Contains(t, err.Error(), "context deadline exceeded")
}

func TestFetchRemoteData_RejectsAnUnusableURL(t *testing.T) {
	data, err := fetchRemoteData(context.Background(), "://not-a-url")

	require.Error(t, err)
	assert.Nil(t, data)
}

// -----------------------------------------------------------------------------
// conduit: fetchConduitNetworks
// -----------------------------------------------------------------------------

func TestFetchConduitNetworks_KeepsOnlyUsableEntries(t *testing.T) {
	srv, hits := jsonServer(t, http.StatusOK, `{"endpoints":[
		{"id":"a","chainId":"8453","httpEndpoint":"https://base.example"},
		{"id":"b","chainId":"not-a-number","httpEndpoint":"https://bad.example"},
		{"id":"c","chainId":"0","httpEndpoint":"https://zero.example"},
		{"id":"d","chainId":"-5","httpEndpoint":"https://negative.example"},
		{"id":"e","chainId":"10","httpEndpoint":""}
	]}`)
	v := CreateConduitVendor().(*ConduitVendor)
	logger := zerolog.Nop()

	data, err := v.fetchConduitNetworks(context.Background(), &logger, srv.URL)

	require.NoError(t, err)
	assert.Equal(t, int32(1), hits.Load())
	require.Len(t, data, 1, "only the one entry with a positive chain ID and an endpoint survives")
	require.NotNil(t, data[8453])
	assert.Equal(t, "https://base.example", data[8453].HttpEndpoint)
	assert.Equal(t, "a", data[8453].ID)
}

func TestFetchConduitNetworks_ReportsTheStatusCodeOnANon2xx(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusForbidden, `{"endpoints":[]}`)
	v := CreateConduitVendor().(*ConduitVendor)
	logger := zerolog.Nop()

	data, err := v.fetchConduitNetworks(context.Background(), &logger, srv.URL)

	require.Error(t, err)
	assert.Nil(t, data)
	assert.Contains(t, err.Error(), "403")
	assert.NotContains(t, err.Error(), "failed to parse")
}

func TestFetchConduitNetworks_MalformedJsonIsAParseFailure(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusOK, `{"endpoints":[{"chainId":`)
	v := CreateConduitVendor().(*ConduitVendor)
	logger := zerolog.Nop()

	data, err := v.fetchConduitNetworks(context.Background(), &logger, srv.URL)

	require.Error(t, err)
	assert.Nil(t, data)
	assert.Contains(t, err.Error(), "failed to parse Conduit API data")
}

func TestFetchConduitNetworks_HonoursTheCallersDeadline(t *testing.T) {
	srv := hangingServer(t)
	v := CreateConduitVendor().(*ConduitVendor)
	logger := zerolog.Nop()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	data, err := v.fetchConduitNetworks(ctx, &logger, srv.URL)

	require.Error(t, err)
	assert.Nil(t, data)
	assert.Less(t, time.Since(start), fetcherTestWait)
	assert.Contains(t, err.Error(), "context deadline exceeded")
}

func TestFetchConduitNetworks_AnEmptyEndpointListSucceedsWithAnEmptyMap(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusOK, `{"endpoints":[]}`)
	v := CreateConduitVendor().(*ConduitVendor)
	logger := zerolog.Nop()

	data, err := v.fetchConduitNetworks(context.Background(), &logger, srv.URL)

	require.NoError(t, err)
	assert.NotNil(t, data, "an empty result must be an allocated map, not nil")
	assert.Empty(t, data)
}

// -----------------------------------------------------------------------------
// superchain: fetchSuperchainNetworks
// -----------------------------------------------------------------------------

func TestFetchSuperchainNetworks_KeepsOnlyEntriesWithAPositiveChainIdAndAnRpc(t *testing.T) {
	srv, hits := jsonServer(t, http.StatusOK, `[
		{"chainId":8453,"rpc":["https://base.example","https://base-b.example"]},
		{"chainId":0,"rpc":["https://zero.example"]},
		{"chainId":-1,"rpc":["https://negative.example"]},
		{"chainId":10,"rpc":[]}
	]`)
	v := CreateSuperchainVendor().(*SuperchainVendor)

	data, err := v.fetchSuperchainNetworks(context.Background(), srv.URL)

	require.NoError(t, err)
	assert.Equal(t, int32(1), hits.Load())
	require.Len(t, data, 1)
	assert.Equal(t, []string{"https://base.example", "https://base-b.example"}, data[8453])
}

func TestFetchSuperchainNetworks_ReportsTheStatusCodeOnANon2xx(t *testing.T) {
	srv, _ := jsonServer(t, http.StatusNotFound, `[]`)
	v := CreateSuperchainVendor().(*SuperchainVendor)

	data, err := v.fetchSuperchainNetworks(context.Background(), srv.URL)

	require.Error(t, err)
	assert.Nil(t, data)
	assert.Contains(t, err.Error(), "404")
	assert.NotContains(t, err.Error(), "failed to parse")
}

func TestFetchSuperchainNetworks_AJsonObjectWhereAListIsExpectedIsAParseFailure(t *testing.T) {
	// A registry that changes shape must fail loudly, not silently return
	// zero chains and make every network look unsupported.
	srv, _ := jsonServer(t, http.StatusOK, `{"chains":[{"chainId":8453,"rpc":["https://base.example"]}]}`)
	v := CreateSuperchainVendor().(*SuperchainVendor)

	data, err := v.fetchSuperchainNetworks(context.Background(), srv.URL)

	require.Error(t, err)
	assert.Nil(t, data)
	assert.Contains(t, err.Error(), "failed to parse Superchain registry data")
}

func TestFetchSuperchainNetworks_HonoursTheCallersDeadline(t *testing.T) {
	srv := hangingServer(t)
	v := CreateSuperchainVendor().(*SuperchainVendor)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	data, err := v.fetchSuperchainNetworks(ctx, srv.URL)

	require.Error(t, err)
	assert.Nil(t, data)
	assert.Less(t, time.Since(start), fetcherTestWait)
	assert.Contains(t, err.Error(), "context deadline exceeded")
}

// -----------------------------------------------------------------------------
// chain-ID probes: chainstack and quicknode both POST eth_chainId per endpoint
// -----------------------------------------------------------------------------

// chainIdServer answers eth_chainId with the given raw JSON-RPC body.
func chainIdServer(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestChainstackFetchChainIDs_FillsTheChainIdFromTheProbe(t *testing.T) {
	srv, hits := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0x2105"}`)
	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()
	node := &ChainstackNode{ID: "n1", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL, AuthKey: "k"}}

	err := v.fetchChainIDs(context.Background(), &logger, []*ChainstackNode{node})

	require.NoError(t, err)
	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, int64(8453), node.ChainID, "0x2105 must decode as base 16")
}

func TestChainstackFetchChainIDs_SkipsNodesThatAreNotRunning(t *testing.T) {
	srv, hits := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()
	stopped := &ChainstackNode{ID: "n1", Status: "stopped", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL}}
	noEndpoint := &ChainstackNode{ID: "n2", Status: "running"}

	err := v.fetchChainIDs(context.Background(), &logger, []*ChainstackNode{stopped, noEndpoint})

	require.NoError(t, err)
	assert.Equal(t, int32(0), hits.Load(), "a stopped node or one with no endpoint must not be probed")
	assert.Equal(t, int64(0), stopped.ChainID)
	assert.Equal(t, int64(0), noEndpoint.ChainID)
}

func TestChainstackFetchChainIDs_LeavesTheChainIdUnsetWhenTheProbeFails(t *testing.T) {
	rpcErr, _ := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)
	badHex, _ := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"mainnet"}`)
	garbage, _ := chainIdServer(t, `not json at all`)
	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()

	nodes := []*ChainstackNode{
		{ID: "rpc-error", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: rpcErr.URL}},
		{ID: "bad-hex", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: badHex.URL}},
		{ID: "garbage", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: garbage.URL}},
	}
	err := v.fetchChainIDs(context.Background(), &logger, nodes)

	// fetchChainIDs never returns an error, even when every probe fails; it
	// only logs. See the report for what an operator observes.
	assert.NoError(t, err)
	for _, n := range nodes {
		assert.Equal(t, int64(0), n.ChainID, "node %s must keep chain ID 0 after a failed probe", n.ID)
	}
}

func TestChainstackFetchChainIDs_OneBadNodeDoesNotStopTheGoodOne(t *testing.T) {
	good, _ := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0xa"}`)
	bad, _ := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"nope"}}`)
	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()

	goodNode := &ChainstackNode{ID: "good", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: good.URL}}
	badNode := &ChainstackNode{ID: "bad", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: bad.URL}}
	err := v.fetchChainIDs(context.Background(), &logger, []*ChainstackNode{badNode, goodNode})

	require.NoError(t, err)
	assert.Equal(t, int64(10), goodNode.ChainID, "a failing sibling must not cost the healthy node its chain ID")
	assert.Equal(t, int64(0), badNode.ChainID)
}

func TestChainstackFetchChainIDs_ACancelledContextStopsEveryProbe(t *testing.T) {
	srv, hits := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	node := &ChainstackNode{ID: "n1", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL}}
	err := v.fetchChainIDs(ctx, &logger, []*ChainstackNode{node})

	require.NoError(t, err)
	assert.Equal(t, int32(0), hits.Load(), "a cancelled context must not reach the network")
	assert.Equal(t, int64(0), node.ChainID)
}

func TestQuicknodeFetchChainIDs_FillsTheChainIdFromTheProbe(t *testing.T) {
	srv, hits := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0x2105"}`)
	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	logger := zerolog.Nop()
	ep := &QuicknodeEndpoint{ID: "e1", HttpUrl: srv.URL}

	err := v.fetchChainIDs(context.Background(), &logger, []*QuicknodeEndpoint{ep})

	require.NoError(t, err)
	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, int64(8453), ep.ChainID)
}

func TestQuicknodeFetchChainIDs_SkipsEndpointsWithNoHttpUrl(t *testing.T) {
	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	logger := zerolog.Nop()
	ep := &QuicknodeEndpoint{ID: "e1"}

	err := v.fetchChainIDs(context.Background(), &logger, []*QuicknodeEndpoint{ep})

	require.NoError(t, err)
	assert.Equal(t, int64(0), ep.ChainID)
}

func TestQuicknodeFetchChainIDs_LeavesTheChainIdUnsetWhenTheProbeFails(t *testing.T) {
	rpcErr, _ := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)
	badHex, _ := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0xzz"}`)
	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	logger := zerolog.Nop()

	eps := []*QuicknodeEndpoint{
		{ID: "rpc-error", HttpUrl: rpcErr.URL},
		{ID: "bad-hex", HttpUrl: badHex.URL},
	}
	err := v.fetchChainIDs(context.Background(), &logger, eps)

	assert.NoError(t, err, "fetchChainIDs swallows every probe failure")
	for _, e := range eps {
		assert.Equal(t, int64(0), e.ChainID, "endpoint %s must keep chain ID 0", e.ID)
	}
}

func TestQuicknodeFetchChainIDs_OneBadEndpointDoesNotStopTheGoodOne(t *testing.T) {
	good, _ := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0x89"}`)
	bad, _ := chainIdServer(t, `garbage`)
	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	logger := zerolog.Nop()

	goodEp := &QuicknodeEndpoint{ID: "good", HttpUrl: good.URL}
	badEp := &QuicknodeEndpoint{ID: "bad", HttpUrl: bad.URL}
	err := v.fetchChainIDs(context.Background(), &logger, []*QuicknodeEndpoint{badEp, goodEp})

	require.NoError(t, err)
	assert.Equal(t, int64(137), goodEp.ChainID)
	assert.Equal(t, int64(0), badEp.ChainID)
}

// chainIdStrconvGuard documents that the probe parses the hex payload rather
// than trusting a decimal string, which would silently misroute traffic.
func TestChainIdProbe_ParsesHexNotDecimal(t *testing.T) {
	srv, _ := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0x10"}`)
	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	logger := zerolog.Nop()
	ep := &QuicknodeEndpoint{ID: "e1", HttpUrl: srv.URL}

	require.NoError(t, v.fetchChainIDs(context.Background(), &logger, []*QuicknodeEndpoint{ep}))

	assert.Equal(t, int64(16), ep.ChainID, "0x10 is 16, not 10")
	assert.NotEqual(t, int64(10), ep.ChainID)
}
