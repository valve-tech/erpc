package erpc

import (
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// parseUrlPath decides which project and which chain a request is for. It reads
// the path and the domain-aliasing preselection together, and every combination
// of the two is a separate routing rule. Getting one wrong sends a caller's
// request to another chain, which answers successfully with the wrong data —
// the failure mode no client can detect.
//
// TestHttpServer_ParseUrlPath covers the ordinary combinations. This table
// covers the rest: the alias lookups, the explicit overrides that are allowed
// on top of a preselection, and the "too many segments" refusals.

// aliasingUrlServer is the minimal server parseUrlPath needs: one project whose
// registry knows the alias "arbitrum".
func aliasingUrlServer() *HttpServer {
	return &HttpServer{
		draining: &atomic.Bool{},
		erpc: &ERPC{
			projectsRegistry: &ProjectsRegistry{
				preparedProjects: map[string]*PreparedProject{
					"myproject": {
						networksRegistry: &NetworksRegistry{
							aliasMu: &sync.RWMutex{},
							aliasToNetworkId: map[string]aliasEntry{
								"arbitrum": {architecture: "evm", chainID: "123"},
							},
						},
					},
				},
			},
		},
	}
}

func TestHttpServer_ParseUrlPath_PreselectionPermutations(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		method      string
		preProject  string
		preArch     string
		preChain    string
		wantProject string
		wantArch    string
		wantChain   string
		wantHealth  bool
		wantErr     bool
		errContains string
	}{
		// --- Nothing preselected: the second segment may be a network alias ---
		{
			name:        "alias in the path resolves to its architecture and chain",
			path:        "/myproject/arbitrum",
			method:      "POST",
			wantProject: "myproject",
			wantArch:    "evm",
			wantChain:   "123",
		},
		{
			name:        "an unknown project cannot resolve an alias, so the segment is the architecture",
			path:        "/otherproject/evm",
			method:      "POST",
			wantProject: "otherproject",
			wantArch:    "evm",
		},

		{
			name:        "an unaliased request with no path has nothing to route on",
			path:        "",
			method:      "OPTIONS",
			wantErr:     true,
			errContains: "must provide /<project>/<architecture>/<chainId>",
		},

		// --- Project preselected by domain ---
		{
			name:        "project alias plus a network alias in the path",
			path:        "/arbitrum",
			method:      "POST",
			preProject:  "myproject",
			wantProject: "myproject",
			wantArch:    "evm",
			wantChain:   "123",
		},
		{
			name:        "project alias still allows a fully explicit path to override it",
			path:        "/otherproject/evm/999",
			method:      "POST",
			preProject:  "myproject",
			wantProject: "otherproject",
			wantArch:    "evm",
			wantChain:   "999",
		},
		{
			name:        "project alias refuses a request with no path",
			path:        "",
			method:      "OPTIONS",
			preProject:  "myproject",
			wantErr:     true,
			errContains: "for project-only alias must provide /<architecture>/<chainId>",
		},
		{
			name:        "project alias refuses four segments",
			path:        "/a/b/c/d",
			method:      "POST",
			preProject:  "myproject",
			wantErr:     true,
			errContains: "for project-only alias must only provide",
		},
		{
			name:       "project alias serves a healthcheck with nothing on the path",
			path:       "/healthcheck",
			method:     "GET",
			preProject: "myproject",
			// A healthcheck needs no chain, so the empty preselection stands.
			wantProject: "myproject",
			wantHealth:  true,
		},

		// --- Project and architecture preselected ---
		{
			name:        "project-and-architecture alias allows a fully explicit override",
			path:        "/otherproject/evm/999",
			method:      "POST",
			preProject:  "myproject",
			preArch:     "evm",
			wantProject: "otherproject",
			wantArch:    "evm",
			wantChain:   "999",
		},
		{
			name:        "project-and-architecture alias refuses two segments",
			path:        "/evm/123",
			method:      "POST",
			preProject:  "myproject",
			preArch:     "evm",
			wantErr:     true,
			errContains: "for project-and-architecture alias must only provide",
		},
		{
			// An empty path is what "OPTIONS *" produces — a legal HTTP/1.1
			// request-target that leaves URL.Path empty and therefore leaves
			// no segments at all. It is the only way to reach these arms.
			name:        "project-and-architecture alias refuses a request with no path",
			path:        "",
			method:      "OPTIONS",
			preProject:  "myproject",
			preArch:     "evm",
			wantErr:     true,
			errContains: "for project-and-architecture alias must provide /<chainId>",
		},
		{
			name:        "project-and-architecture alias needs a chain on a POST",
			path:        "/",
			method:      "POST",
			preProject:  "myproject",
			preArch:     "evm",
			wantErr:     true,
			errContains: "must provide /<chainId>",
		},

		// --- Everything preselected ---
		{
			name:        "a fully aliased domain refuses extra path segments",
			path:        "/evm/123",
			method:      "POST",
			preProject:  "myproject",
			preArch:     "evm",
			preChain:    "123",
			wantErr:     true,
			errContains: "must not provide anything on the path",
		},

		// --- Architecture and chain preselected ---
		{
			name:        "architecture-and-chain alias takes the project from the path",
			path:        "/myproject",
			method:      "POST",
			preArch:     "evm",
			preChain:    "123",
			wantProject: "myproject",
			wantArch:    "evm",
			wantChain:   "123",
		},
		{
			name:        "architecture-and-chain alias allows a fully explicit override",
			path:        "/otherproject/evm/999",
			method:      "POST",
			preArch:     "evm",
			preChain:    "123",
			wantProject: "otherproject",
			wantArch:    "evm",
			wantChain:   "999",
		},
		{
			name:        "architecture-and-chain alias refuses two segments",
			path:        "/a/b",
			method:      "POST",
			preArch:     "evm",
			preChain:    "123",
			wantErr:     true,
			errContains: "for architecture-and-chain alias must only provide",
		},
		{
			name:        "architecture-and-chain alias needs a project on a POST",
			path:        "/",
			method:      "POST",
			preArch:     "evm",
			preChain:    "123",
			wantErr:     true,
			errContains: "for architecture-and-chain alias must provide /<project>",
		},
		{
			name:        "architecture-and-chain alias refuses a request with no path",
			path:        "",
			method:      "OPTIONS",
			preArch:     "evm",
			preChain:    "123",
			wantErr:     true,
			errContains: "for architecture-and-chain alias must provide /<project>",
		},

		// --- Architecture only preselected ---
		{
			name:        "architecture-only alias takes the project from one segment",
			path:        "/myproject",
			method:      "POST",
			preArch:     "evm",
			wantProject: "myproject",
			wantArch:    "evm",
		},
		{
			name:        "architecture-only alias takes project and chain from two segments",
			path:        "/myproject/123",
			method:      "POST",
			preArch:     "evm",
			wantProject: "myproject",
			wantArch:    "evm",
			wantChain:   "123",
		},
		{
			name:        "architecture-only alias allows a fully explicit override",
			path:        "/otherproject/evm/999",
			method:      "POST",
			preArch:     "evm",
			wantProject: "otherproject",
			wantArch:    "evm",
			wantChain:   "999",
		},
		{
			name:        "architecture-only alias refuses four segments",
			path:        "/a/b/c/d",
			method:      "POST",
			preArch:     "evm",
			wantErr:     true,
			errContains: "for architecture-only alias must only provide",
		},
		{
			name:        "architecture-only alias refuses a request with no path",
			path:        "",
			method:      "OPTIONS",
			preArch:     "evm",
			wantErr:     true,
			errContains: "for architecture-only alias must provide /<project>",
		},
		{
			// "/" leaves one empty segment, not zero, so this lands on the
			// one-segment arm and reports the generic refusal instead.
			name:        "architecture-only alias with a bare slash reports the generic refusal",
			path:        "/",
			method:      "POST",
			preArch:     "evm",
			wantErr:     true,
			errContains: "project is required either in path or via domain aliasing",
		},

		// --- Validation that runs after every branch ---
		{
			name:        "an architecture this build does not serve is refused",
			path:        "/myproject/notachain/1",
			method:      "POST",
			wantErr:     true,
			errContains: "architecture is not valid",
		},
		{
			name:        "a preselected architecture is validated the same way",
			path:        "/myproject",
			method:      "POST",
			preArch:     "notachain",
			wantErr:     true,
			errContains: "architecture is not valid",
		},
		{
			name:        "a trailing slash is cleaned away rather than counted as a segment",
			path:        "/myproject/evm/123/",
			method:      "POST",
			wantProject: "myproject",
			wantArch:    "evm",
			wantChain:   "123",
		},

		// --- Method decides what a non-POST path means ---
		{
			name:        "a plain GET on a full path is a healthcheck, not a call",
			path:        "/myproject/evm/123",
			method:      "GET",
			wantProject: "myproject",
			wantArch:    "evm",
			wantChain:   "123",
			wantHealth:  true,
		},
		{
			name:        "an OPTIONS preflight is not a healthcheck",
			path:        "/myproject/evm/123",
			method:      "OPTIONS",
			wantProject: "myproject",
			wantArch:    "evm",
			wantChain:   "123",
		},
		{
			name:        "a GET to /admin is not the admin endpoint",
			path:        "/admin",
			method:      "GET",
			wantProject: "admin",
			wantHealth:  true,
		},
	}

	s := aliasingUrlServer()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{Method: tt.method, URL: &url.URL{Path: tt.path}}

			project, arch, chain, isAdmin, isHealth, err := s.parseUrlPath(
				r, tt.preProject, tt.preArch, tt.preChain)

			if tt.wantErr {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errContains)
				require.Empty(t, project, "a refused path must not leak a routing decision")
				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.wantProject, project)
			require.Equal(t, tt.wantArch, arch)
			require.Equal(t, tt.wantChain, chain)
			require.False(t, isAdmin)
			require.Equal(t, tt.wantHealth, isHealth)
		})
	}
}

// TestHttpServer_ParseUrlPath_TreatsAWebSocketUpgradeAsACall covers the one
// non-POST that is not a healthcheck. Misjudging it does not raise an error —
// eRPC answers 200 with a health body and the upgrade never happens, so the
// client sees a connection that opened and then closed with no explanation.
func TestHttpServer_ParseUrlPath_TreatsAWebSocketUpgradeAsACall(t *testing.T) {
	s := aliasingUrlServer()

	upgrade := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: "/myproject/evm/123"},
		Header: http.Header{
			// RFC 6455 makes these case-insensitive tokens, and real clients
			// send every spelling. A raw == comparison misses "WebSocket".
			"Connection": []string{"keep-alive, Upgrade"},
			"Upgrade":    []string{"WebSocket"},
		},
	}
	_, _, _, _, isHealth, err := s.parseUrlPath(upgrade, "", "", "")
	require.NoError(t, err)
	require.False(t, isHealth, "an upgrade must reach the websocket handler, not the healthcheck")

	plain := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: "/myproject/evm/123"},
		Header: http.Header{"Connection": []string{"keep-alive"}},
	}
	_, _, _, _, isHealth, err = s.parseUrlPath(plain, "", "", "")
	require.NoError(t, err)
	require.True(t, isHealth, "a GET that is not an upgrade is a healthcheck")
}
