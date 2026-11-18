package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A transaction sequencer (Base, Arbitrum) serves eth_sendRawTransaction and
// answers almost everything else, eth_chainId included, with -32601. These
// tests drive the real HTTP client and error normalizer against such a node.

// fakeSequencer answers eth_chainId with chainIdResult when it is non-empty,
// and with -32601 (method not found) when it is empty.
func fakeSequencer(t *testing.T, chainIdResult string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(body), "eth_chainId") && chainIdResult != "" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0", "id": 75412, "result": chainIdResult,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0", "id": 75412,
			"error": map[string]interface{}{
				"code":    -32601,
				"message": "the method eth_chainId does not exist/is not available",
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sequencerUpstream(t *testing.T, env *fwdEnv, endpoint string, chainId int64) *Upstream {
	t.Helper()
	u, err := env.newUpstream(t, &common.UpstreamConfig{
		Id:       "sequencer",
		Endpoint: endpoint,
		Evm:      &common.EvmUpstreamConfig{ChainId: chainId},
		AllowMethods: []string{
			"eth_sendRawTransaction",
			"eth_chainId",
		},
		IgnoreMethods: []string{"*"},
	})
	require.NoError(t, err)
	return u
}

func TestSequencerChainId_ConfiguredIdIsTrustedWhenMethodNotFound(t *testing.T) {
	env := newFwdEnv(t, nil)
	u := sequencerUpstream(t, env, fakeSequencer(t, "").URL, 8453)

	require.NoError(t, u.detectFeatures(env.ctx),
		"a sequencer with a configured chainId must initialize when it lacks eth_chainId")
	assert.Equal(t, int64(8453), u.Config().Evm.ChainId)
	assert.Equal(t, "evm:8453", u.NetworkId())
}

// The endpoint URL may carry an API key in its path. The warning must name the
// upstream without leaking that key.
func TestSequencerChainId_WarningRedactsEndpoint(t *testing.T) {
	const key = "k3y-in-the-path-0123456789"
	env := newFwdEnv(t, nil)
	u := sequencerUpstream(t, env, fakeSequencer(t, "").URL+"/"+key, 8453)
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })
	var buf bytes.Buffer
	lg := zerolog.New(&buf)
	u.logger = &lg

	require.NoError(t, u.detectFeatures(env.ctx))
	out := buf.String()
	assert.Contains(t, out, `"level":"warn"`)
	assert.Contains(t, out, `"upstreamId":"sequencer"`)
	assert.Contains(t, out, "eth_chainId")
	assert.NotContains(t, out, key, "the endpoint key leaked into the log")
	assert.Equal(t, 1, strings.Count(out, "using the configured evm.chainId"), "log once per init")
}

func TestSequencerChainId_ConfiguredIdMismatchStillFails(t *testing.T) {
	env := newFwdEnv(t, nil)
	u := sequencerUpstream(t, env, fakeSequencer(t, "0x1").URL, 8453)

	err := u.detectFeatures(env.ctx)
	require.Error(t, err)
	assert.True(t, isFatal(err), "a chainId mismatch is permanent: %v", err)
	assert.Contains(t, err.Error(), "ErrUpstreamChainIdMismatch")
	assert.Equal(t, "n/a", u.NetworkId(), "a node on the wrong chain must not be routable")
}

func TestSequencerChainId_NoConfiguredIdKeepsTodaysFailure(t *testing.T) {
	env := newFwdEnv(t, nil)
	u := sequencerUpstream(t, env, fakeSequencer(t, "").URL, 0)

	err := u.detectFeatures(env.ctx)
	require.Error(t, err, "without a configured chainId there is no id to trust")
	assert.False(t, isFatal(err), "detection failure stays retryable: %v", err)
	assert.Contains(t, err.Error(), "ErrUpstreamChainIdDetectionFailed")
	assert.Equal(t, "n/a", u.NetworkId())
}

func TestSequencerChainId_ServerErrorWithConfiguredIdKeepsTodaysFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	env := newFwdEnv(t, nil)
	u := sequencerUpstream(t, env, srv.URL, 8453)

	err := u.detectFeatures(env.ctx)
	require.Error(t, err, "a 5xx is not a method-not-found verdict")
	assert.False(t, isFatal(err), "a 5xx stays retryable: %v", err)
	assert.Contains(t, err.Error(), "ErrUpstreamChainIdDetectionFailed")
	assert.Equal(t, "n/a", u.NetworkId())
}
