package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

// The cache read path builds a response straight out of the stored bytes
// (json_rpc_cache.go, NewJsonRpcResponseFromBytes) and never checks that they
// are JSON. A store that hands back a truncated or foreign value therefore
// turns into an HTTP 200 carrying a body no client can parse, with nothing
// logged on the eRPC side.
//
// The cache is a SHARED store — Redis, PostgreSQL, S3 — so its bytes are not
// eRPC's to trust: a truncated value, a partial object, or another writer on
// the same key all produce this.
//
// This test pins the CURRENT behaviour. It fails once the read path validates
// the stored bytes; see the entry in valve/upstream-bug-log.md before changing
// it. Validation costs a scan of every cache hit, so the trade-off belongs to
// the maintainer, not to this test.
func TestEvmJsonRpcCache_ACorruptStoredValueReachesTheClientVerbatim(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t, cacheConfig(
		cacheConns(memoryConnector("mem")),
		cachePolicies(cachePolicyCfg("mem", "eth_getBlockByNumber")),
	))

	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":11,"method":"eth_getBlockByNumber","params":["0x1b4",false]}`))

	blockRef, _, err := ResolveCacheBlockRef(ctx, req, nil)
	require.NoError(t, err)
	groupKey, requestKey, err := generateKeysForJsonRpcRequest(req, blockRef, ctx)
	require.NoError(t, err)

	// A value truncated mid-string, the shape a partial write or a clipped
	// read produces.
	corrupt := []byte(`{"number":"0x1b4`)
	require.NoError(t, f.connector("mem").Set(ctx, groupKey, requestKey, corrupt, nil))

	var resp *common.NormalizedResponse
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := f.cache.Get(ctx, req)
		require.NoError(t, err, "the cache reports no error on a corrupt value")
		if got != nil {
			resp = got
			break
		}
		require.False(t, time.Now().After(deadline), "nothing landed in the cache")
		time.Sleep(5 * time.Millisecond)
	}

	var wire bytes.Buffer
	_, err = resp.WriteTo(&wire)
	require.NoError(t, err, "the write path reports no error either")
	require.False(t, json.Valid(wire.Bytes()),
		"the client receives invalid JSON: %s", wire.String())
	require.Equal(t, `{"jsonrpc":"2.0","id":11,"result":{"number":"0x1b4}`, wire.String())
}
