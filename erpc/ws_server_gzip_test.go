package erpc

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// A WebSocket upgrade arrives through the same handler chain as every other
// request: TimeoutHandler -> gzipHandler -> createRequestHandler. Both of the
// outer two wrap http.ResponseWriter, and gorilla's Upgrade() needs the writer
// it is handed to be an http.Hijacker (server.go:175, a plain type assertion
// with no Unwrap() fallback). TimeoutHandler already steps aside for upgrades;
// gzipHandler did not, so it replaced the writer with one that cannot hijack
// and the upgrade came back 500.
//
// Nothing in the existing WS suite caught this because gorilla's dialer writes
// its request with req.Write() instead of going through http.Transport, so it
// never sends the Accept-Encoding: gzip that http.Transport adds for free.
// Browsers send it, and so does every reverse proxy. These tests set it by hand.
func TestWebSocketUpgradeBehindGzip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	setupGock()

	cfg := standardWsConfig("ws://rpc2.localhost")
	// Explicit rather than inherited: enableGzip already defaults to true
	// (common/defaults.go), so this is the shipped configuration, but a test
	// for gzip should not go quiet if that default ever flips.
	cfg.Server.EnableGzip = util.BoolPtr(true)

	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	header := http.Header{}
	header.Set("Accept-Encoding", "gzip")

	conn, resp, err := websocket.DefaultDialer.Dial(
		fmt.Sprintf("ws://%s/test_ws/evm/123", addr), header)
	if resp != nil {
		defer resp.Body.Close()
	}
	// Report the status on failure: the pre-fix behaviour is a 500 carrying
	// gorilla's "response does not implement http.Hijacker", and naming it
	// keeps a future regression from being mistaken for an unrelated refusal.
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	require.NoErrorf(t, err,
		"a client that accepts gzip must still be able to upgrade (status %d)", status)
	require.Equal(t, http.StatusSwitchingProtocols, status)
	defer conn.Close()
}

// The same request without Accept-Encoding is the path the rest of the suite
// already exercises. It is here so a failure can be attributed: if both tests
// go red the upgrade is broken generally, if only the one above does then the
// gzip wrapper is the difference.
func TestWebSocketUpgradeWithoutGzip(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	setupGock()

	cfg := standardWsConfig("ws://rpc2.localhost")
	cfg.Server.EnableGzip = util.BoolPtr(true)

	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	conn, resp, err := websocket.DefaultDialer.Dial(
		fmt.Sprintf("ws://%s/test_ws/evm/123", addr), nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	defer conn.Close()
}
