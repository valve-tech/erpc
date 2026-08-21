package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cache.Set reads the response through NormalizedResponse.JsonRpcResponse,
// which answers (nil, nil) both for a nil response and for one that has been
// released. Every reader downstream of that call therefore has to survive a nil
// *JsonRpcResponse. Entries 67 and 134 in valve/upstream-bug-log.md record two
// readers that do not.

// traceLevelCache builds a real cache whose logger is at trace level, so the
// trace-only branches of Set actually run. An `allow` empty behaviour is what
// lets a response with no result reach them at all.
func traceLevelCache(t *testing.T, out *bytes.Buffer) *EvmJsonRpcCache {
	t.Helper()
	// The package's test logger disables output globally, which would hide the
	// records this test reads back. The branch itself runs either way, because
	// Set gates it on the logger's OWN level.
	previous := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(previous) })

	logger := zerolog.New(out).Level(zerolog.TraceLevel)
	policy := cachePolicyCfg("mem", "eth_getBlockByNumber")
	policy.Empty = common.CacheEmptyBehaviorAllow
	cache, err := NewEvmJsonRpcCache(context.Background(), &logger,
		cacheConfig(cacheConns(memoryConnector("mem")), cachePolicies(policy)))
	require.NoError(t, err)
	return cache
}

// TestCacheSet_ANilResponseAtTraceLevelIsNotAPanic covers entry 134. The trace
// branch reads rpcResp.GetResultBytes() before the policy fan-out, so it
// dereferences the nil earlier than the site entry 67 names. It runs only at
// trace level — which is the level an operator raises to debug a cache problem.
func TestCacheSet_ANilResponseAtTraceLevelIsNotAPanic(t *testing.T) {
	var out bytes.Buffer
	cache := traceLevelCache(t, &out)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := cacheRequest(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1234",false]}`)

	require.NotPanics(t, func() {
		_ = cache.Set(ctx, req, nil)
	}, "a response eRPC cannot read must not take the process with it")
}

// TestCacheSet_ATraceRecordStaysValidJsonWithNoResult is the second half. A
// response with no result bytes must not corrupt the log record it appears in:
// zerolog writes a RawJSON value verbatim, so an empty one leaves a dangling
// key and every field after it is unreadable. See entry 160.
func TestCacheSet_ATraceRecordStaysValidJsonWithNoResult(t *testing.T) {
	var out bytes.Buffer
	cache := traceLevelCache(t, &out)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req := cacheRequest(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["0x1234",false]}`)
	require.NotPanics(t, func() { _ = cache.Set(ctx, req, nil) })

	var caching int
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]interface{}
		require.NoErrorf(t, json.Unmarshal(line, &record),
			"every log record must be readable JSON; this one was %s", line)
		if record["message"] == "caching the response" {
			caching++
		}
	}
	// Discriminating: without this the test would pass for a Set that never
	// reached the trace branch at all.
	assert.Equal(t, 1, caching, "the trace branch under test must have run; log was %s", out.String())
}

// TestJsonRpcResponse_NilReceiverReadsAsNoResult pins the fix at its root. Both
// readers Set uses are on *JsonRpcResponse, and IsResultEmptyish on the same
// type already answers for a nil receiver. Making the other two agree fixes
// entries 67 and 134 together, and every future reader with them.
func TestJsonRpcResponse_NilReceiverReadsAsNoResult(t *testing.T) {
	var nilResp *common.JsonRpcResponse

	assert.True(t, nilResp.IsResultEmptyish(), "the existing convention on this type")
	assert.Nil(t, nilResp.GetResultBytes(), "no response carries no result bytes")
	assert.Equal(t, 0, nilResp.ResultLength(), "no response carries a result of length 0")
	assert.Equal(t, "", nilResp.GetResultString())
}
