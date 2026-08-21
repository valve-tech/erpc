package data

import (
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestSharedStateRegistry_FetchValue pins how a remote counter is read back.
// The value it returns seeds a node's view of the cluster's latest/finalized
// block, so a silent zero here would make a fresh pod believe the chain is at
// block 0 and re-serve stale data.
func TestSharedStateRegistry_FetchValue(t *testing.T) {
	t.Run("parses a stored integer", func(t *testing.T) {
		registry, connector, ctx := setupTest("fetch-ok")
		connector.On("Get", mock.Anything, ConnectorMainIndex, "fetch-ok/latest", "value", nil).
			Return([]byte("123456"), nil)

		got, err := registry.fetchValue(ctx, "fetch-ok/latest")
		require.NoError(t, err)
		assert.Equal(t, int64(123456), got, "the stored number must reach the caller unchanged")
	})

	t.Run("an empty payload reads as zero", func(t *testing.T) {
		registry, connector, ctx := setupTest("fetch-empty")
		connector.On("Get", mock.Anything, ConnectorMainIndex, "fetch-empty/latest", "value", nil).
			Return([]byte(""), nil)

		got, err := registry.fetchValue(ctx, "fetch-empty/latest")
		require.NoError(t, err)
		assert.Equal(t, int64(0), got)
	})

	t.Run("an unparsable payload is an error, not a silent zero", func(t *testing.T) {
		registry, connector, ctx := setupTest("fetch-bad")
		connector.On("Get", mock.Anything, ConnectorMainIndex, "fetch-bad/latest", "value", nil).
			Return([]byte("not-a-number"), nil)

		_, err := registry.fetchValue(ctx, "fetch-bad/latest")
		require.Error(t, err, "a corrupt counter must surface, not be read as block 0")
	})

	t.Run("a connector error is propagated", func(t *testing.T) {
		registry, connector, ctx := setupTest("fetch-err")
		boom := errors.New("connector down")
		connector.On("Get", mock.Anything, ConnectorMainIndex, "fetch-err/latest", "value", nil).
			Return([]byte(nil), boom)

		_, err := registry.fetchValue(ctx, "fetch-err/latest")
		require.Error(t, err)
		assert.ErrorIs(t, err, boom, "the connector's own error must reach the caller")
	})

	t.Run("a record-not-found is propagated as such", func(t *testing.T) {
		registry, connector, ctx := setupTest("fetch-missing")
		notFound := common.NewErrRecordNotFound("fetch-missing/latest", "value", MemoryDriverName)
		connector.On("Get", mock.Anything, ConnectorMainIndex, "fetch-missing/latest", "value", nil).
			Return([]byte(nil), notFound)

		_, err := registry.fetchValue(ctx, "fetch-missing/latest")
		require.Error(t, err)
		assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound),
			"callers distinguish 'never written' from 'read failed'")
	})
}

// TestSharedStateRegistry_TimeoutAccessors pins the two timeouts callers read
// off the registry. GetFallbackTimeout bounds how long a caller waits on the
// shared store before falling back to its local value, so a wrong number here
// either stalls the request path or abandons a healthy store too early.
func TestSharedStateRegistry_TimeoutAccessors(t *testing.T) {
	registry, _, _ := setupTest("timeouts")

	assert.Equal(t, 500*time.Millisecond, registry.GetFallbackTimeout(),
		"the configured fallback timeout must be reported unchanged")
	assert.Equal(t, 2*time.Second, registry.GetLockTtl(),
		"the configured lock TTL must be reported unchanged")
}
