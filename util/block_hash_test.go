package util

import (
	"encoding/hex"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A block hash becomes part of the cache key. Two spellings of one hash
// that normalise differently split the cache; two DIFFERENT hashes that
// normalise to the same string serve one block's data for another. Both
// failures are silent, so every accepted spelling below is load-bearing.

const canonicalHash = "0x00000000000000000000000000000000000000000000000000000000deadbeef"

func TestNormalizeBlockHashHexString_AllSpellingsOfOneHashAgree(t *testing.T) {
	for _, in := range []string{
		"0xdeadbeef",
		"0Xdeadbeef",
		"deadbeef",
		"0xDEADBEEF",
		"DeAdBeEf",
		"  0xdeadbeef  ",
		"0x00000000000000000000000000000000000000000000000000000000deadbeef",
		"0x000000000000000000000000000000000000000000000000000000000deadbeef", // 65 digits, leading zero
	} {
		got, err := NormalizeBlockHashHexString(in)
		require.NoError(t, err, "input %q", in)
		require.Equal(t, canonicalHash, got, "input %q must normalise to the canonical form", in)
	}
}

func TestNormalizeBlockHashHexString_AlwaysReturns64DigitsWithAPrefix(t *testing.T) {
	// Downstream code slices this at fixed offsets. A short result would
	// index out of range; a long one would truncate the hash.
	for _, in := range []string{"0x1", "1", "0xabc", strings.Repeat("f", 64)} {
		got, err := NormalizeBlockHashHexString(in)
		require.NoError(t, err, "input %q", in)
		require.True(t, strings.HasPrefix(got, "0x"), "input %q gave %q", in, got)
		require.Len(t, got, 66, "input %q gave %q", in, got)
	}
}

func TestNormalizeBlockHashHexString_PadsAnOddDigitCountOnTheLeft(t *testing.T) {
	// "0x1" is 1, not 16. Padding on the right would shift every nibble
	// and produce a completely different hash.
	got, err := NormalizeBlockHashHexString("0x1")
	require.NoError(t, err)
	require.Equal(t, "0x"+strings.Repeat("0", 63)+"1", got)
}

func TestNormalizeBlockHashHexString_KeepsDistinctHashesDistinct(t *testing.T) {
	a, err := NormalizeBlockHashHexString("0xdeadbeef")
	require.NoError(t, err)
	b, err := NormalizeBlockHashHexString("0xdeadbeee")
	require.NoError(t, err)
	require.NotEqual(t, a, b, "one-nibble-apart hashes must not collide in the cache key")

	full, err := NormalizeBlockHashHexString(strings.Repeat("a", 64))
	require.NoError(t, err)
	require.NotEqual(t, canonicalHash, full)
}

func TestNormalizeBlockHashHexString_RejectsWhatItCannotNormalise(t *testing.T) {
	for _, in := range []string{
		"",                              // empty
		"0xzz",                          // non-hex
		"0xdead beef",                   // inner space
		"0xdeadbeefg",                   // trailing non-hex
		"-1",                            // sign
		"0x" + strings.Repeat("1", 65),  // 65 significant digits
		"0x" + strings.Repeat("f", 128), // far too long
		"0x0.1",                         // decimal point
	} {
		_, err := NormalizeBlockHashHexString(in)
		require.Error(t, err, "input %q must be rejected rather than silently reshaped", in)
	}
}

func TestNormalizeBlockHashHexString_BareZeroPrefixBecomesTheZeroHash(t *testing.T) {
	// Observed behaviour, pinned as a hazard rather than asserted away.
	// The empty-string guard runs BEFORE the "0x" prefix is stripped, so
	// "0x" survives it, normalises to 64 zeros and becomes a valid-looking
	// cache key. A caller that reads `blockHash` from a client request and
	// passes it through gets a hash for a block that does not exist,
	// silently, instead of an error. The zero hash is a legitimate padding
	// target for "0x0", so the two cases are indistinguishable downstream.
	zero := "0x" + strings.Repeat("0", 64)

	got, err := NormalizeBlockHashHexString("0x")
	require.NoError(t, err)
	require.Equal(t, zero, got)

	fromZero, err := NormalizeBlockHashHexString("0x0")
	require.NoError(t, err)
	require.Equal(t, zero, fromZero, "\"0x\" and \"0x0\" are indistinguishable after normalisation")
}

func TestNormalizeBlockHashHexString_IsIdempotent(t *testing.T) {
	// Values round-trip through config, cache and logs. A second pass must
	// not change the key.
	once, err := NormalizeBlockHashHexString("0xdeadbeef")
	require.NoError(t, err)
	twice, err := NormalizeBlockHashHexString(once)
	require.NoError(t, err)
	require.Equal(t, once, twice)
}

func TestParseBlockHashHexToBytes_Returns32BytesMatchingTheNormalisedForm(t *testing.T) {
	b, err := ParseBlockHashHexToBytes("0xdeadbeef")
	require.NoError(t, err)
	require.Len(t, b, 32, "a block hash is always 32 bytes on the wire")
	require.Equal(t, strings.TrimPrefix(canonicalHash, "0x"), hex.EncodeToString(b))
}

func TestParseBlockHashHexToBytes_AgreesWithTheStringNormaliser(t *testing.T) {
	// The byte form and the string form key the same cache entries from
	// different call sites. They must never disagree.
	for _, in := range []string{"0xdeadbeef", "DEADBEEF", "  0Xdeadbeef ", strings.Repeat("a", 64)} {
		s, err := NormalizeBlockHashHexString(in)
		require.NoError(t, err)
		b, err := ParseBlockHashHexToBytes(in)
		require.NoError(t, err)
		require.Equal(t, strings.TrimPrefix(s, "0x"), hex.EncodeToString(b), "input %q", in)
	}
}

func TestParseBlockHashHexToBytes_PropagatesTheNormaliserRejection(t *testing.T) {
	for _, in := range []string{"", "0xzz", "0x" + strings.Repeat("1", 65)} {
		_, err := ParseBlockHashHexToBytes(in)
		require.Error(t, err, "input %q", in)
	}
}

func TestRandomID_StaysInsideTheInt32RangeAndVaries(t *testing.T) {
	// The ID rides a JSON-RPC request to the upstream. A value above
	// 2^31 loses precision in clients that store IDs as int32 or as a
	// JavaScript number's integer range, and the response can no longer
	// be matched to its request.
	seen := map[int64]bool{}
	for i := 0; i < 1000; i++ {
		id := RandomID()
		require.GreaterOrEqual(t, id, int64(0))
		require.Less(t, id, int64(math.MaxInt32))
		seen[id] = true
	}
	require.Greater(t, len(seen), 900, "IDs must vary — a constant would collide across in-flight requests")
}
