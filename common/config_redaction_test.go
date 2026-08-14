package common

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// These marshallers are the only thing standing between an operator's
// credentials and the admin config dump (and every log line that prints a
// config). Each test asserts BOTH that the secret is gone AND that the
// surrounding field survived, because a marshaller that returned an empty
// document would pass a "secret is absent" check while telling the operator
// nothing.

const (
	redisPassword   = "R3D1S-P4SSW0RD"
	pgURICredential = "PGUSER:PGSECRET"
	awsSecretKey    = "AWS-SECRET-ACCESS-KEY"
	authSecretValue = "AUTH-SHARED-SECRET"
	upstreamAPIKey  = "UPSTREAM-API-KEY"
	providerAPIKey  = "PROVIDER-API-KEY"
)

func marshalJSONAndYAML(t *testing.T, v interface{}) (string, string) {
	t.Helper()
	j, err := SonicCfg.Marshal(v)
	require.NoError(t, err)
	y, err := yaml.Marshal(v)
	require.NoError(t, err)
	return string(j), string(y)
}

// TestRedisConnectorConfig_MarshalRedactsThePasswordAndTheUri. A Redis URI
// carries its password inline, so both fields have to go.
func TestRedisConnectorConfig_MarshalRedactsThePasswordAndTheUri(t *testing.T) {
	t.Parallel()

	c := &RedisConnectorConfig{
		Addr:         "cache.internal:6379",
		Username:     "erpc",
		Password:     redisPassword,
		DB:           3,
		ConnPoolSize: 12,
		URI:          "redis://erpc:" + redisPassword + "@cache.internal:6379/3",
	}

	j, y := marshalJSONAndYAML(t, c)
	for _, out := range []string{j, y} {
		require.NotContains(t, out, redisPassword, "the redis password must never be serialized")
		require.Contains(t, out, "REDACTED")
		require.Contains(t, out, "cache.internal:6379", "the address is not a secret and helps diagnosis")
		require.Contains(t, out, "erpc", "the username is not a secret")
	}
}

// TestPostgreSQLConnectorConfig_MarshalRedactsTheConnectionUri — the Postgres
// connection string embeds the user and password.
func TestPostgreSQLConnectorConfig_MarshalRedactsTheConnectionUri(t *testing.T) {
	t.Parallel()

	c := &PostgreSQLConnectorConfig{
		ConnectionUri: "postgres://" + pgURICredential + "@db.internal:5432/erpc",
		Table:         "erpc_cache",
		MinConns:      2,
		MaxConns:      20,
	}

	j, y := marshalJSONAndYAML(t, c)
	for _, out := range []string{j, y} {
		require.NotContains(t, out, pgURICredential, "the database credential must never be serialized")
		require.NotContains(t, out, "db.internal", "the redacted form keeps only the scheme")
		require.Contains(t, out, "redacted=")
		require.Contains(t, out, "erpc_cache", "the table name is not a secret")
	}
}

// TestAwsAuthConfig_MarshalRedactsOnlyTheSecretKey. The access key ID is an
// identifier an operator needs in a support ticket; the secret key is not.
func TestAwsAuthConfig_MarshalRedactsOnlyTheSecretKey(t *testing.T) {
	t.Parallel()

	a := &AwsAuthConfig{
		Mode:            "env",
		CredentialsFile: "/etc/erpc/aws",
		Profile:         "prod",
		AccessKeyID:     "AKIAEXAMPLE",
		SecretAccessKey: awsSecretKey,
	}

	j, y := marshalJSONAndYAML(t, a)
	for _, out := range []string{j, y} {
		require.NotContains(t, out, awsSecretKey)
		require.Contains(t, out, "REDACTED")
		require.Contains(t, out, "AKIAEXAMPLE", "the access key id is an identifier, not a secret")
		require.Contains(t, out, "prod")
	}
}

// TestSecretStrategyConfig_MarshalRedactsTheSharedSecret. This value is the
// bearer token clients present; leaking it in an admin dump hands over auth.
func TestSecretStrategyConfig_MarshalRedactsTheSharedSecret(t *testing.T) {
	t.Parallel()

	s := &SecretStrategyConfig{
		Id:              "team-a",
		Value:           authSecretValue,
		RateLimitBudget: "paid",
	}

	j, y := marshalJSONAndYAML(t, s)
	for _, out := range []string{j, y} {
		require.NotContains(t, out, authSecretValue)
		require.Contains(t, out, "REDACTED")
	}

	// The YAML form keeps the identifying fields; the JSON form drops them.
	// That asymmetry is real, and an operator reading the JSON dump cannot tell
	// which strategy a redacted entry belongs to.
	require.Contains(t, y, "team-a")
	require.Contains(t, y, "paid")
	require.NotContains(t, j, "team-a",
		"the JSON dump loses the strategy id, so redacted entries are indistinguishable")
}

// TestUpstreamConfig_MarshalRedactsTheEndpoint. The upstream endpoint carries
// the vendor API key in its path or query for nearly every hosted provider.
func TestUpstreamConfig_MarshalRedactsTheEndpoint(t *testing.T) {
	t.Parallel()

	u := &UpstreamConfig{
		Id:       "alchemy-main",
		Type:     UpstreamTypeEvm,
		Endpoint: "https://eth-mainnet.g.alchemy.com/v2/" + upstreamAPIKey,
	}

	j, y := marshalJSONAndYAML(t, u)
	for _, out := range []string{j, y} {
		require.NotContains(t, out, upstreamAPIKey, "the vendor API key must never be serialized")
		require.Contains(t, out, "redacted=")
		require.Contains(t, out, "alchemy-main", "the upstream id must survive so the entry is identifiable")
	}
}

// TestProviderConfig_MarshalRedactsTheSettings. Provider settings are a free
// map, and that is exactly where API keys live.
func TestProviderConfig_MarshalRedactsTheSettings(t *testing.T) {
	t.Parallel()

	p := &ProviderConfig{
		Id:                 "alchemy",
		Vendor:             "alchemy",
		Settings:           VendorSettings{"apiKey": providerAPIKey},
		OnlyNetworks:       []string{"evm:1"},
		IgnoreNetworks:     []string{"evm:137"},
		UpstreamIdTemplate: "<VENDOR>-<NETWORK>",
	}

	j, y := marshalJSONAndYAML(t, p)
	for _, out := range []string{j, y} {
		require.NotContains(t, out, providerAPIKey, "provider settings must never be serialized")
		require.Contains(t, out, "REDACTED")
		require.Contains(t, out, "alchemy")
		require.Contains(t, out, "evm:1")
	}

	// The JSON form omits ignoreNetworks that the YAML form keeps. An operator
	// comparing the two dumps sees a network exclusion in one and not the other.
	require.Contains(t, y, "evm:137")
	require.NotContains(t, j, "evm:137",
		"DEFECT: ProviderConfig.MarshalJSON drops ignoreNetworks")
}

// TestConfig_MarshalLeaksNoSecretFromAnyNestedBlock is the test that matters
// operationally: the admin endpoint dumps the WHOLE config, so a redaction that
// works in isolation but is bypassed by an outer marshaller is worthless. One
// distinct secret per slot, and none of them may appear in the output.
func TestConfig_MarshalLeaksNoSecretFromAnyNestedBlock(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Projects: []*ProjectConfig{
			{
				Id: "main",
				Upstreams: []*UpstreamConfig{
					{
						Id:       "alchemy-main",
						Type:     UpstreamTypeEvm,
						Endpoint: "https://eth-mainnet.g.alchemy.com/v2/" + upstreamAPIKey,
					},
				},
				Providers: []*ProviderConfig{
					{Id: "alchemy", Vendor: "alchemy", Settings: VendorSettings{"apiKey": providerAPIKey}},
				},
				Auth: &AuthConfig{
					Strategies: []*AuthStrategyConfig{
						{Type: AuthTypeSecret, Secret: &SecretStrategyConfig{Id: "team-a", Value: authSecretValue}},
					},
				},
			},
		},
		RateLimiters: &RateLimiterConfig{
			Store: &RateLimitStoreConfig{
				Driver: "redis",
				Redis: &RedisConnectorConfig{
					Addr:     "cache.internal:6379",
					Password: redisPassword,
					URI:      "redis://erpc:" + redisPassword + "@cache.internal:6379",
				},
			},
		},
	}

	j, y := marshalJSONAndYAML(t, cfg)

	secrets := map[string]string{
		"upstream API key":  upstreamAPIKey,
		"provider API key":  providerAPIKey,
		"auth shared token": authSecretValue,
		"redis password":    redisPassword,
	}

	for _, out := range []string{j, y} {
		for name, secret := range secrets {
			require.False(t, strings.Contains(out, secret),
				"%s leaked into a full config dump", name)
		}
		// The dump must still be useful: the project and the upstream have to
		// be identifiable, otherwise "no secrets" is trivially satisfied.
		require.Contains(t, out, "main")
		require.Contains(t, out, "alchemy-main")
	}
}
