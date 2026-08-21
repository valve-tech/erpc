package data

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// lockedLogBuffer collects log output written from the connector's goroutines.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRedisReverseIndexTTL_DetectsAnExpiredTarget covers the TTL check the
// reverse-index lookup runs on the key it resolved. go-redis hands the -2 and
// -1 sentinels back as bare time.Duration values — -2ns and -1ns — not as
// seconds, so a comparison against -2*time.Second never matches and the check
// never runs. The lookup then spends a second round trip on a key it already
// knows is gone.
func TestRedisReverseIndexTTL_DetectsAnExpiredTarget(t *testing.T) {
	prev := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prev) })

	m, err := miniredis.Run()
	require.NoError(t, err)
	defer m.Close()

	var out lockedLogBuffer
	logger := zerolog.New(&out).Level(zerolog.TraceLevel)
	ctx := context.Background()

	cfg := &common.RedisConnectorConfig{
		Addr:        m.Addr(),
		InitTimeout: common.Duration(2 * time.Second),
		GetTimeout:  common.Duration(2 * time.Second),
		SetTimeout:  common.Duration(2 * time.Second),
	}
	require.NoError(t, cfg.SetDefaults())

	connector, err := NewRedisConnector(ctx, &logger, "test-reverse-ttl", cfg)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return connector.initializer.State() == util.StateReady
	}, 3*time.Second, 100*time.Millisecond, "connector did not become ready")

	rangeKey := "eth_getTransactionReceipt:0xdeadbeef"
	concretePartitionKey := "evm:123:latest"
	wildcardPartitionKey := "evm:123:*"

	require.NoError(t, connector.Set(ctx, concretePartitionKey, rangeKey, []byte("tx-receipt-value"), nil))

	// The control: while the target is present, the lookup resolves and the
	// value comes back.
	got, err := connector.Get(ctx, ConnectorReverseIndex, wildcardPartitionKey, rangeKey, nil)
	require.NoError(t, err)
	require.Equal(t, []byte("tx-receipt-value"), got)
	// The -1 sentinel shares the same bug: a key written without a TTL reports
	// -1ns, not -1s. Both branches have to read the same units.
	require.Contains(t, out.String(), "resolved key has no TTL (persistent)",
		"the -1 sentinel did not match, so the persistent-key branch never ran")

	// Now drop the target and leave the reverse index pointing at it — the
	// state a TTL expiry produces, because the index outlives the entry.
	m.Del(fmt.Sprintf("%s:%s", concretePartitionKey, rangeKey))
	require.True(t, m.Exists(fmt.Sprintf("%s#%s#%s", redisReverseIndexPrefix, wildcardPartitionKey, rangeKey)),
		"the reverse index must survive, or the test proves nothing about the TTL check")

	_, err = connector.Get(ctx, ConnectorReverseIndex, wildcardPartitionKey, rangeKey, nil)
	require.Error(t, err)
	require.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)

	// Discriminating: a plain miss returns the same error by falling through to
	// a GET of the key that is not there. Only the log line shows the TTL check
	// recognised the sentinel and stopped early.
	require.Contains(t, out.String(), "resolved key from reverse index no longer exists",
		"the -2 sentinel did not match, so the expired-target branch never ran")
}
