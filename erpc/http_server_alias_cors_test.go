package erpc

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// Domain aliasing lets an operator hand a customer a bare hostname —
// https://eth.example.com — and have it resolve to one project and one chain
// with no path at all. Every request to that hostname is decided by these
// rules before anything else runs, so a rule that stops being applied does not
// fail loudly: it turns a working customer endpoint into "projectId is
// required".

// aliasedURL builds a request against the running server while presenting the
// Host header a customer's DNS would produce.
func aliasedRequest(t *testing.T, addr, host, path, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Host = host
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Transport: &http.Transport{}, Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(respBody)
}

// aliasedConfig wires the counting upstream behind a project and applies the
// given aliasing rules.
func aliasedConfig(u *countingUpstream, rules ...*common.AliasingRuleConfig) *common.Config {
	cfg := transportTestConfig(u)
	cfg.Server.Aliasing = &common.AliasingConfig{Rules: rules}
	return cfg
}

// TestHttpServer_AnAliasedHostnameServesItsNetworkWithNoPathAtAll is the whole
// point of the feature. The customer sends POST / and must be routed to the
// project, architecture and chain the rule names.
func TestHttpServer_AnAliasedHostnameServesItsNetworkWithNoPathAtAll(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := aliasedConfig(up, &common.AliasingRuleConfig{
		MatchDomain:       "eth.example.com",
		ServeProject:      "test_http",
		ServeArchitecture: "evm",
		ServeChain:        "123",
	})
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	// The Host arrives with a port in practice; the rule matches the hostname,
	// so the port must be stripped before matching.
	resp, body := aliasedRequest(t, addr, "eth.example.com:8080", "/", balanceCall)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Contains(t, body, "0xabc123")
	require.Equal(t, 1, up.count("eth_getBalance"))
}

// TestHttpServer_AHostnameNoRuleMatchesIsStillRefused is the negative control
// for the test above. Without it, a rule engine that matched everything would
// look identical.
func TestHttpServer_AHostnameNoRuleMatchesIsStillRefused(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := aliasedConfig(up, &common.AliasingRuleConfig{
		MatchDomain:       "eth.example.com",
		ServeProject:      "test_http",
		ServeArchitecture: "evm",
		ServeChain:        "123",
	})
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	resp, body := aliasedRequest(t, addr, "unknown.example.com", "/", balanceCall)

	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "body was %s", body)
	require.Zero(t, up.count("eth_getBalance"),
		"an unrouted hostname must not reach any upstream")
}

// TestHttpServer_ABrokenAliasingRuleDoesNotShadowTheRulesBehindIt covers the
// match-error branch. A rule whose pattern does not compile must be logged and
// skipped; if it aborted the loop instead, one typo in one rule would take every
// customer hostname listed after it offline, and the config would still load.
func TestHttpServer_ABrokenAliasingRuleDoesNotShadowTheRulesBehindIt(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := aliasedConfig(up,
		// An unbalanced parenthesis is a match expression that cannot be
		// compiled, which is how WildcardMatch reports an error at all.
		&common.AliasingRuleConfig{
			MatchDomain:       "(eth.example.com",
			ServeProject:      "test_http",
			ServeArchitecture: "evm",
			ServeChain:        "123",
		},
		&common.AliasingRuleConfig{
			MatchDomain:       "eth.example.com",
			ServeProject:      "test_http",
			ServeArchitecture: "evm",
			ServeChain:        "123",
		},
	)
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	resp, body := aliasedRequest(t, addr, "eth.example.com", "/", balanceCall)

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"the rule after the broken one must still be evaluated; body was %s", body)
	require.Equal(t, 1, up.count("eth_getBalance"))
}

// TestHttpServer_AProjectOnlyAliasStillTakesTheChainFromThePath covers the
// partial alias. An operator who serves many chains from one hostname aliases
// the project only and keeps /<architecture>/<chainId> in the path.
func TestHttpServer_AProjectOnlyAliasStillTakesTheChainFromThePath(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := aliasedConfig(up, &common.AliasingRuleConfig{
		MatchDomain:  "multi.example.com",
		ServeProject: "test_http",
	})
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	resp, body := aliasedRequest(t, addr, "multi.example.com", "/evm/123", balanceCall)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Contains(t, body, "0xabc123")
}

//
// --- CORS ---
//

// corsRequest issues one request with an Origin, using the given method.
func corsRequest(t *testing.T, method, url, origin, body string) (*http.Response, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)

	client := &http.Client{Transport: &http.Transport{}, Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(respBody)
}

func corsConfig(u *countingUpstream, cors *common.CORSConfig) *common.Config {
	cfg := transportTestConfig(u)
	cfg.Projects[0].CORS = cors
	return cfg
}

// TestHttpServer_AnAllowedPreflightAdvertisesTheWholeConfiguredPolicy checks the
// fields a browser reads and then caches. Max-Age in particular is a promise the
// browser keeps for that many seconds — dropping it does not break anything
// visibly, it just makes every call preflight again and doubles the request rate
// against the upstream budget.
func TestHttpServer_AnAllowedPreflightAdvertisesTheWholeConfiguredPolicy(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := corsConfig(up, &common.CORSConfig{
		AllowedOrigins:   []string{"https://app.example.com"},
		AllowedMethods:   []string{"POST", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type", "Authorization"},
		ExposedHeaders:   []string{"X-ERPC-Upstream"},
		AllowCredentials: util.BoolPtr(true),
		MaxAge:           600,
	})
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	url := fmt.Sprintf("http://%s/test_http/evm/123", addr)
	resp, _ := corsRequest(t, http.MethodOptions, url, "https://app.example.com", "")

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Equal(t, "https://app.example.com", resp.Header.Get("Access-Control-Allow-Origin"))
	require.Equal(t, "POST, OPTIONS", resp.Header.Get("Access-Control-Allow-Methods"))
	require.Equal(t, "Content-Type, Authorization", resp.Header.Get("Access-Control-Allow-Headers"))
	require.Equal(t, "X-ERPC-Upstream", resp.Header.Get("Access-Control-Expose-Headers"))
	require.Equal(t, "true", resp.Header.Get("Access-Control-Allow-Credentials"))
	require.Equal(t, "600", resp.Header.Get("Access-Control-Max-Age"))
	require.Zero(t, up.count("eth_getBalance"), "a preflight carries no call to route")
}

// TestHttpServer_AnAllowedPreflightOmitsCredentialsWhenUnset is the
// counterpart. Access-Control-Allow-Credentials must never appear unasked: with
// it, a browser attaches the caller's cookies to every cross-site call to this
// endpoint. Max-Age is checked here too, because an unset value is not omitted —
// config defaults it to 3600, and that default reaching the wire is the
// behaviour an operator actually sees.
func TestHttpServer_AnAllowedPreflightOmitsCredentialsWhenUnset(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := corsConfig(up, &common.CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"POST"},
	})
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	url := fmt.Sprintf("http://%s/test_http/evm/123", addr)
	resp, _ := corsRequest(t, http.MethodOptions, url, "https://app.example.com", "")

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Equal(t, "https://app.example.com", resp.Header.Get("Access-Control-Allow-Origin"))
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Credentials"),
		"cookies must not be invited by a policy that never asked for them")
	require.Equal(t, "3600", resp.Header.Get("Access-Control-Max-Age"),
		"an unset maxAge is defaulted, not dropped")
}

// TestHttpServer_ACallFromADisallowedOriginIsStillServed pins a deliberate
// design choice that reads like a bug if you only look at the status code. eRPC
// does not use cookies, so CORS here is a browser-side convenience, not an
// access control: a disallowed origin gets no Access-Control-Allow-* headers —
// which is what stops the browser — but the call itself is served, because
// non-browser clients are legitimate and cannot be distinguished anyway.
func TestHttpServer_ACallFromADisallowedOriginIsStillServed(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := corsConfig(up, &common.CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"POST"},
	})
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	url := fmt.Sprintf("http://%s/test_http/evm/123", addr)
	resp, body := corsRequest(t, http.MethodPost, url, "https://elsewhere.example.com", balanceCall)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Contains(t, body, "0xabc123")
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"),
		"the browser must not be told this origin is allowed")
	require.Equal(t, 1, up.count("eth_getBalance"))
}

// TestHttpServer_APreflightFromADisallowedOriginIsAnsweredWithoutPolicy covers
// the OPTIONS half of the same rule: answer the preflight so the client is not
// left hanging, but attach nothing the browser could read as permission.
func TestHttpServer_APreflightFromADisallowedOriginIsAnsweredWithoutPolicy(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := corsConfig(up, &common.CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"POST"},
	})
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	url := fmt.Sprintf("http://%s/test_http/evm/123", addr)
	resp, _ := corsRequest(t, http.MethodOptions, url, "https://elsewhere.example.com", "")

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Methods"))
}

// TestHttpServer_ARequestWithNoOriginSkipsCorsEntirely covers the header eRPC's
// own clients never send. Server-side callers, health probes and the CLI have no
// Origin; enforcing a policy on them would break every non-browser integration
// the moment an operator turned CORS on for their frontend.
func TestHttpServer_ARequestWithNoOriginSkipsCorsEntirely(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := corsConfig(up, &common.CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"POST"},
	})
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	url := fmt.Sprintf("http://%s/test_http/evm/123", addr)
	resp, body := postRaw(t, url, []byte(balanceCall), nil)

	require.Equal(t, http.StatusOK, resp.StatusCode, "body was %s", body)
	require.Contains(t, body, "0xabc123")
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	require.Equal(t, 1, up.count("eth_getBalance"))
}

// TestHttpServer_TheAdminEndpointEnforcesItsOwnCorsPolicy covers the separate
// admin CORS block. Admin has its own config for a reason — the origins allowed
// to read a project's data are not the origins allowed to reconfigure the node —
// so it must not silently inherit the project policy or skip the check.
func TestHttpServer_TheAdminEndpointEnforcesItsOwnCorsPolicy(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()

	up := newCountingUpstream(t)
	cfg := transportTestConfig(up)
	cfg.Admin = &common.AdminConfig{
		CORS: &common.CORSConfig{
			AllowedOrigins: []string{"https://console.example.com"},
			AllowedMethods: []string{"POST"},
			MaxAge:         300,
		},
	}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	url := fmt.Sprintf("http://%s/admin", addr)

	allowed, _ := corsRequest(t, http.MethodOptions, url, "https://console.example.com", "")
	require.Equal(t, http.StatusNoContent, allowed.StatusCode)
	require.Equal(t, "https://console.example.com", allowed.Header.Get("Access-Control-Allow-Origin"))
	require.Equal(t, "300", allowed.Header.Get("Access-Control-Max-Age"))

	denied, _ := corsRequest(t, http.MethodOptions, url, "https://elsewhere.example.com", "")
	require.Equal(t, http.StatusNoContent, denied.StatusCode)
	require.Empty(t, denied.Header.Get("Access-Control-Allow-Origin"),
		"an origin the admin policy does not name must get no admin CORS grant")
}
