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
	mux.HandleFunc("POST /evm/{chainId}", handler(&lg, billing, b, valvebilling.Limits{}, defaultMaxRequestBytes))
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

// defaultMaxRequestBytes is the -max-request-bytes default, restated here so
// the handler tests read one body limit rather than each inventing one.
const defaultMaxRequestBytes = 8 << 20

// -max-request-bytes really bounds the body. The flag exists so an operator
// can move this, so a test has to prove the value reaches MaxBytesReader.
func TestHandler_HonoursTheConfiguredBodyLimit(t *testing.T) {
	b := &stubBackend{answer: []byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`)}
	lg := zerolog.Nop()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /evm/{chainId}", handler(&lg, nil, b, valvebilling.Limits{}, 8))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, post("/evm/1", `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`, nil))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, 0, b.calls, "an over-long body must not reach the upstream")
}

// The handler puts the deployment's tier limits on every request. Without
// this the Limits struct stays zero and every rate gate in the script is off.
func TestHandler_PutsTheTierLimitsOnEveryRequest(t *testing.T) {
	mr := miniredis.RunT(t)
	m, err := valvebilling.New(context.Background(), valvebilling.Config{
		Enabled:  true,
		RedisURL: "redis://" + mr.Addr(),
		Pepper:   "0123456789012345678901234567890123456789",
	}, valvebilling.NewPriceTable(nil, 6))
	require.NoError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, mr.Set("valve:credits:acct_1:ceiling", "1000000000000"))

	b := &stubBackend{answer: []byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`)}
	lg := zerolog.Nop()
	mux := http.NewServeMux()
	// One request per second is all this deployment allows.
	limits := valvebilling.Limits{SlowThreshold: 1, FullCPS: 1000, SlowCPS: 100, FullRPS: 1, SlowRPS: 1}
	mux.HandleFunc("POST /evm/{chainId}", handler(&lg, m, b, limits, defaultMaxRequestBytes))

	headers := map[string]string{"X-Valve-Account-Id": "acct_1", "X-Valve-Key-Id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber"}`

	first := httptest.NewRecorder()
	mux.ServeHTTP(first, post("/evm/1", body, headers))
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())

	second := httptest.NewRecorder()
	mux.ServeHTTP(second, post("/evm/1", body, headers))
	assert.Equal(t, http.StatusPaymentRequired, second.Code,
		"the second request in the same second must trip the per-second gate; a zero Limits would let it through")
	assert.Contains(t, second.Body.String(), "rate_second")
}
