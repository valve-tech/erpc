package common

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// Two hash functions decide whether eRPC treats two things as the same:
// canonicalizeTo feeds the consensus response hash, and hashValue feeds the
// cache key. Both write into an io.Writer, and both must abort on a write
// failure rather than hash a truncated byte stream — a short hash makes two
// different responses look identical to consensus, or two different requests
// share a cache entry.

// failAfterNWrites accepts n writes and then fails. Sweeping n over a range
// exercises every write site in a nested value without naming any of them.
type failAfterNWrites struct {
	remaining int
	written   int
	failed    int
}

func (w *failAfterNWrites) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		w.failed++
		return 0, errors.New("writer closed")
	}
	w.remaining--
	w.written += len(p)
	return len(p), nil
}

func TestCanonicalizeTo_AbortsOnAWriteFailure(t *testing.T) {
	values := []interface{}{
		map[string]interface{}{"b": "0x2", "a": "0x1", "c": map[string]interface{}{"d": "0x3"}},
		[]interface{}{"0x1", "0x2", []interface{}{"0x3"}},
		map[string]interface{}{"list": []interface{}{"0x1", "0x2"}},
	}

	for vi, v := range values {
		// First find how many writes the value needs when nothing fails.
		counting := &failAfterNWrites{remaining: 1 << 20}
		wrote, err := canonicalizeTo(counting, v)
		require.NoError(t, err)
		require.True(t, wrote)
		total := (1 << 20) - counting.remaining
		require.Greater(t, total, 1, "value %d should need several writes", vi)

		for n := 0; n < total; n++ {
			t.Run(fmt.Sprintf("value%d/failAfter%d", vi, n), func(t *testing.T) {
				// The returned flag is not asserted: on the final closing
				// brace the function returns (true, err), and every caller
				// reads err first.
				//
				// Exactly ONE write may fail. A second failed attempt means
				// the first error was swallowed and canonicalization carried
				// on writing into a broken stream.
				w := &failAfterNWrites{remaining: n}
				_, err := canonicalizeTo(w, v)
				require.Error(t, err, "a write failure must surface, not be swallowed")
				require.Equal(t, 1, w.failed, "canonicalization must stop at the first write failure")
			})
		}
	}
}

// TestCanonicalizeTo_ReportsThatItWroteNothing pins the "wrote" flag for a
// container whose every member is emptyish. The parent uses that flag to drop
// the key entirely, which is what makes {"a":{}} and {} hash the same.
// (The resulting hashes themselves are pinned in json_rpc_canonical_test.go.)
func TestCanonicalizeTo_ReportsThatItWroteNothing(t *testing.T) {
	t.Run("an all-empty object writes nothing", func(t *testing.T) {
		w := &failAfterNWrites{remaining: 1 << 20}
		wrote, err := canonicalizeTo(w, map[string]interface{}{"a": nil, "b": ""})
		require.NoError(t, err)
		require.False(t, wrote)
	})

	t.Run("an all-empty array writes nothing", func(t *testing.T) {
		w := &failAfterNWrites{remaining: 1 << 20}
		wrote, err := canonicalizeTo(w, []interface{}{nil, ""})
		require.NoError(t, err)
		require.False(t, wrote)
	})
}

func TestHashValue_CoversEveryParameterType(t *testing.T) {
	tests := []struct {
		name  string
		value interface{}
	}{
		{"bool", true},
		{"int", 42},
		{"float", 1.5},
		{"string", "0xABC"},
		{"nil", nil},
		{"array", []interface{}{"0x1", 2, false}},
		{"object", map[string]interface{}{"to": "0x1", "data": "0x2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &failAfterNWrites{remaining: 1 << 20}
			require.NoError(t, hashValue(w, tt.value))
			require.Positive(t, w.written, "every supported type must contribute bytes")
		})
	}
}

func TestHashValue_RejectsAnUnsupportedType(t *testing.T) {
	type custom struct{ A int }

	w := &failAfterNWrites{remaining: 1 << 20}
	err := hashValue(w, custom{A: 1})

	require.Error(t, err, "an unhashable param must fail loudly, not hash to nothing")
	require.Contains(t, err.Error(), "unsupported type for value during hash")
	require.Zero(t, w.written)
}

func TestHashValue_AbortsOnAWriteFailure(t *testing.T) {
	nested := []interface{}{
		"0x1",
		map[string]interface{}{"b": "0x2", "a": []interface{}{"0x3", "0x4"}},
	}

	counting := &failAfterNWrites{remaining: 1 << 20}
	require.NoError(t, hashValue(counting, nested))
	total := (1 << 20) - counting.remaining
	require.Greater(t, total, 2)

	for n := 0; n < total; n++ {
		t.Run(fmt.Sprintf("failAfter%d", n), func(t *testing.T) {
			w := &failAfterNWrites{remaining: n}
			require.Error(t, hashValue(w, nested))
		})
	}
}

// TestCacheHash_IsCaseInsensitiveForStringParams pins the deliberate
// lowercasing: EVM addresses and hex data arrive in mixed case from different
// clients, and eRPC must not split one cache entry into two.
func TestCacheHash_IsCaseInsensitiveForStringParams(t *testing.T) {
	upper, err := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xABCDEF", "latest"}).CacheHash()
	require.NoError(t, err)
	lower, err := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xabcdef", "latest"}).CacheHash()
	require.NoError(t, err)

	require.Equal(t, upper, lower)
}

func TestCacheHash_SeparatesByMethodAndParameterValue(t *testing.T) {
	base := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xabc", "latest"})
	baseHash, err := base.CacheHash()
	require.NoError(t, err)
	require.Contains(t, baseHash, "eth_getBalance:")

	otherMethod, err := NewJsonRpcRequest("eth_getCode", []interface{}{"0xabc", "latest"}).CacheHash()
	require.NoError(t, err)
	require.NotEqual(t, baseHash, otherMethod)

	otherParam, err := NewJsonRpcRequest("eth_getBalance", []interface{}{"0xabd", "latest"}).CacheHash()
	require.NoError(t, err)
	require.NotEqual(t, baseHash, otherParam)
}

// TestCacheHash_ConcatenatesAdjacentParamsWithoutASeparator records bug 118.
// hashValue writes each value straight after the previous one, so two
// parameter lists whose concatenations match produce ONE cache key. The
// assertion below is the defect, not the requirement: when the separator
// lands, this test fails and should be rewritten as a NotEqual.
func TestCacheHash_ConcatenatesAdjacentParamsWithoutASeparator(t *testing.T) {
	split, err := NewJsonRpcRequest("eth_getStorageAt", []interface{}{"0xabc", "0xdef", "latest"}).CacheHash()
	require.NoError(t, err)
	joined, err := NewJsonRpcRequest("eth_getStorageAt", []interface{}{"0xabc0xdef", "", "latest"}).CacheHash()
	require.NoError(t, err)

	require.Contains(t, split, "eth_getStorageAt:", "both requests must produce a real key")
	require.Equal(t, split, joined,
		"bug 118: two different parameter lists share one cache key because hashValue writes no separator")
}
