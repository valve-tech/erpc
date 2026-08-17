package evm

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// NormalizedResponse.JsonRpcResponse() answers (nil, nil) for a nil response,
// so shouldCacheResponse can receive a nil resp and a nil rpcResp together. Its
// own isEmpty expression says so. ResultLength and GetResultBytes both lock the
// receiver, so a nil there panics rather than reaching that expression.
func TestShouldCacheResponse_NilResponseIsEmptyNotAPanic(t *testing.T) {
	ctx := context.Background()
	lg := log.Logger
	ttl := time.Minute

	newPolicy := func(t *testing.T, empty common.CacheEmptyBehavior) *data.CachePolicy {
		t.Helper()
		p, err := data.NewCachePolicy(&common.CachePolicyConfig{
			Connector: "mock-connector",
			TTL:       common.FixedDuration(ttl),
			Network:   "*",
			Method:    "eth_getBlockByNumber",
			Finality:  common.DataFinalityStateUnknown,
			Empty:     empty,
		}, &data.MockConnector{})
		require.NoError(t, err)
		return p
	}

	t.Run("IgnoreRefusesToCacheIt", func(t *testing.T) {
		got, err := shouldCacheResponse(ctx, lg, nil, nil,
			newPolicy(t, common.CacheEmptyBehaviorIgnore), common.DataFinalityStateUnknown)
		require.NoError(t, err)
		assert.False(t, got, "a nil response carries no result, so it is empty")
	})

	// Discriminating: the two behaviours must disagree. If the nil case were
	// short-circuited to a blanket false, this half would pass for the wrong
	// reason.
	t.Run("OnlyTreatsItAsEmptyAndCachesIt", func(t *testing.T) {
		got, err := shouldCacheResponse(ctx, lg, nil, nil,
			newPolicy(t, common.CacheEmptyBehaviorOnly), common.DataFinalityStateUnknown)
		require.NoError(t, err)
		assert.True(t, got, "empty-only must see a nil response as empty")
	})
}
