package valverelay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPBackend_PostsToTheProjectAndChainPath(t *testing.T) {
	var gotPath, gotMethod, gotAccept, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAccept = r.Header.Get("Accept-Encoding")
		gotType = r.Header.Get("Content-Type")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`))
	}))
	defer srv.Close()

	b, err := NewHTTPBackend(srv.URL, "main", time.Second)
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	out, err := b.Forward(context.Background(), 42161, []byte(`{"method":"eth_blockNumber"}`))
	require.NoError(t, err)

	assert.Equal(t, `{"jsonrpc":"2.0","id":1,"result":"0x10"}`, string(out))
	assert.Equal(t, "/main/evm/42161", gotPath)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotType)
	assert.Equal(t, `{"method":"eth_blockNumber"}`, gotBody)
	// Without this header Go's transport asks for gzip on its own, and eRPC
	// compresses a body the relay inflates a microsecond later on the same
	// box. The monorepo measured 0.78 cores saved by asking for identity.
	assert.Equal(t, "identity", gotAccept,
		"the relay hop asked for a compressed answer it is about to decompress itself")
}

// A trailing slash on the base URL must not produce a doubled separator.
func TestHTTPBackend_TrimsATrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"result":1}`))
	}))
	defer srv.Close()

	b, err := NewHTTPBackend(srv.URL+"/", "main", time.Second)
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	_, err = b.Forward(context.Background(), 1, []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, "/main/evm/1", gotPath)
}

// Rule 2, through the real HTTP backend: eRPC answers 200 for a JSON-RPC
// application error, that body is an answer, and the answer is billed.
func TestHTTPBackend_AJsonRpcErrorIsAnAnswerAndIsBilled(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"error":{"code":3,"message":"execution reverted"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	b, err := NewHTTPBackend(srv.URL, "main", time.Second)
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1000"))

	res, err := billed(context.Background(), m, b, fundedRequest())
	require.NoError(t, err)
	assert.Equal(t, body, string(res.Body))
	assert.Equal(t, int64(testCost), res.Billed.Int64())

	got, err := mr.Get(testSpendKey)
	require.NoError(t, err, "a reverted call travelled over HTTP and was not billed")
	assert.Equal(t, fmt.Sprint(testCost), got)
}

// Rule 1, through the real HTTP backend: eRPC reserves non-2xx for
// transport-level faults — rate limits, auth, an unknown network. Those are
// not upstream work and the customer pays nothing.
func TestHTTPBackend_TreatsNon2xxAsNoAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":-32005,"message":"rate limit"}}`))
	}))
	defer srv.Close()

	b, err := NewHTTPBackend(srv.URL, "main", time.Second)
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1000"))

	res, err := billed(context.Background(), m, b, fundedRequest())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "429")
	assert.Nil(t, res.Body)
	assert.False(t, mr.Exists(testSpendKey), "a rate-limited request was billed")
}

// A hung upstream must not hold the request forever.
func TestHTTPBackend_TimesOut(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	b, err := NewHTTPBackend(srv.URL, "main", 50*time.Millisecond)
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	start := time.Now()
	_, err = b.Forward(context.Background(), 1, []byte(`{}`))
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "the request outlived the timeout")
}

// An empty body is not an answer, whatever the status says.
func TestHTTPBackend_RefusesAnEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	b, err := NewHTTPBackend(srv.URL, "main", time.Second)
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	_, err = b.Forward(context.Background(), 1, []byte(`{}`))
	require.Error(t, err)
}

func TestNewHTTPBackend_RefusesUnusableSettings(t *testing.T) {
	_, err := NewHTTPBackend("http://localhost:4000", "", time.Second)
	require.Error(t, err, "a missing project id would post to the wrong path")

	_, err = NewHTTPBackend("http://localhost:4000", "main", 0)
	require.Error(t, err, "a relay without a timeout can be held open forever")

	_, err = NewHTTPBackend("localhost:4000", "main", time.Second)
	require.Error(t, err, "a base URL without a scheme is not usable")

	_, err = NewHTTPBackend("://nope", "main", time.Second)
	require.Error(t, err)
}

// A transport failure is not a rejection either: the caller must not report a
// dead upstream to a customer as an empty account.
func TestHTTPBackend_TransportFailureIsNotARejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close()

	b, err := NewHTTPBackend(addr, "main", time.Second)
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1000"))

	_, err = billed(context.Background(), m, b, fundedRequest())
	require.Error(t, err)
	var rejected *RejectedError
	assert.False(t, errors.As(err, &rejected))
	assert.False(t, mr.Exists(testSpendKey))
}
