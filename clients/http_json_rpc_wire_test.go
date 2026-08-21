package clients

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

// fakeNode is a real HTTP server standing in for an upstream. These tests use
// a live server rather than a transport mock because the behaviour under test
// is on the wire: what eRPC sends (gzip framing, headers) and what it makes of
// what comes back (gzip decoding, the JSON-RPC 1.0 `"error": null` envelope).
type fakeNode struct {
	*httptest.Server
	// closeOnce guards Close: httptest.Server.Close blocks on a second call,
	// and a test that closes the server early would otherwise collide with the
	// cleanup close.
	closeOnce sync.Once

	mu       sync.Mutex
	gotBody  []byte
	gotHdr   http.Header
	handler  func(w http.ResponseWriter, r *http.Request)
	requests int
}

func newFakeNode(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *fakeNode {
	t.Helper()
	n := &fakeNode{handler: handler}
	n.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n.mu.Lock()
		n.gotBody = body
		n.gotHdr = r.Header.Clone()
		n.requests++
		n.mu.Unlock()
		n.handler(w, r)
	}))
	t.Cleanup(n.closeSafely)
	return n
}

func (n *fakeNode) closeSafely() {
	n.closeOnce.Do(func() {
		if n.Server != nil {
			n.Server.Close()
		}
	})
}

func (n *fakeNode) snapshot() ([]byte, http.Header, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.gotBody, n.gotHdr, n.requests
}

// jsonOK replies with a fixed body and a 200.
func jsonOK(body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}
}

// httpClientFor builds a GenericHttpJsonRpcClient pointed at the node. cfg may
// be nil for the plain single-request client.
func httpClientFor(t *testing.T, node *fakeNode, cfg *common.JsonRpcUpstreamConfig) *GenericHttpJsonRpcClient {
	t.Helper()
	util.ResetGock()
	t.Cleanup(util.ResetGock)

	lg := zerolog.Nop()
	u, err := url.Parse(node.URL)
	if err != nil {
		t.Fatalf("parse node url: %v", err)
	}
	ups := common.NewFakeUpstream("rpc-wire")
	ups.Config().Type = common.UpstreamTypeEvm
	ups.Config().Endpoint = node.URL
	ups.Config().JsonRpc = cfg

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	c, err := NewGenericHttpJsonRpcClient(ctx, &lg, "prj1", ups, u, cfg, nil, &noopErrorExtractor{})
	if err != nil {
		t.Fatalf("NewGenericHttpJsonRpcClient: %v", err)
	}
	return c.(*GenericHttpJsonRpcClient)
}

func newBlockNumberRequest() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
}

// A JSON-RPC 1.0 server (bitcoind, and many 2.0 servers too) sends
// `"error": null` on EVERY success. eRPC used to read the four bytes `null` as
// a failure, so every successful call became a ServerSideException and the
// request exhausted the whole upstream pool. This is the single-request half of
// that guard.
func TestSingleRequest_ExplicitNullErrorIsASuccessNotAFailure(t *testing.T) {
	node := newFakeNode(t, jsonOK(`{"result":"0x2a","error":null,"id":1}`))
	c := httpClientFor(t, node, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.SendRequest(ctx, newBlockNumberRequest())
	if err != nil {
		t.Fatalf(`"error": null was read as a failure: %v`, err)
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Fatalf("JsonRpcResponse: %v", err)
	}
	if jrr.Error != nil {
		t.Fatalf("response carries error %v; a null error member means no error", jrr.Error)
	}
	if got := string(jrr.GetResultBytes()); got != `"0x2a"` {
		t.Fatalf("result = %s, want \"0x2a\"", got)
	}
}

// The batch path parses the envelope through a different function
// (getJsonRpcResponseFromNode), so the `"error": null` rule has to hold there
// too or batching silently reintroduces the same outage.
func TestBatchResponse_ExplicitNullErrorIsASuccessNotAFailure(t *testing.T) {
	node := newFakeNode(t, jsonOK(`[{"result":"0x2a","error":null,"id":1}]`))
	c := httpClientFor(t, node, &common.JsonRpcUpstreamConfig{
		SupportsBatch: &common.TRUE,
		BatchMaxSize:  1,
		BatchMaxWait:  common.Duration(20 * time.Millisecond),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.SendRequest(ctx, newBlockNumberRequest())
	if err != nil {
		t.Fatalf(`batched "error": null was read as a failure: %v`, err)
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Fatalf("JsonRpcResponse: %v", err)
	}
	if jrr.Error != nil {
		t.Fatalf("batched response carries error %v; a null error member means no error", jrr.Error)
	}
}

// A real error member must still be an error. Without this the test above
// would pass on code that ignores the error member entirely.
func TestSingleRequest_ARealErrorMemberIsStillAnError(t *testing.T) {
	node := newFakeNode(t, jsonOK(`{"result":null,"error":{"code":-32000,"message":"execution reverted"},"id":1}`))
	c := httpClientFor(t, node, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.SendRequest(ctx, newBlockNumberRequest())
	if err == nil {
		t.Fatal("a real JSON-RPC error member was served as a success")
	}
	if !strings.Contains(err.Error(), "execution reverted") {
		t.Fatalf("error %v lost the upstream's message", err)
	}
}

// enableGzip must actually compress the request body AND declare it. A body
// compressed without the header, or a header without compression, makes the
// upstream reject every request — an outage that only shows up against a real
// node.
func TestPrepareRequest_GzipCompressesTheBodyAndDeclaresIt(t *testing.T) {
	node := newFakeNode(t, jsonOK(`{"result":"0x1","id":1}`))
	c := httpClientFor(t, node, &common.JsonRpcUpstreamConfig{EnableGzip: &common.TRUE})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := c.SendRequest(ctx, newBlockNumberRequest()); err != nil {
		t.Fatalf("SendRequest: %v", err)
	}

	body, hdr, _ := node.snapshot()
	if got := hdr.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	// Go's server does not transparently decode a gzipped REQUEST body, so what
	// the handler read is the raw compressed bytes — decode them here to prove
	// the payload survived the round trip intact.
	zr, err := gzip.NewReader(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("request body is not gzip despite the header: %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading the gzipped request body: %v", err)
	}
	if !strings.Contains(string(plain), `"eth_blockNumber"`) {
		t.Fatalf("decompressed body %q lost the method", plain)
	}
}

// Without enableGzip the body must go out as plain JSON. A client that always
// compressed would break every upstream that does not accept gzip requests.
func TestPrepareRequest_WithoutGzipTheBodyIsPlainJson(t *testing.T) {
	node := newFakeNode(t, jsonOK(`{"result":"0x1","id":1}`))
	c := httpClientFor(t, node, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := c.SendRequest(ctx, newBlockNumberRequest()); err != nil {
		t.Fatalf("SendRequest: %v", err)
	}

	body, hdr, _ := node.snapshot()
	if got := hdr.Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q on an uncompressed request", got)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
		t.Fatalf("body %q is not plain JSON", body)
	}
}

// eRPC always advertises that it accepts a gzipped RESPONSE. Dropping this
// header multiplies bandwidth on every large eth_getLogs answer.
func TestPrepareRequest_AlwaysAdvertisesGzipAcceptance(t *testing.T) {
	node := newFakeNode(t, jsonOK(`{"result":"0x1","id":1}`))
	c := httpClientFor(t, node, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := c.SendRequest(ctx, newBlockNumberRequest()); err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	_, hdr, _ := node.snapshot()
	if got := hdr.Get("Accept-Encoding"); !strings.Contains(got, "gzip") {
		t.Fatalf("Accept-Encoding = %q, want it to include gzip", got)
	}
	if got := hdr.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := hdr.Get("User-Agent"); !strings.HasPrefix(got, "erpc (") {
		t.Fatalf("User-Agent = %q; an upstream cannot attribute traffic without it", got)
	}
}

// Configured headers must reach the wire on the SINGLE-request path too. This
// is how a vendor API key gets attached; losing it turns every request into a
// 401 that reads like an upstream outage.
func TestPrepareRequest_ConfiguredHeadersReachTheWire(t *testing.T) {
	node := newFakeNode(t, jsonOK(`{"result":"0x1","id":1}`))
	c := httpClientFor(t, node, &common.JsonRpcUpstreamConfig{
		Headers: map[string]string{"Authorization": "Bearer secret-key"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := c.SendRequest(ctx, newBlockNumberRequest()); err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	_, hdr, _ := node.snapshot()
	if got := hdr.Get("Authorization"); got != "Bearer secret-key" {
		t.Fatalf("Authorization = %q, want the configured key", got)
	}
}

// Per-request forwarded headers ride along with the configured ones. These
// carry a caller's own trace or tenant identity to the upstream.
func TestSendSingleRequest_ForwardedHeadersReachTheWire(t *testing.T) {
	node := newFakeNode(t, jsonOK(`{"result":"0x1","id":1}`))
	c := httpClientFor(t, node, nil)

	req := newBlockNumberRequest()
	req.ForwardHeaders = http.Header{"X-Tenant-Id": []string{"acme"}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := c.SendRequest(ctx, req); err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	_, hdr, _ := node.snapshot()
	if got := hdr.Get("X-Tenant-Id"); got != "acme" {
		t.Fatalf("X-Tenant-Id = %q, want acme", got)
	}
}

// A gzipped RESPONSE must be decoded. Without this the caller sees the
// compressed bytes as a malformed JSON body and the healthy upstream is
// blamed for it.
func TestSendSingleRequest_DecodesAGzippedResponse(t *testing.T) {
	node := newFakeNode(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, `{"jsonrpc":"2.0","id":1,"result":"0xdeadbeef"}`)
		_ = zw.Close()
	})
	c := httpClientFor(t, node, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.SendRequest(ctx, newBlockNumberRequest())
	if err != nil {
		t.Fatalf("SendRequest on a gzipped response: %v", err)
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Fatalf("JsonRpcResponse: %v", err)
	}
	if got := string(jrr.GetResultBytes()); got != `"0xdeadbeef"` {
		t.Fatalf("result = %s, want \"0xdeadbeef\"", got)
	}
}

// The BATCH path decodes gzip through readResponseBody, a separate function
// from the single-request path. A regression in either one is invisible to a
// test of the other.
func TestProcessBatchResponse_DecodesAGzippedResponse(t *testing.T) {
	node := newFakeNode(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = io.WriteString(zw, `[{"jsonrpc":"2.0","id":1,"result":"0xfeed"}]`)
		_ = zw.Close()
	})
	c := httpClientFor(t, node, &common.JsonRpcUpstreamConfig{
		SupportsBatch: &common.TRUE,
		BatchMaxSize:  1,
		BatchMaxWait:  common.Duration(20 * time.Millisecond),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.SendRequest(ctx, newBlockNumberRequest())
	if err != nil {
		t.Fatalf("batch SendRequest on a gzipped response: %v", err)
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Fatalf("JsonRpcResponse: %v", err)
	}
	if got := string(jrr.GetResultBytes()); got != `"0xfeed"` {
		t.Fatalf("result = %s, want \"0xfeed\"", got)
	}
}

// A body that claims gzip but is not gzip must be an error, not a silent empty
// answer. Some proxies mislabel bodies; serving "" would look like a valid
// empty result to the cache.
func TestReadResponseBody_LyingGzipHeaderIsAnError(t *testing.T) {
	node := newFakeNode(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, `[{"jsonrpc":"2.0","id":1,"result":"0x1"}]`) // NOT gzipped
	})
	c := httpClientFor(t, node, &common.JsonRpcUpstreamConfig{
		SupportsBatch: &common.TRUE,
		BatchMaxSize:  1,
		BatchMaxWait:  common.Duration(20 * time.Millisecond),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.SendRequest(ctx, newBlockNumberRequest())
	if err == nil {
		jrr, _ := resp.JsonRpcResponse()
		t.Fatalf("a mislabelled gzip body was served as a result: %v", jrr)
	}
}

// A batch response the upstream returns for an ID nobody asked for must not be
// matched to a waiting caller. Delivering it would hand one caller another
// caller's answer.
func TestProcessBatchResponse_MismatchedIdDoesNotAnswerTheWaitingCaller(t *testing.T) {
	node := newFakeNode(t, jsonOK(`[{"jsonrpc":"2.0","id":999,"result":"0x1"}]`))
	c := httpClientFor(t, node, &common.JsonRpcUpstreamConfig{
		SupportsBatch: &common.TRUE,
		BatchMaxSize:  1,
		BatchMaxWait:  common.Duration(20 * time.Millisecond),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.SendRequest(ctx, newBlockNumberRequest())
	if err == nil {
		t.Fatal("a response for a different request ID was delivered to this caller")
	}
	if !strings.Contains(err.Error(), "no response received for request") {
		t.Fatalf("error = %v, want it to say no matching response arrived", err)
	}
}

// A batch response that is neither an array nor an object is malformed. Saying
// so — rather than returning an empty result — is what lets the router try
// another upstream.
func TestProcessBatchResponse_ScalarBodyIsAMalformedResponse(t *testing.T) {
	node := newFakeNode(t, jsonOK(`12345`))
	c := httpClientFor(t, node, &common.JsonRpcUpstreamConfig{
		SupportsBatch: &common.TRUE,
		BatchMaxSize:  1,
		BatchMaxWait:  common.Duration(20 * time.Millisecond),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.SendRequest(ctx, newBlockNumberRequest())
	if err == nil {
		t.Fatal("a bare number was accepted as a batch response")
	}
}

// A server that answers a BATCH with a single object still has to reach the
// caller. Some vendors collapse a one-element batch, and failing here would
// make every batched request to them time out.
func TestProcessBatchResponse_SingleObjectForABatchStillAnswers(t *testing.T) {
	node := newFakeNode(t, jsonOK(`{"jsonrpc":"2.0","id":1,"result":"0xabc"}`))
	c := httpClientFor(t, node, &common.JsonRpcUpstreamConfig{
		SupportsBatch: &common.TRUE,
		BatchMaxSize:  1,
		BatchMaxWait:  common.Duration(20 * time.Millisecond),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.SendRequest(ctx, newBlockNumberRequest())
	if err != nil {
		t.Fatalf("a collapsed single-object batch response failed: %v", err)
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		t.Fatalf("JsonRpcResponse: %v", err)
	}
	if got := string(jrr.GetResultBytes()); got != `"0xabc"` {
		t.Fatalf("result = %s, want \"0xabc\"", got)
	}
}

// batchWindow is long enough that no batch flushes during these tests.
const batchWindow = 3 * time.Second

// batchClientFor builds a batching client and returns it ready for direct
// queueRequest calls.
func batchClientFor(t *testing.T, node *fakeNode) *GenericHttpJsonRpcClient {
	t.Helper()
	return httpClientFor(t, node, &common.JsonRpcUpstreamConfig{
		SupportsBatch: &common.TRUE,
		BatchMaxSize:  5,
		BatchMaxWait:  common.Duration(batchWindow),
	})
}

// queueOne drives queueRequest directly and returns the error it delivered
// plus the number of requests left occupying the batch.
//
// Direct rather than through SendRequest on purpose. SendRequest also selects
// on ctx.Done() and classifies the error itself, so an end-to-end test races
// the two paths and cannot say which one produced the answer.
func queueOne(t *testing.T, c *GenericHttpJsonRpcClient, ctx context.Context) (error, int) {
	t.Helper()
	br := &batchRequest{
		ctx:      ctx,
		request:  newBlockNumberRequest(),
		response: make(chan *common.NormalizedResponse, 1),
		err:      make(chan error, 1),
	}
	c.queueRequest(1, br)

	var got error
	select {
	case got = <-br.err:
	default:
	}
	c.batchMu.Lock()
	queued := len(c.batchRequests)
	c.batchMu.Unlock()
	return got, queued
}

// A request whose context is already cancelled must be failed on the spot and
// must never take a batch slot. A dead request occupying a slot counts toward
// batchMaxSize, so it flushes a batch of LIVE requests early and wastes the
// upstream round trip the batching was configured to save.
func TestQueueRequest_CancelledRequestIsFailedAndNeverTakesABatchSlot(t *testing.T) {
	node := newFakeNode(t, jsonOK(`[{"jsonrpc":"2.0","id":1,"result":"0x1"}]`))
	c := batchClientFor(t, node)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err, queued := queueOne(t, c, ctx)
	if err == nil {
		t.Fatal("queueRequest neither failed nor answered an already-cancelled request")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled) {
		t.Fatalf("error = %v, want a request-cancelled classification", err)
	}
	if queued != 0 {
		t.Fatalf("%d cancelled request(s) still occupy the batch", queued)
	}
}

// A policy-driven timeout attaches its own sentinel as the context cause.
// queueRequest must classify it as a TIMEOUT and keep the sentinel: the
// upstream classifier promotes only that sentinel to a failsafe timeout, and a
// generic cancellation is retried differently.
func TestQueueRequest_DynamicTimeoutIsClassifiedAsATimeoutKeepingItsSentinel(t *testing.T) {
	node := newFakeNode(t, jsonOK(`[{"jsonrpc":"2.0","id":1,"result":"0x1"}]`))
	c := batchClientFor(t, node)

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(common.ErrDynamicTimeoutExceeded)

	err, queued := queueOne(t, c, ctx)
	if err == nil {
		t.Fatal("queueRequest accepted a request its timeout policy had already expired")
	}
	if !errors.Is(err, common.ErrDynamicTimeoutExceeded) {
		t.Fatalf("error %v lost the ErrDynamicTimeoutExceeded cause", err)
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout) {
		t.Fatalf("error = %v, want a request-timeout classification", err)
	}
	if queued != 0 {
		t.Fatalf("%d expired request(s) still occupy the batch", queued)
	}
}

// A live request MUST take a batch slot. Without this, the two tests above
// would pass on a queueRequest that refuses everything.
func TestQueueRequest_LiveRequestTakesABatchSlot(t *testing.T) {
	node := newFakeNode(t, jsonOK(`[{"jsonrpc":"2.0","id":1,"result":"0x1"}]`))
	c := batchClientFor(t, node)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err, queued := queueOne(t, c, ctx)
	if err != nil {
		t.Fatalf("a live request was failed at queue time: %v", err)
	}
	if queued != 1 {
		t.Fatalf("%d requests queued, want 1 — batching would never coalesce", queued)
	}
}

// An already-cancelled caller must reach no upstream at all, whichever guard
// catches it.
func TestSendRequest_CancelledCallerNeverReachesTheUpstream(t *testing.T) {
	node := newFakeNode(t, jsonOK(`[{"jsonrpc":"2.0","id":1,"result":"0x1"}]`))
	c := batchClientFor(t, node)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.SendRequest(ctx, newBlockNumberRequest()); err == nil {
		t.Fatal("a cancelled request was answered")
	}
	if _, _, n := node.snapshot(); n != 0 {
		t.Fatalf("the upstream saw %d requests for an already-cancelled caller", n)
	}
}

// A dead endpoint is a transport failure and the dial error must stay
// readable, or an operator cannot tell a refused connection from a DNS miss.
func TestSendSingleRequest_UnreachableEndpointIsATransportFailure(t *testing.T) {
	node := newFakeNode(t, jsonOK(`{"result":"0x1","id":1}`))
	c := httpClientFor(t, node, nil)
	node.closeSafely()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.SendRequest(ctx, newBlockNumberRequest())
	if err == nil {
		t.Fatal("a closed endpoint answered successfully")
	}
	if !common.HasErrorCode(err, common.ErrCodeEndpointTransportFailure) {
		t.Fatalf("error = %v, want a transport failure so the upstream is cordoned", err)
	}
}

// A caller that gives up mid-flight must get a CANCELLED classification, not a
// transport failure. Blaming the upstream for a client-side abort cordons a
// healthy node.
func TestSendSingleRequest_CallerCancellationIsNotBlamedOnTheUpstream(t *testing.T) {
	// The handler parks on an explicit release channel. It must NOT wait only
	// on r.Context(): Go's server does not cancel a request context when the
	// CLIENT abandons an otherwise-idle HTTP/1.1 connection, so the handler
	// would outlive the test and Close() would block forever.
	release := make(chan struct{})
	node := newFakeNode(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	c := httpClientFor(t, node, nil)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		_, err := c.SendRequest(ctx, newBlockNumberRequest())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an abandoned request returned a result")
		}
		if !common.HasErrorCode(err, common.ErrCodeEndpointRequestCanceled) {
			t.Fatalf("error = %v, want a request-cancelled classification", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SendRequest ignored caller cancellation")
	}
}

// A deadline that expires mid-flight must be a TIMEOUT, not a cancellation.
// The two drive different retry decisions upstream.
func TestSendSingleRequest_DeadlineExpiryIsClassifiedAsATimeout(t *testing.T) {
	release := make(chan struct{})
	node := newFakeNode(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	c := httpClientFor(t, node, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.SendRequest(ctx, newBlockNumberRequest())
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a request that blew its deadline returned a result")
		}
		if !common.HasErrorCode(err, common.ErrCodeEndpointRequestTimeout) {
			t.Fatalf("error = %v, want a request-timeout classification", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SendRequest ignored its deadline")
	}
}

// Batching must actually batch: several concurrent callers under the batch
// size have to leave as ONE HTTP request. If they do not, the feature costs
// the upstream N times the request budget it was configured to save.
func TestBatch_ConcurrentCallersLeaveAsOneHttpRequest(t *testing.T) {
	node := newFakeNode(t, jsonOK(
		`[{"jsonrpc":"2.0","id":1,"result":"0x1"},{"jsonrpc":"2.0","id":2,"result":"0x2"},{"jsonrpc":"2.0","id":3,"result":"0x3"}]`))
	c := httpClientFor(t, node, &common.JsonRpcUpstreamConfig{
		SupportsBatch: &common.TRUE,
		BatchMaxSize:  3,
		BatchMaxWait:  common.Duration(500 * time.Millisecond),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			req := common.NewNormalizedRequest([]byte(
				`{"jsonrpc":"2.0","id":` + string(rune('0'+id)) + `,"method":"eth_blockNumber","params":[]}`))
			_, errs[id-1] = c.SendRequest(ctx, req)
		}(i + 1)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d failed: %v", i+1, err)
		}
	}
	if _, _, n := node.snapshot(); n != 1 {
		t.Fatalf("the upstream saw %d HTTP requests, want 1 — batching did not coalesce", n)
	}
}

// An envelope carrying neither result nor error must still be attributed to
// the request that asked for it, and reported as an UNPARSEABLE response.
//
// The distinction matters to an operator: "cannot parse the upstream's answer"
// names a broken upstream, while "no response arrived for this ID" reads as a
// batching or routing bug in eRPC. Losing the ID turns the first into the
// second and sends the investigation to the wrong place.
func TestProcessBatchResponse_EnvelopeWithNeitherMemberIsReportedAsUnparseable(t *testing.T) {
	node := newFakeNode(t, jsonOK(`[{"jsonrpc":"2.0","id":1}]`))
	c := httpClientFor(t, node, &common.JsonRpcUpstreamConfig{
		SupportsBatch: &common.TRUE,
		BatchMaxSize:  1,
		BatchMaxWait:  common.Duration(20 * time.Millisecond),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.SendRequest(ctx, newBlockNumberRequest())
	if err == nil {
		t.Fatal("an envelope with neither result nor error was served as a success")
	}
	if strings.Contains(err.Error(), "no response received for request") {
		t.Fatalf("error = %v; the response lost its ID and now reads as a routing bug", err)
	}
	if !strings.Contains(err.Error(), "cannot parse json rpc response") {
		t.Fatalf("error = %v, want it to name the unparseable upstream response", err)
	}
}

// getHttpClient falls back to the client's own transport when no proxy pool is
// configured. Returning nil here would panic on the first request.
func TestGetHttpClient_NoProxyPoolUsesTheClientsOwnTransport(t *testing.T) {
	node := newFakeNode(t, jsonOK(`{"result":"0x1","id":1}`))
	c := httpClientFor(t, node, nil)

	if got := c.getHttpClient(); got == nil {
		t.Fatal("getHttpClient returned nil with no proxy pool configured")
	}
	if c.getHttpClient() != c.httpClient {
		t.Fatal("getHttpClient did not return the client's own http.Client")
	}
}

func TestGetType_IsHttpJsonRpc(t *testing.T) {
	node := newFakeNode(t, jsonOK(`{"result":"0x1","id":1}`))
	if got := httpClientFor(t, node, nil).GetType(); got != ClientTypeHttpJsonRpc {
		t.Fatalf("GetType() = %v, want %v", got, ClientTypeHttpJsonRpc)
	}
}
