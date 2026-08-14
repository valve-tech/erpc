package common

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// NormalizedResponse is handed between the upstream layer, the cache and the
// HTTP writer, and every one of them may hold a nil pointer or release the
// object while another goroutine still reads it. These tests pin both: the
// nil-safety contract, and the release/parse ordering that decides whether a
// client gets its body or a truncated one.

const responseTestTimeout = 10 * time.Second

// countingCloser reports how many times the body was closed. A double close on
// a pooled gzip reader corrupts the next request that borrows the buffer.
type countingCloser struct {
	io.Reader
	closes  atomic.Int32
	failErr error
}

func (c *countingCloser) Close() error {
	c.closes.Add(1)
	return c.failErr
}

func responseWithBody(t *testing.T, body string) (*NormalizedResponse, *countingCloser) {
	t.Helper()
	rc := &countingCloser{Reader: strings.NewReader(body)}
	r := NewNormalizedResponse().WithBody(rc).WithExpectedSize(len(body))
	return r, rc
}

// Every accessor must tolerate a nil receiver: the request path calls them on
// the result of a failed attempt, where the response is nil by definition. A
// panic here takes down the whole process, not just the request.
func TestNormalizedResponse_NilReceiverIsSafeOnEveryAccessor(t *testing.T) {
	var r *NormalizedResponse

	require.False(t, r.FromCache())
	require.Nil(t, r.EvmBlockRef())
	require.Nil(t, r.EvmBlockNumber())
	require.Equal(t, time.Duration(0), r.Duration())
	require.Equal(t, 0, r.Attempts())
	require.Equal(t, 0, r.Retries())
	require.Equal(t, 0, r.Hedges())
	require.Nil(t, r.Upstream())
	require.Equal(t, "", r.UpstreamId())
	require.Nil(t, r.Request())
	require.True(t, r.IsObjectNull())
	require.False(t, r.IsIntegrityRejected())
	require.Equal(t, DataFinalityStateUnknown, r.Finality(context.Background()))

	// The chainable setters must return the nil receiver rather than panic.
	require.Nil(t, r.SetFromCache(true))
	require.Nil(t, r.WithFromCache(true))
	require.Nil(t, r.WithRequest(nil))
	require.Nil(t, r.WithBody(nil))
	require.Nil(t, r.WithExpectedSize(1))
	require.Nil(t, r.WithFinality(DataFinalityStateRealtime))
	require.Nil(t, r.SetUpstream(nil))

	// These simply must not panic.
	r.SetEvmBlockRef("latest")
	r.SetEvmBlockNumber(int64(1))
	r.SetDuration(time.Second)
	r.MarkIntegrityRejected()
	r.AddRef()
	r.DoneRef()
	r.Release()

	hash, err := r.Hash()
	require.NoError(t, err)
	require.Equal(t, "", hash)

	size, err := r.Size()
	require.NoError(t, err)
	require.Equal(t, 0, size)

	// WriteTo is the only one that must report a problem: a caller about to
	// write a body needs to know there is none.
	n, err := r.WriteTo(&bytes.Buffer{})
	require.Error(t, err)
	require.Zero(t, n)
}

func TestNormalizedResponse_CountersAndFlagsRoundTrip(t *testing.T) {
	r := NewNormalizedResponse()

	require.Equal(t, 7, r.SetAttempts(7).Attempts())
	require.Equal(t, 3, r.SetRetries(3).Retries())
	require.Equal(t, 2, r.SetHedges(2).Hedges())
	require.True(t, r.SetFromCache(true).FromCache())
	require.False(t, r.WithFromCache(false).FromCache())

	r.SetDuration(250 * time.Millisecond)
	require.Equal(t, 250*time.Millisecond, r.Duration())

	r.SetEvmBlockRef("latest")
	r.SetEvmBlockNumber(int64(1234))
	require.Equal(t, "latest", r.EvmBlockRef())
	require.Equal(t, int64(1234), r.EvmBlockNumber())

	// A nil value must not overwrite an already-stored one: the extractors call
	// these with whatever they found, including nothing.
	r.SetEvmBlockRef(nil)
	r.SetEvmBlockNumber(nil)
	require.Equal(t, "latest", r.EvmBlockRef())
	require.Equal(t, int64(1234), r.EvmBlockNumber())
}

func TestNormalizedResponse_UpstreamIdComesFromTheConfig(t *testing.T) {
	r := NewNormalizedResponse()
	require.Equal(t, "", r.UpstreamId(), "no upstream means no id, not a panic")

	up := NewFakeUpstream("up-alpha")
	r.SetUpstream(up)
	require.Same(t, up, r.Upstream())
	require.Equal(t, "up-alpha", r.UpstreamId())

	// A nil upstream must not clear the one already set — the hedge path calls
	// SetUpstream with whatever the losing attempt had.
	r.SetUpstream(nil)
	require.Equal(t, "up-alpha", r.UpstreamId())
}

// A response an integrity check rejected must never be re-served. The flag is
// the only thing standing between a corrupt hedged body and the client.
func TestNormalizedResponse_IntegrityRejectionSticks(t *testing.T) {
	r := NewNormalizedResponse()
	require.False(t, r.IsIntegrityRejected())

	r.MarkIntegrityRejected()
	require.True(t, r.IsIntegrityRejected())

	// There is deliberately no way to clear it.
	r.MarkIntegrityRejected()
	require.True(t, r.IsIntegrityRejected())
}

func TestNormalizedResponse_ParsesItsBodyOnceAndClosesIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	r, rc := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":"0x64"}`)

	jrr, err := r.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.NotNil(t, jrr)
	require.Nil(t, jrr.Error)
	require.Equal(t, `"0x64"`, string(jrr.GetResultString()))

	// The body is closed as soon as parsing succeeds, so a gzip buffer is
	// returned to its pool instead of being pinned for the response's lifetime.
	require.Equal(t, int32(1), rc.closes.Load())

	// A second call must reuse the parsed object rather than re-read a spent
	// reader, which would yield an empty result.
	again, err := r.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.Same(t, jrr, again)
	require.Equal(t, int32(1), rc.closes.Load(), "the body must be closed exactly once")
}

// A truncated or non-JSON body must surface as a JSON-RPC parse error the
// client can read, not as a silent empty result.
func TestNormalizedResponse_MalformedBodyBecomesAParseError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	r, _ := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":`)

	jrr, err := r.JsonRpcResponse(ctx)
	require.NoError(t, err, "the parse failure is carried in the response, not returned")
	require.NotNil(t, jrr)
	require.NotNil(t, jrr.Error, "a malformed body must produce a JSON-RPC error")
	require.Contains(t, jrr.Error.Error(), "cannot parse json-rpc response")
}

// A response with no body at all is a bug somewhere upstream. It must still
// produce a readable error rather than a nil dereference in the writer.
func TestNormalizedResponse_NoBodyBecomesAParseError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	r := NewNormalizedResponse()
	jrr, err := r.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.NotNil(t, jrr)
	require.NotNil(t, jrr.Error)
	require.Contains(t, jrr.Error.Error(), "no body available to parse")
}

func TestNormalizedResponse_ReleaseIsIdempotentAndClosesTheBody(t *testing.T) {
	r, rc := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)

	r.Release()
	require.Equal(t, int32(1), rc.closes.Load())

	// Release runs on several paths (the writer, the cache, a defer). A second
	// call must not close the body again.
	r.Release()
	r.Release()
	require.Equal(t, int32(1), rc.closes.Load())
}

// A body whose Close fails must not panic the release path. The failure is
// logged when a network is attached and swallowed otherwise.
func TestNormalizedResponse_ReleaseSurvivesAFailingClose(t *testing.T) {
	rc := &countingCloser{Reader: strings.NewReader("{}"), failErr: errors.New("already closed")}
	r := NewNormalizedResponse().WithBody(rc)

	require.NotPanics(t, r.Release)
	require.Equal(t, int32(1), rc.closes.Load())
}

// Release waits for background users (an async cache write, say) before it
// frees the buffers. Without the wait the cache writer reads freed memory.
func TestNormalizedResponse_ReleaseWaitsForPendingOps(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	r, _ := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	_, err := r.JsonRpcResponse(ctx)
	require.NoError(t, err)

	r.AddRef()

	released := make(chan struct{})
	go func() {
		r.Release()
		close(released)
	}()

	select {
	case <-released:
		t.Fatal("Release must not finish while a background op still holds a reference")
	case <-time.After(50 * time.Millisecond):
	}

	r.DoneRef()

	select {
	case <-released:
	case <-ctx.Done():
		t.Fatal("Release must finish once the last reference is done")
	}
}

func TestNormalizedResponse_MarshalAndWriteToUseTheParsedBody(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	// Before parsing there is nothing to marshal or write.
	empty := NewNormalizedResponse()
	body, err := empty.MarshalJSON()
	require.NoError(t, err)
	require.Nil(t, body)

	_, err = empty.WriteTo(&bytes.Buffer{})
	require.ErrorContains(t, err, "unexpected empty response")

	r, _ := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":"0x64"}`)
	_, err = r.JsonRpcResponse(ctx)
	require.NoError(t, err)

	// After parsing, MarshalJSON deliberately refuses: JsonRpcResponse holds the
	// result as raw bytes, and re-marshalling it would re-encode (and could
	// reorder) a payload the client must receive verbatim. WriteTo is the only
	// sanctioned way out.
	_, err = r.MarshalJSON()
	require.ErrorContains(t, err, "MarshalJSON must not be used on JsonRpcResponse")

	var buf bytes.Buffer
	n, err := r.WriteTo(&buf)
	require.NoError(t, err)
	require.Greater(t, n, int64(0))
	require.Contains(t, buf.String(), `"0x64"`)
}

func TestNormalizedResponse_HashAndSizeComeFromTheParsedBody(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	a, _ := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":{"a":1,"b":2}}`)
	// Same result, different key order and a different id: the canonical hash
	// must ignore both, or a consensus comparison would flag agreeing upstreams
	// as disputing.
	b, _ := responseWithBody(t, `{"jsonrpc":"2.0","id":9,"result":{"b":2,"a":1}}`)

	ha, err := a.Hash(ctx)
	require.NoError(t, err)
	hb, err := b.Hash(ctx)
	require.NoError(t, err)
	require.Equal(t, ha, hb)
	require.NotEmpty(t, ha)

	// Ignoring a field must change the hash, or the ignore list does nothing.
	hIgnored, err := a.HashWithIgnoredFields([]string{"a"}, ctx)
	require.NoError(t, err)
	require.NotEqual(t, ha, hIgnored)

	size, err := a.Size(ctx)
	require.NoError(t, err)
	require.Greater(t, size, 0)
}

func TestNormalizedResponse_IsObjectNullAndEmptyish(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	full, _ := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":"0x64"}`)
	require.False(t, full.IsObjectNull(ctx))
	require.False(t, full.IsResultEmptyish(ctx))

	// A null result is the shape a point lookup returns for a block that does
	// not exist. It is present but empty — not a null object.
	null, _ := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":null}`)
	require.True(t, null.IsResultEmptyish(ctx))

	var nilResp *NormalizedResponse
	require.True(t, nilResp.IsObjectNull(ctx))
	require.True(t, nilResp.IsResultEmptyish(ctx))
}

// WithFinality bypasses the derivation logic. A synthetic response (a cache
// seed, a static response) has no upstream to ask, so the explicit value has to
// win and be cached.
func TestNormalizedResponse_FinalityPrefersTheExplicitValue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	r := NewNormalizedResponse()
	require.Equal(t, DataFinalityStateUnknown, r.Finality(ctx),
		"with no request and no explicit value the answer must be unknown, not finalized")

	r.WithFinality(DataFinalityStateRealtime)
	require.Equal(t, DataFinalityStateRealtime, r.Finality(ctx))

	// A request with no network attached must not change the stored answer.
	r.WithRequest(NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`)))
	require.Equal(t, DataFinalityStateRealtime, r.Finality(ctx))
}

func TestNormalizedResponse_MarshalZerologObjectNamesTheUpstream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	prevLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prevLevel) })

	render := func(r *NormalizedResponse) string {
		var buf bytes.Buffer
		lg := zerolog.New(&buf).Level(zerolog.DebugLevel)
		lg.Info().Object("resp", r).Msg("response")
		return buf.String()
	}

	// Without an upstream the field must still be present and say so: a missing
	// field reads as "we forgot to log it", which is a different diagnosis.
	bare := render(NewNormalizedResponse())
	require.Contains(t, bare, `"upstream":"nil"`)
	require.Contains(t, bare, `"fromCache":false`)
	require.Contains(t, bare, `"attempts":0`)

	r, _ := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":"0x64"}`)
	r.SetUpstream(NewFakeUpstream("up-alpha"))
	r.SetFromCache(true)
	r.SetAttempts(2)
	r.SetRetries(1)
	r.SetHedges(1)
	r.SetEvmBlockNumber(int64(99))
	_, err := r.JsonRpcResponse(ctx)
	require.NoError(t, err)

	out := render(r)
	require.Contains(t, out, `"upstream":"up-alpha"`)
	require.Contains(t, out, `"fromCache":true`)
	require.Contains(t, out, `"attempts":2`)
	require.Contains(t, out, `"retries":1`)
	require.Contains(t, out, `"hedges":1`)
	require.Contains(t, out, `"evmBlockNumber":99`)
	require.Contains(t, out, `"jsonRpc"`)

	// A nil receiver must log nothing rather than panic inside the logger.
	var nilResp *NormalizedResponse
	require.NotPanics(t, func() { render(nilResp) })
}

// A multiplexed request shares one upstream call between several clients. Each
// copy must carry that client's own JSON-RPC id, or every client but one gets
// a reply it will discard as unmatched.
//
// The original is parsed FIRST on purpose. Copying an unparsed response
// deadlocks: CopyResponseForRequest holds the response's read lock
// (common/response.go:623) and then calls JsonRpcResponse, which takes the same
// non-reentrant mutex for writing (common/response.go:331). See the bug note in
// the report — that path is left uncovered here rather than hanging the suite.
func TestCopyResponseForRequest_RewritesTheIdForTheNewRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	orig, _ := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":"0x64"}`)
	_, err := orig.JsonRpcResponse(ctx)
	require.NoError(t, err)
	orig.SetUpstream(NewFakeUpstream("up-alpha"))
	orig.SetFromCache(true)
	orig.SetAttempts(3)
	orig.SetRetries(2)
	orig.SetHedges(1)
	orig.SetEvmBlockRef("latest")
	orig.SetEvmBlockNumber(int64(100))

	req := NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{JSONRPC: "2.0", ID: 77, Method: "eth_blockNumber"})

	copied, cerr := CopyResponseForRequest(ctx, orig, req)
	require.NoError(t, cerr)
	require.NotNil(t, copied)

	jrr, err := copied.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 77, jrr.ID(), "the copy must answer the new request's id")

	// The metadata must travel with the copy: the HTTP layer reads it for the
	// X-ERPC-* headers of the multiplexed client too.
	require.Equal(t, "up-alpha", copied.UpstreamId())
	require.True(t, copied.FromCache())
	require.Equal(t, 3, copied.Attempts())
	require.Equal(t, 2, copied.Retries())
	require.Equal(t, 1, copied.Hedges())
	require.Equal(t, "latest", copied.EvmBlockRef())
	require.Equal(t, int64(100), copied.EvmBlockNumber())
	require.Same(t, req, copied.Request())

	// The original must keep its own id — the copy must not mutate the shared
	// response in place.
	origJrr, err := orig.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, origJrr.ID())
}

func TestCopyResponseForRequest_NilResponseCopiesToNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	req := NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{JSONRPC: "2.0", ID: 1, Method: "eth_call"})
	copied, err := CopyResponseForRequest(ctx, nil, req)
	require.NoError(t, err)
	require.Nil(t, copied)
}

// A bodyless original still produces a copy, but the copy must carry the parse
// error rather than an empty 200 the multiplexed client would treat as success.
func TestCopyResponseForRequest_BodylessOriginalCarriesTheParseError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	orig := NewNormalizedResponse()
	// Parse first — see the deadlock note on the test above.
	_, err := orig.JsonRpcResponse(ctx)
	require.NoError(t, err)

	req := NewNormalizedRequestFromJsonRpcRequest(&JsonRpcRequest{JSONRPC: "2.0", ID: 1, Method: "eth_call"})

	copied, cerr := CopyResponseForRequest(ctx, orig, req)
	require.NoError(t, cerr)
	require.NotNil(t, copied)

	jrr, err := copied.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.NotNil(t, jrr.Error, "the parse failure must survive the copy")
}

// Parsing and releasing race in production: the writer parses while a deferred
// Release runs on another goroutine. Neither may panic and the body must still
// close exactly once.
func TestNormalizedResponse_ConcurrentParseAndReleaseAreSafe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), responseTestTimeout)
	defer cancel()

	for i := 0; i < 50; i++ {
		r, rc := responseWithBody(t, `{"jsonrpc":"2.0","id":1,"result":"0x64"}`)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = r.JsonRpcResponse(ctx)
		}()
		go func() {
			defer wg.Done()
			r.Release()
		}()
		wg.Wait()

		require.Equal(t, int32(1), rc.closes.Load(), "iteration %d closed the body more than once", i)
	}
}
