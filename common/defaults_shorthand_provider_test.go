package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An operator may write a vendor shorthand where an endpoint belongs —
// `alchemy://<key>` instead of a full URL. SetDefaults turns that upstream into
// a provider, which is what actually mints the per-network upstreams. Getting
// this wrong means the operator's networks never appear.

// The shorthand must become a provider that carries the vendor, the credential
// and every other key the operator wrote on the upstream.
func TestConvertUpstreamToProvider_ShorthandBecomesAProvider(t *testing.T) {
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: main
    upstreams:
      - id: my-alchemy
        endpoint: alchemy://SECRET_KEY
        rateLimitBudget: heavy
`, &DefaultOptions{})

	project := onlyProject(t, cfg)
	provider := providerById(t, project, "my-alchemy")

	assert.Equal(t, "alchemy", provider.Vendor)
	assert.Equal(t, "SECRET_KEY", provider.Settings["apiKey"],
		"the credential must reach the vendor settings")
	assert.Equal(t, "<PROVIDER>-<NETWORK>", provider.UpstreamIdTemplate)

	override := provider.Overrides["*"]
	require.NotNil(t, override, "the provider must carry a catch-all override")
	assert.Equal(t, "heavy", override.RateLimitBudget,
		"every other key the operator wrote must survive the conversion")
	assert.Empty(t, override.Endpoint, "the provider mints the endpoint per network")

	assert.Empty(t, project.Upstreams,
		"the converted upstream must not also stay in the upstream list")
}

// A plain URL is not a shorthand. It must stay an upstream, or an operator's
// direct endpoint would be replaced by a vendor's network discovery.
func TestConvertUpstreamToProvider_LeavesAPlainUrlAlone(t *testing.T) {
	for _, endpoint := range []string{
		"https://rpc.example.com",
		"http://rpc.example.com",
		"wss://rpc.example.com",
		"grpc://rpc.example.com:443",
	} {
		t.Run(endpoint, func(t *testing.T) {
			cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: main
    upstreams:
      - id: u1
        endpoint: `+endpoint+`
        evm:
          chainId: 1
`, &DefaultOptions{})

			project := onlyProject(t, cfg)
			require.Len(t, project.Upstreams, 1, "a plain URL stays an upstream")
			assert.Equal(t, endpoint, project.Upstreams[0].Endpoint)
			assert.Empty(t, project.Providers, "nothing may be converted")
		})
	}
}

// An unrecognised scheme is the open case: the vendor set grows, and a typo
// looks exactly like a vendor eRPC has not learned yet. It must abort the load
// and name the scheme, not start with the upstream silently missing.
func TestConvertUpstreamToProvider_UnknownVendorAbortsTheLoad(t *testing.T) {
	_, err := setDefaultsFromYAML(t, `
projects:
  - id: main
    upstreams:
      - id: u1
        endpoint: notavendor://SECRET
`, &DefaultOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported vendor name")
	assert.Contains(t, err.Error(), "notavendor", "the operator must see what they typed")
	assert.Contains(t, err.Error(), "u1", "and which upstream it came from")
}

// A recognised vendor whose shorthand is malformed must fail with the vendor's
// own message, wrapped so the operator knows which upstream to fix.
func TestConvertUpstreamToProvider_MalformedVendorShorthandAbortsTheLoad(t *testing.T) {
	_, err := setDefaultsFromYAML(t, `
projects:
  - id: main
    upstreams:
      - id: rm1
        endpoint: routemesh://mesh.example.com/wrong/shape
`, &DefaultOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "routemesh endpoint path must be in format")
	assert.Contains(t, err.Error(), "rm1", "the operator must see which upstream failed")
}
