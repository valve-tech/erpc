package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/valvebilling"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubBackend struct {
	answer []byte
	calls  int
}

func (s *stubBackend) Forward(ctx context.Context, chainID int64, body []byte) ([]byte, error) {
	s.calls++
	return s.answer, nil
}
func (s *stubBackend) Close() error { return nil }

func serve(t *testing.T, billing *valvebilling.Module, b *stubBackend, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	lg := zerolog.Nop()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /evm/{chainId}", handler(&lg, billing, b))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func post(path, body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// With the flag clear the binary is a proxy: no headers, no billing, no Redis.
func TestHandler_PassesThroughWhenBillingIsOff(t *testing.T) {
	b := &stubBackend{answer: []byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`)}
	rec := serve(t, nil, b, post("/evm/1", `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`, nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, string(b.answer), rec.Body.String())
	assert.Equal(t, 1, b.calls)
}

// With billing on, a request that names no account cannot be billed, so it is
// refused before it reaches the upstream rather than served for free.
func TestHandler_RequiresIdentityWhenBillingIsOn(t *testing.T) {
	mr := miniredis.RunT(t)
	m, err := valvebilling.New(context.Background(), valvebilling.Config{
		Enabled:  true,
		RedisURL: "redis://" + mr.Addr(),
		Pepper:   "SYNTHETIC-pepper-for-tests-00000",
	}, valvebilling.NewPriceTable(map[string]int64{}, 6))
	require.NoError(t, err)
	defer func() { _ = m.Close() }()

	b := &stubBackend{answer: []byte(`{}`)}
	rec := serve(t, m, b, post("/evm/1", `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`, nil))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, b.calls, "an unbillable request reached the upstream")
}

func TestHandler_RefusesABadChainId(t *testing.T) {
	b := &stubBackend{answer: []byte(`{}`)}
	rec := serve(t, nil, b, post("/evm/mainnet", `{"method":"eth_blockNumber"}`, nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, b.calls)
}

// A batch would be billed as one call, which undercharges silently. Refuse it.
func TestMethodOf(t *testing.T) {
	m, err := methodOf([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[]}`))
	require.NoError(t, err)
	assert.Equal(t, "eth_call", m)

	_, err = methodOf([]byte(` [{"method":"eth_call"}]`))
	require.Error(t, err, "a batch must not be priced as one call")

	_, err = methodOf([]byte(`{"jsonrpc":"2.0","id":1}`))
	require.Error(t, err)

	_, err = methodOf([]byte(`not json`))
	require.Error(t, err)
}
