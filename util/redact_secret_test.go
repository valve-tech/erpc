package util

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// RedactSecret stands between a caller's credential and every log line that
// wants to name it. The properties below are the whole contract: the secret
// never survives, the same secret always renders the same way so an operator
// can correlate two lines, two different secrets render differently, and no
// input length can panic.

func TestRedactSecret_DropsTheSecret(t *testing.T) {
	// Synthetic key, invented for this test.
	const secret = "vk_SYNTHETIC_TEST_KEY_NEVER_ISSUED_0001"

	got := RedactSecret(secret)
	require.False(t, strings.Contains(got, secret),
		"the secret must never reach a log line, got %q", got)
	require.True(t, strings.HasPrefix(got, "redacted="),
		"output follows the RedactEndpoint vocabulary, got %q", got)

	// No substring of the secret longer than a couple of characters may
	// survive either — a partial key is still a key.
	for i := 0; i+6 <= len(secret); i++ {
		require.False(t, strings.Contains(got, secret[i:i+6]),
			"a six-character run of the secret leaked into %q", got)
	}
}

func TestRedactSecret_IsStableSoLogsCorrelate(t *testing.T) {
	// Two log lines about the same key must be joinable. If the output
	// varied per call, the redaction would be safe and useless.
	const secret = "vk_SYNTHETIC_TEST_KEY_NEVER_ISSUED_0002"
	require.Equal(t, RedactSecret(secret), RedactSecret(secret))
}

func TestRedactSecret_DistinguishesDifferentSecrets(t *testing.T) {
	// Two keys probed in the same minute must not look like one key.
	a := RedactSecret("vk_SYNTHETIC_TEST_KEY_NEVER_ISSUED_0003")
	b := RedactSecret("vk_SYNTHETIC_TEST_KEY_NEVER_ISSUED_0004")
	require.NotEqual(t, a, b)
}

func TestRedactSecret_ShortAndEmptyInputsDoNotPanic(t *testing.T) {
	// The slice comes off a 64-character hex digest, not off the input, so
	// no input length can shorten it. Pin that down.
	for _, in := range []string{"", "a", "ab", "abc", "abcd", "abcde", "\x00", "  "} {
		got := RedactSecret(in)
		require.True(t, strings.HasPrefix(got, "redacted="),
			"input %q rendered as %q", in, got)
	}
}

func TestRedactSecret_EmptyStringIsMarkedNotHashed(t *testing.T) {
	// The SHA-256 of "" is a well-known constant. Printing it would read
	// like a real digest and send an operator hunting for a key that was
	// never sent.
	require.Equal(t, "redacted=empty", RedactSecret(""))
	require.NotEqual(t, RedactSecret(""), RedactSecret("a"))
}

func TestRedactSecret_MatchesRedactEndpointDigestChoice(t *testing.T) {
	// One vocabulary across the codebase: the same hash, the same prefix
	// length. RedactEndpoint falls back to a bare "redacted=<hash[:5]>" for
	// anything it cannot parse as a URL, which is exactly this rendering.
	const secret = "vk_SYNTHETIC_TEST_KEY_NEVER_ISSUED_0005"
	require.Equal(t, RedactEndpoint(secret), RedactSecret(secret))
}
