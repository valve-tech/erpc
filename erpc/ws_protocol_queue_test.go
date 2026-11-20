package erpc

import (
	"bytes"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

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

// The fork's notification-queue keys still parse, so an old config loads,
// but each one only warns: upstream's subscription writer has no budget or
// queue to size.
func TestRetiredNotifyKeysWarn(t *testing.T) {
	for name, ws := range map[string]*common.WebSocketServerConfig{
		"notifyBufferSize":     {NotifyBufferSize: 5_000},
		"notifyBudgetBytes":    {NotifyBudgetBytes: 256 << 20},
		"fallbackGasLimit":     {FallbackGasLimit: 30_000_000},
		"closeSlowSubscribers": {CloseSlowSubscribers: true},
	} {
		t.Run(name, func(t *testing.T) {
			buf := withCapturedLog(t)
			s := &common.ServerConfig{WebSocket: ws}
			require.NoError(t, s.SetDefaults())
			require.Equal(t, 1, strings.Count(buf.String(), "are retired and have no effect"))
		})
	}

	buf := withCapturedLog(t)
	s := &common.ServerConfig{}
	require.NoError(t, s.SetDefaults())
	require.NotContains(t, buf.String(), "retired")
}
