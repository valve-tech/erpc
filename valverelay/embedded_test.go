package valverelay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEvmUpstream answers the handful of methods eRPC needs to bring a
// network up, plus whatever the test asks for. It is a real HTTP server
// rather than a mock transport so the embedded backend exercises eRPC's
// actual client path.
func fakeEvmUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := string(req.ID)
		if id == "" {
			id = "null"
		}

		var result string
		switch req.Method {
		case "eth_chainId":
			result = `"0x7b"` // 123
		case "eth_syncing":
			result = `false`
		case "eth_blockNumber":
			result = `"0x100"`
		case "eth_getBlockByNumber":
			result = `{"number":"0x100","hash":"0x1111111111111111111111111111111111111111111111111111111111111111","parentHash":"0x2222222222222222222222222222222222222222222222222222222222222222","timestamp":"0x64000000"}`
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"unsupported: %s"}}`, id, req.Method)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, id, result)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func embeddedConfig(t *testing.T, endpoint string) *common.Config {
	t.Helper()
	yaml := fmt.Sprintf(`
logLevel: error
projects:
  - id: main
    networks:
      - architecture: evm
        evm:
          chainId: 123
    upstreams:
      - id: fake
        type: evm
        endpoint: %s
        evm:
          chainId: 123
        jsonRpc:
          supportsBatch: false
`, endpoint)

	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, "/erpc.yaml", []byte(yaml), 0o600))
	cfg, err := common.LoadConfig(fs, "/erpc.yaml", &common.DefaultOptions{})
	require.NoError(t, err)
	return cfg
}

// The embedded backend boots a real eRPC and forwards through it in-process,
// with no HTTP server between the relay and the network.
//
// This is also the test that pins the serialisation: NormalizedResponse.WriteTo
// is the only way to get these bytes. JsonRpcResponse.MarshalJSON is a hard
// error by construction, so a backend that marshalled instead would fail every
// request — and on the billing path, fail it AFTER the upstream did the work.
func TestEmbeddedBackend_ForwardsInProcess(t *testing.T) {
	srv := fakeEvmUpstream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lg := zerolog.Nop()
	b, err := NewEmbeddedBackend(ctx, &lg, embeddedConfig(t, srv.URL), "main")
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	out, err := b.Forward(ctx, 123, []byte(`{"jsonrpc":"2.0","id":7,"method":"eth_blockNumber","params":[]}`))
	require.NoError(t, err)

	var got struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  string          `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &got), "the backend produced %q", string(out))
	assert.Equal(t, "2.0", got.JSONRPC)
	assert.Equal(t, "7", string(got.ID))
	assert.Equal(t, "0x100", got.Result)
}

// The billed path over the embedded backend: same rules, same capture.
func TestEmbeddedBackend_BillsThroughTheSamePath(t *testing.T) {
	srv := fakeEvmUpstream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lg := zerolog.Nop()
	b, err := NewEmbeddedBackend(ctx, &lg, embeddedConfig(t, srv.URL), "main")
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1000"))

	req := fundedRequest()
	req.ChainID = 123
	res, err := billed(ctx, m, b, req)
	require.NoError(t, err)
	assert.Contains(t, string(res.Body), "0x100")
	assert.Equal(t, int64(testCost), res.Billed.Int64())

	got, err := mr.Get(testSpendKey)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprint(testCost), got)
}

// A chain the config does not serve produces no answer, so it bills nothing.
func TestEmbeddedBackend_UnknownChainIsNotBilled(t *testing.T) {
	srv := fakeEvmUpstream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lg := zerolog.Nop()
	b, err := NewEmbeddedBackend(ctx, &lg, embeddedConfig(t, srv.URL), "main")
	require.NoError(t, err)
	defer func() { _ = b.Close() }()

	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1000"))

	req := fundedRequest()
	req.ChainID = 999
	_, err = billed(ctx, m, b, req)
	require.Error(t, err)
	assert.False(t, mr.Exists(testSpendKey))
}

func TestNewEmbeddedBackend_RefusesUnusableSettings(t *testing.T) {
	srv := fakeEvmUpstream(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	lg := zerolog.Nop()

	_, err := NewEmbeddedBackend(ctx, &lg, embeddedConfig(t, srv.URL), "")
	require.Error(t, err, "a missing project id must not be guessed from the config")

	_, err = NewEmbeddedBackend(ctx, &lg, nil, "main")
	require.Error(t, err)

	_, err = NewEmbeddedBackend(ctx, &lg, embeddedConfig(t, srv.URL), "not-a-project")
	require.Error(t, err, "an unknown project must fail at boot, not on every request")
	if err != nil {
		assert.True(t, strings.Contains(err.Error(), "not-a-project"))
	}
}
