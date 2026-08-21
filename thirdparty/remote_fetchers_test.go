package thirdparty

import (
	"bytes"
	"context"
	"fmt"
	"io"
	stdlog "log"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
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
	// The read must fail on its own. If the fetcher ignored the read error and
	// let the partial bytes reach the parser, the operator would be told the
	// vendor sent bad JSON rather than that the connection dropped.
	assert.Contains(t, err.Error(), "unexpected EOF")
	assert.NotContains(t, err.Error(), "failed to parse remote repository data")
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
		{"id":"e","chainId":"10","httpEndpoint":""},
		{"id":"f","chainId":"99999999999999999999","httpEndpoint":"https://overflow.example"}
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
	// A chain ID too large for int64 parses to MaxInt64 with a range error.
	// Only the error check keeps it out; the positive-value check would let
	// it through and register a phantom chain.
	assert.NotContains(t, data, int64(math.MaxInt64), "an out-of-range chain ID must be skipped, not clamped")
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

	v.fetchChainIDs(context.Background(), &logger, []*ChainstackNode{node})

	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, int64(8453), node.ChainID, "0x2105 must decode as base 16")
}

func TestChainstackFetchChainIDs_SkipsNodesThatAreNotRunning(t *testing.T) {
	srv, hits := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()
	stopped := &ChainstackNode{ID: "n1", Status: "stopped", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL}}
	noEndpoint := &ChainstackNode{ID: "n2", Status: "running"}

	v.fetchChainIDs(context.Background(), &logger, []*ChainstackNode{stopped, noEndpoint})

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
	v.fetchChainIDs(context.Background(), &logger, nodes)

	// A failed probe costs that node its chain ID and nothing more. The
	// function reports the failures on its own logger; it has no error to
	// return, because a partial failure must never discard the good nodes.
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
	v.fetchChainIDs(context.Background(), &logger, []*ChainstackNode{badNode, goodNode})

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
	v.fetchChainIDs(ctx, &logger, []*ChainstackNode{node})

	assert.Equal(t, int32(0), hits.Load(), "a cancelled context must not reach the network")
	assert.Equal(t, int64(0), node.ChainID)
}

func TestQuicknodeFetchChainIDs_FillsTheChainIdFromTheProbe(t *testing.T) {
	srv, hits := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0x2105"}`)
	v := CreateQuicknodeVendor().(*QuicknodeVendor)
	logger := zerolog.Nop()
	ep := &QuicknodeEndpoint{ID: "e1", HttpUrl: srv.URL}

	v.fetchChainIDs(context.Background(), &logger, []*QuicknodeEndpoint{ep})

	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, int64(8453), ep.ChainID)
}

// A JSON-RPC response that carries both an error and a result is malformed,
// but vendors do send it. The error must win: trusting the result would route
// traffic to a node whose own answer says the call failed.
func TestChainIdProbe_AnErrorBesideAResultMustWin(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":"0x1","error":{"code":-32000,"message":"node syncing"}}`
	logger := zerolog.Nop()

	csSrv, _ := chainIdServer(t, body)
	cs := CreateChainstackVendor().(*ChainstackVendor)
	node := &ChainstackNode{ID: "n1", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: csSrv.URL}}
	cs.fetchChainIDs(context.Background(), &logger, []*ChainstackNode{node})
	assert.Equal(t, int64(0), node.ChainID, "chainstack must not accept a result that came with an error")

	qnSrv, _ := chainIdServer(t, body)
	qn := CreateQuicknodeVendor().(*QuicknodeVendor)
	ep := &QuicknodeEndpoint{ID: "e1", HttpUrl: qnSrv.URL}
	qn.fetchChainIDs(context.Background(), &logger, []*QuicknodeEndpoint{ep})
	assert.Equal(t, int64(0), ep.ChainID, "quicknode must not accept a result that came with an error")
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
	v.fetchChainIDs(context.Background(), &logger, eps)

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
	v.fetchChainIDs(context.Background(), &logger, []*QuicknodeEndpoint{badEp, goodEp})

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

	v.fetchChainIDs(context.Background(), &logger, []*QuicknodeEndpoint{ep})

	assert.Equal(t, int64(16), ep.ChainID, "0x10 is 16, not 10")
	assert.NotEqual(t, int64(10), ep.ChainID)
}

// fetchChainIDs must not offer an error result. A probe failure costs one node
// its chain ID and nothing else, so there is no failure for a caller to act on.
// An error result would invite a caller to discard the nodes that did answer.
// These declarations fail to compile if either signature grows one back.
var (
	_ func(context.Context, *zerolog.Logger, []*ChainstackNode)    = (&ChainstackVendor{}).fetchChainIDs
	_ func(context.Context, *zerolog.Logger, []*QuicknodeEndpoint) = (&QuicknodeVendor{}).fetchChainIDs
)

// fetchChainIDs reports every probe failure on its own logger, and it names the
// node that failed. That log line is the operator's whole signal, so it must
// survive. The function returns nothing: a partial failure is normal and the
// caller must keep the nodes that did answer.
func TestFetchChainIDs_ReportsEveryFailureOnItsOwnLogger(t *testing.T) {
	good, _ := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0xa"}`)
	bad, _ := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"node syncing"}}`)

	t.Run("chainstack", func(t *testing.T) {
		var buf bytes.Buffer
		logger := zerolog.New(&buf)
		v := CreateChainstackVendor().(*ChainstackVendor)
		goodNode := &ChainstackNode{ID: "good", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: good.URL}}
		badNode := &ChainstackNode{ID: "bad-node", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: bad.URL}}

		v.fetchChainIDs(context.Background(), &logger, []*ChainstackNode{goodNode, badNode})

		assert.Contains(t, buf.String(), "bad-node", "the warning must name the node that failed")
		assert.Contains(t, buf.String(), `"level":"warn"`)
		assert.Equal(t, int64(10), goodNode.ChainID, "the healthy node keeps its chain ID")
	})

	t.Run("quicknode", func(t *testing.T) {
		var buf bytes.Buffer
		logger := zerolog.New(&buf)
		v := CreateQuicknodeVendor().(*QuicknodeVendor)
		goodEp := &QuicknodeEndpoint{ID: "good", HttpUrl: good.URL}
		badEp := &QuicknodeEndpoint{ID: "bad-endpoint", HttpUrl: bad.URL}

		v.fetchChainIDs(context.Background(), &logger, []*QuicknodeEndpoint{goodEp, badEp})

		assert.Contains(t, buf.String(), "bad-endpoint", "the warning must name the endpoint that failed")
		assert.Contains(t, buf.String(), `"level":"warn"`)
		assert.Equal(t, int64(10), goodEp.ChainID, "the healthy endpoint keeps its chain ID")
	})
}

// A run in which every probe succeeds must stay silent.
func TestFetchChainIDs_LogsNothingWhenEveryProbeSucceeds(t *testing.T) {
	srv, _ := chainIdServer(t, `{"jsonrpc":"2.0","id":1,"result":"0xa"}`)
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	v := CreateChainstackVendor().(*ChainstackVendor)
	node := &ChainstackNode{ID: "n1", Status: "running", Details: ChainstackNodeDetails{HTTPSEndpoint: srv.URL}}

	v.fetchChainIDs(context.Background(), &logger, []*ChainstackNode{node})

	assert.Empty(t, buf.String(), "a clean run must not warn")
}

// -----------------------------------------------------------------------------
// chainstack: fetchNodes pagination
// -----------------------------------------------------------------------------

// pagedNodesServer serves `pages` pages of the Chainstack node listing and
// tracks how many connections it holds open at once.
func pagedNodesServer(t *testing.T, pages int) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	open, maxOpen := 0, 0
	var srv *httptest.Server

	srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page == 0 {
			page = 1
		}
		next := "null"
		if page < pages {
			next = fmt.Sprintf(`"%s/?page=%d"`, srv.URL, page+1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"next":%s,"results":[{"id":"n%d","status":"running","details":{"https_endpoint":"https://n%d.example"}}]}`,
			next, page, page)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Padding the decoder never reads. Go returns a connection to its pool
		// only after the body reaches EOF or the caller closes it, so this is
		// what turns an unclosed body into a held connection.
		_, _ = w.Write(bytes.Repeat([]byte("\n"), 64*1024))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch s {
		case http.StateNew:
			open++
			if open > maxOpen {
				maxOpen = open
			}
		case http.StateClosed, http.StateHijacked:
			open--
		}
	}
	// ConnState must be in place before the server starts serving.
	srv.Start()
	t.Cleanup(srv.Close)

	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return maxOpen
	}
}

// fetchNodes must release each page's body before it asks for the next one.
// Holding them all open costs one connection per page for the whole walk,
// which on a large account is a connection leak in everything but name.
func TestChainstackFetchNodes_ReleasesEachPageBeforeTheNextRequest(t *testing.T) {
	const pages = 8
	srv, maxOpen := pagedNodesServer(t, pages)
	prev := chainstackNodesApiUrl
	chainstackNodesApiUrl = srv.URL + "/"
	t.Cleanup(func() { chainstackNodesApiUrl = prev })

	v := CreateChainstackVendor().(*ChainstackVendor)
	logger := zerolog.Nop()

	nodes, err := v.fetchNodes(context.Background(), &logger, "k", nil)

	require.NoError(t, err)
	assert.Len(t, nodes, pages, "the walk must collect every page")
	// The bound is 2, not 1. The decoder stops before the padding, so Go
	// cannot return the connection to its pool and closes the socket instead.
	// The server's ConnState hook can see the next StateNew before it sees
	// that StateClosed, which shows up as a second connection. What matters
	// is that the count stays flat: leave the close to the end of the walk
	// and it becomes `pages`.
	assert.LessOrEqual(t, maxOpen(), 2,
		"page N's body must close before page N+1 is requested; deferring the close to the end of the walk holds one connection per page")
}
