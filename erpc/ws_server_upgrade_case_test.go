package erpc

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/require"
)

// RFC 6455 sends Upgrade/Connection as tokens, and tokens are case-insensitive:
// "Upgrade: WebSocket" is exactly as valid as "Upgrade: websocket". gorilla
// agrees — IsWebSocketUpgrade compares with equalASCIIFold — but the chain used
// to decide this three different ways: parseUrlPath and TimeoutHandler each did
// a case-sensitive == "websocket" against the raw header, while the dispatcher
// used IsWebSocketUpgrade.
//
// parseUrlPath is the one that decided the outcome, and it fails quietly rather
// than loudly: any non-POST it does not recognise as an upgrade is reclassified
// as a healthcheck, so a mixed-case upgrade was answered 200 with a JSON health
// body and never reached the upgrade path at all. The client just sees a failed
// handshake against a server that looks healthy.
//
// Dialed raw because gorilla's client always spells the token lowercase and
// cannot produce this request.
func TestWebSocketUpgradeMixedCaseToken(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	setupGock()

	cfg := standardWsConfig("ws://rpc2.localhost")
	cfg.Server.EnableGzip = util.BoolPtr(true)

	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))

	// A valid upgrade in every respect except the spelling of two tokens.
	req := "GET /test_ws/evm/123 HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: WebSocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n" +
		"\r\n"
	_, err = conn.Write([]byte(req))
	require.NoError(t, err)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Include the body on failure: the pre-fix answer is a 200 whose body is a
	// health report, which is the tell that this was misrouted rather than
	// refused, and is otherwise an easy failure to misread.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	require.Equalf(t, http.StatusSwitchingProtocols, resp.StatusCode,
		"mixed-case upgrade token must upgrade, got %d: %s", resp.StatusCode, string(body))
}
