package erpc

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
)

func servedBy(t *testing.T, id string) *common.NormalizedResponse {
	t.Helper()
	return routedResponse(t, "eth_call").SetUpstream(common.NewFakeUpstream(id))
}

func servedFromCache(t *testing.T) *common.NormalizedResponse {
	t.Helper()
	return routedResponse(t, "eth_call").SetFromCache(true)
}

func TestBatchServedHeader(t *testing.T) {
	ctx := context.Background()
	items := func() []interface{} {
		return []interface{}{
			servedBy(t, "up-a"),
			servedBy(t, "up-a"),
			servedFromCache(t),
			servedBy(t, "up-b"),
			&HttpJsonRpcErrorResponse{Cause: errors.New("boom")},
		}
	}

	t.Run("AllModeNamesEachItem", func(t *testing.T) {
		w := httptest.NewRecorder()
		costTestServer(&common.ServerConfig{}).writeBatchExecHeaders(ctx, w, items())
		assert.Equal(t, "up-a;up-b", w.Header().Get("X-ERPC-Served-Ids"))
		assert.Equal(t, "0,0,c,1,-", w.Header().Get("X-ERPC-Served"))
	})

	t.Run("SummaryModeKeepsBoth", func(t *testing.T) {
		summary := common.ExecutionHeadersSummary
		w := httptest.NewRecorder()
		costTestServer(&common.ServerConfig{ExecutionHeaders: &summary}).writeBatchExecHeaders(ctx, w, items())
		assert.Equal(t, "up-a;up-b", w.Header().Get("X-ERPC-Served-Ids"))
		assert.Equal(t, "0,0,c,1,-", w.Header().Get("X-ERPC-Served"))
	})

	t.Run("OffModeSetsNeither", func(t *testing.T) {
		off := common.ExecutionHeadersOff
		w := httptest.NewRecorder()
		costTestServer(&common.ServerConfig{ExecutionHeaders: &off}).writeBatchExecHeaders(ctx, w, items())
		assert.Empty(t, w.Header().Get("X-ERPC-Served-Ids"))
		assert.Empty(t, w.Header().Get("X-ERPC-Served"))
	})

	t.Run("AllCacheHasNoIds", func(t *testing.T) {
		w := httptest.NewRecorder()
		costTestServer(&common.ServerConfig{}).writeBatchExecHeaders(ctx, w, []interface{}{
			servedFromCache(t), servedFromCache(t), servedFromCache(t), servedFromCache(t), servedFromCache(t),
		})
		assert.Equal(t, "c,c,c,c,c", w.Header().Get("X-ERPC-Served"))
		_, present := w.Header()["X-Erpc-Served-Ids"]
		assert.False(t, present)
	})
}
