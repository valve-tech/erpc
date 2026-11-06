package erpc

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/indexer"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// A client that never reads must get a 4013 close once the process's byte
// budget is spent, not a silent drop.
func TestSlowSubscriberGets4013OnBudget(t *testing.T) {
	const payloadSize = 200 << 10
	filler := strings.Repeat("a", payloadSize)
	stop := make(chan struct{})
	defer close(stop)

	mockUpstream := mockWsUpstream(t, func(conn *websocket.Conn) {
		standardMockWsHandler(conn, func(method string, id interface{}, req map[string]interface{}) {
			switch method {
			case "eth_subscribe":
				subID := "0xflood"
				_ = mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": subID})
				go func() {
					// Distinct heads, so the indexer's dedup passes every one.
					for n := 1; ; n++ {
						select {
						case <-stop:
							return
						default:
						}
						if mockWriteJSON(conn, map[string]interface{}{
							"jsonrpc": "2.0",
							"method":  "eth_subscription",
							"params": map[string]interface{}{
								"subscription": subID,
								"result": map[string]interface{}{
									"number": fmt.Sprintf("0x%x", n),
									"hash":   fmt.Sprintf("0x%064x", n),
									"extra":  filler,
								},
							},
						}) != nil {
							return
						}
					}
				}()
			default:
				_ = mockWriteJSON(conn, map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": "0x1"})
			}
		})
	})
	defer mockUpstream.Close()

	setupGock()
	defer util.ResetGock()

	before := testutil.ToFloat64(telemetry.MetricWsBudgetClosesTotal.WithLabelValues("host_budget"))

	cfg := standardWsConfig("ws" + strings.TrimPrefix(mockUpstream.URL, "http"))
	cfg.Server.WebSocket = &common.WebSocketServerConfig{NotifyBudgetBytes: 64 << 20}
	addr, cleanup := setupTestERPCServer(t, cfg)
	defer cleanup()
	time.Sleep(2 * time.Second)

	conn := dialWs(t, addr)
	defer func() { _ = conn.Close() }()
	conn.SetReadLimit(-1)
	resp := sendAndReceive(t, conn, `{"jsonrpc":"2.0","id":1,"method":"eth_subscribe","params":["newHeads"]}`)
	require.NotNil(t, resp["result"])

	// Do not read: let the queue grow until the budget closes us.
	deadline := time.Now().Add(60 * time.Second)
	for testutil.ToFloat64(telemetry.MetricWsBudgetClosesTotal.WithLabelValues("host_budget")) == before {
		if time.Now().After(deadline) {
			t.Fatal("budget never closed the slow subscriber")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Now read through what was buffered to the close frame.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var ce *websocket.CloseError
		require.True(t, errors.As(err, &ce), "want a close frame, got %v", err)
		require.Equal(t, SlowSubscriberCloseCode, ce.Code)
		require.Equal(t, "subscriber too slow; resubscribe and back-fill", ce.Text)
		break
	}
	require.Equal(t, before+1, testutil.ToFloat64(telemetry.MetricWsBudgetClosesTotal.WithLabelValues("host_budget")))
}

func withCapturedLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, prevLevel := log.Logger, zerolog.GlobalLevel()
	log.Logger = zerolog.New(&buf)
	zerolog.SetGlobalLevel(zerolog.WarnLevel)
	t.Cleanup(func() {
		log.Logger = prev
		zerolog.SetGlobalLevel(prevLevel)
	})
	return &buf
}

func TestRetiredCloseSlowSubscribersWarns(t *testing.T) {
	buf := withCapturedLog(t)
	s := &common.ServerConfig{WebSocket: &common.WebSocketServerConfig{CloseSlowSubscribers: true}}
	require.NoError(t, s.SetDefaults())
	require.Equal(t, 1, strings.Count(buf.String(), "closeSlowSubscribers is retired"))

	buf.Reset()
	s = &common.ServerConfig{}
	require.NoError(t, s.SetDefaults())
	require.NotContains(t, buf.String(), "closeSlowSubscribers")
}

func TestNotifyBudgetClamped(t *testing.T) {
	cases := []struct {
		in, want int64
	}{
		{0, 1 << 30},
		{1, 64 << 20},
		{1 << 40, 16 << 30},
		{256 << 20, 256 << 20},
	}
	for _, c := range cases {
		s := &common.ServerConfig{WebSocket: &common.WebSocketServerConfig{NotifyBudgetBytes: c.in}}
		require.NoError(t, s.SetDefaults())
		require.Equal(t, c.want, s.WebSocket.NotifyBudgetBytes, "notifyBudgetBytes %d", c.in)
	}
}

func TestNotifyFloorAndGasLimitClamped(t *testing.T) {
	s := &common.ServerConfig{WebSocket: &common.WebSocketServerConfig{NotifyBufferSize: 5_000_000, FallbackGasLimit: 1}}
	require.NoError(t, s.SetDefaults())
	require.Equal(t, 1_000_000, s.WebSocket.NotifyBufferSize)
	require.Equal(t, uint64(30_000_000), s.WebSocket.FallbackGasLimit)

	s = &common.ServerConfig{WebSocket: &common.WebSocketServerConfig{NotifyBufferSize: -1, FallbackGasLimit: 1 << 40}}
	require.NoError(t, s.SetDefaults())
	require.Equal(t, 0, s.WebSocket.NotifyBufferSize)
	require.Equal(t, uint64(1_000_000_000), s.WebSocket.FallbackGasLimit)

	s = &common.ServerConfig{}
	require.NoError(t, s.SetDefaults())
	require.Equal(t, 0, s.WebSocket.NotifyBufferSize)
	require.Equal(t, uint64(60_000_000), s.WebSocket.FallbackGasLimit)
}

// The cap is two blocks at the protocol maximum, and follows the live gas
// limit.
func TestQueueCapIsTwoProtocolBlocks(t *testing.T) {
	lg := zerolog.Nop()
	sm := NewSubscriptionManager(&lg, newTestIndexerForCap())
	require.Equal(t, 2*160_000, sm.queueCap("evm:1"))
	sm.idx.GasLimits().Observe("evm:1", []byte(`{"gasLimit":"0x391bd79"}`))
	require.Equal(t, 2*159_688, sm.queueCap("evm:1"))
	require.Equal(t, float64(2*159_688), testutil.ToFloat64(telemetry.MetricWsQueueCap.WithLabelValues("evm:1")))
}

func newTestIndexerForCap() *indexer.Indexer {
	lg := zerolog.Nop()
	return indexer.New(&lg, indexer.Options{})
}
