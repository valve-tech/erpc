package util

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ParseByteSize turns an operator's config string into the byte budget for
// request and response limits. A silent misparse either rejects legitimate
// traffic or lets an unbounded body through, so every accepted spelling and
// every rejection below is load-bearing.
func TestParseByteSize_AcceptsTheDocumentedSpellings(t *testing.T) {
	cases := map[string]int{
		"1024":    1024,
		"1024B":   1024,
		"1kb":     1024,
		"1KB":     1024,
		"1Kb":     1024,
		" 10 KB ": 10 * 1024,
		"5MB":     5 * 1024 * 1024,
		"5mb":     5 * 1024 * 1024,
		"0":       0,
		"0KB":     0,
	}
	for in, want := range cases {
		got, err := ParseByteSize(in)
		require.NoError(t, err, "input %q", in)
		require.Equal(t, want, got, "input %q", in)
	}
}

func TestParseByteSize_MBIsNotParsedAsB(t *testing.T) {
	// "MB" ends in "B". If the suffix checks ran in the wrong order, 5MB
	// would parse as 5 bytes and every large response would be rejected.
	mb, err := ParseByteSize("5MB")
	require.NoError(t, err)
	require.Equal(t, 5*1024*1024, mb)

	kb, err := ParseByteSize("5KB")
	require.NoError(t, err)
	require.Equal(t, 5*1024, kb)
}

func TestParseByteSize_RejectsWhatItCannotSize(t *testing.T) {
	for _, in := range []string{
		"",       // empty
		"   ",    // whitespace only
		"KB",     // suffix with no number
		"abc",    // not a number
		"1.5MB",  // fractions are not supported
		"1GB",    // GB is not in the accepted set; "1G" is not an integer
		"-1",     // negative budget
		"-1KB",   // negative with suffix
		"1 2 KB", // two numbers
	} {
		_, err := ParseByteSize(in)
		require.Error(t, err, "input %q must be rejected rather than silently sized", in)
	}
}

func TestPointerHelpers_ReturnAddressableCopies(t *testing.T) {
	// Config structs use these to distinguish "unset" from "set to zero".
	// A helper that returned a shared pointer would alias two config
	// fields onto one value.
	s1, s2 := StringPtr("x"), StringPtr("x")
	require.Equal(t, "x", *s1)
	require.NotSame(t, s1, s2, "each call must yield an independent pointer")

	i := IntPtr(0)
	require.Equal(t, 0, *i)
	b := BoolPtr(false)
	require.Equal(t, false, *b)
	f := Float64Ptr(0)
	require.Equal(t, float64(0), *f)

	*i = 7
	require.Equal(t, 7, *IntPtr(7))
	require.Equal(t, 0, *IntPtr(0), "mutating one pointer must not affect a later call")
}
