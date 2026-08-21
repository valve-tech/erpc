package util

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ParseBlockParameter reads the block argument of an EVM request. The value
// decides which cache entry the response lands in, so a malformed argument
// must produce an error rather than a plausible-looking wrong block.

func TestParseBlockHashHexToBytes_RejectsANonHexString(t *testing.T) {
	_, err := ParseBlockHashHexToBytes("0xZZZZ")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid hex character")
}

func TestParseBlockHashHexToBytes_RejectsAHashLongerThan32Bytes(t *testing.T) {
	// 65 significant hex digits. The normalizer trims leading zeros first,
	// so only a value that stays over 64 digits is a real overflow.
	long := "0x" + "1" + repeatHex("f", 64)
	_, err := ParseBlockHashHexToBytes(long)
	require.Error(t, err)
	require.Contains(t, err.Error(), "too long")
}

func TestParseBlockParameter_RejectsAMalformed66CharBlockHash(t *testing.T) {
	// A 66-character string that starts with 0x takes the block-hash branch.
	// Non-hex content there must fail, not silently become a block number.
	_, _, err := ParseBlockParameter("0x" + repeatHex("z", 64))
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to parse block hash")
}

func TestParseBlockParameter_RejectsAMalformedBlockHashInsideAnObject(t *testing.T) {
	_, _, err := ParseBlockParameter(map[string]interface{}{"blockHash": "0xnothex"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to parse blockHash from object")
}

func TestParseBlockParameter_RejectsAnUnsupportedType(t *testing.T) {
	// A JSON array, a bool, or nil in the block slot is a client error. The
	// caller must see it; a silent "" would read as "latest" downstream.
	// `int` is in the list on purpose: the switch handles int64 and uint64
	// but not int, so a caller that passes a plain Go int gets an error.
	for _, param := range []interface{}{nil, true, []interface{}{"0x1"}, int(5)} {
		_, _, err := ParseBlockParameter(param)
		require.Error(t, err)
		require.Contains(t, err.Error(), "invalid block parameter type")
	}
}

func repeatHex(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
