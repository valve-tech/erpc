package util

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// RedactEndpoint is the only thing between an operator's upstream API key
// and every log line, metric label and admin surface that prints an
// endpoint. A leak here publishes a paid credential, so each shape below
// asserts the secret is absent, not just that the output "looks redacted".

func TestRedactEndpoint_DropsPathAndQueryForNonNativeSchemes(t *testing.T) {
	// A vendor scheme (alchemy://, drpc://, ...) carries the key in the
	// host or path. Only the scheme survives, so nothing after it can leak.
	got := RedactEndpoint("alchemy://SUPER-SECRET-KEY")
	require.False(t, strings.Contains(got, "SUPER-SECRET-KEY"),
		"the vendor key must never reach a log line")
	require.True(t, strings.HasPrefix(got, "alchemy#redacted="),
		"only the scheme plus a hash suffix survives, got %q", got)
}

func TestRedactEndpoint_KeepsSchemeAndHostForNativeProtocols(t *testing.T) {
	// http/https/ws/wss endpoints keep host so an operator can still tell
	// two upstreams apart. The path holds the key and must be dropped.
	for _, ep := range []string{
		"https://eth-mainnet.example.com/v2/API-KEY-9000?token=t0k3n",
		"wss://eth-mainnet.example.com/v2/API-KEY-9000",
		"grpc://node.example.com:443/API-KEY-9000",
		"grpc+bds://node.example.com/API-KEY-9000",
	} {
		got := RedactEndpoint(ep)
		require.False(t, strings.Contains(got, "API-KEY-9000"),
			"endpoint %q leaked its key as %q", ep, got)
		require.False(t, strings.Contains(got, "t0k3n"),
			"endpoint %q leaked its query token as %q", ep, got)
		require.True(t, strings.Contains(got, "example.com"),
			"host must survive so operators can tell upstreams apart, got %q", got)
		require.True(t, strings.Contains(got, "#redacted="),
			"native endpoints carry a hash suffix, got %q", got)
	}
}

func TestRedactEndpoint_EnvioKeepsHostWithoutHash(t *testing.T) {
	// Envio endpoints carry no secret, so the redactor deliberately emits
	// no hash. If this ever gained a hash suffix, dashboards that group by
	// endpoint would split one upstream into two series.
	got := RedactEndpoint("envio://rpc.hypersync.xyz")
	require.Equal(t, "envio://rpc.hypersync.xyz", got)
}

func TestRedactEndpoint_RepositoryKeepsHostAndHash(t *testing.T) {
	got := RedactEndpoint("repository://evm-public-endpoints.erpc.cloud")
	require.True(t, strings.HasPrefix(got, "repository://evm-public-endpoints.erpc.cloud#redacted="),
		"repository endpoints keep host and gain a hash, got %q", got)
}

func TestRedactEndpoint_UnparseableInputFallsBackToHashOnly(t *testing.T) {
	// The fallthrough is the primary path: an endpoint shape nobody
	// anticipated must still redact completely rather than pass through.
	for _, ep := range []string{"", "not a url at all", "://missing-scheme", "justastring"} {
		got := RedactEndpoint(ep)
		require.True(t, strings.HasPrefix(got, "redacted="),
			"unparseable endpoint %q must degrade to hash-only, got %q", ep, got)
		require.Len(t, got, len("redacted=")+5,
			"hash-only form is the 5-char prefix, got %q", got)
	}
}

func TestRedactEndpoint_IsStableAndDistinguishing(t *testing.T) {
	// Operators correlate log lines by the hash. It must be stable for one
	// endpoint and differ between two, or correlation silently misleads.
	a := RedactEndpoint("https://example.com/key-a")
	b := RedactEndpoint("https://example.com/key-b")
	require.Equal(t, a, RedactEndpoint("https://example.com/key-a"),
		"the same endpoint must redact to the same string every time")
	require.NotEqual(t, a, b,
		"two endpoints on one host must stay distinguishable")
}
