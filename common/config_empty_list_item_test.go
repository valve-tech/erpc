package common

import (
	"strings"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

// A single stray dash in a YAML config used to kill eRPC at boot with a
// nil-pointer panic. yaml decodes an item with nothing after the dash into a
// nil pointer, and every SetDefaults down the tree dereferenced its receiver
// without a guard.
//
// FuzzLoadConfigYaml found it from the eleven-byte input "projects:\n-".
//
// The operator must instead get an error that names the offending item. These
// cases cover one list per level of the config tree; the check itself is
// generic, so a list added later is covered too.
func TestLoadConfig_AnEmptyListItemNamesItselfInsteadOfPanicking(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantPath string
	}{
		{
			"projects",
			"projects:\n  -\n",
			"projects[0]",
		},
		{
			"upstreams",
			"projects:\n  - id: a\n    upstreams:\n      -\n",
			"projects[0].upstreams[0]",
		},
		{
			"networks",
			"projects:\n  - id: a\n    networks:\n      -\n",
			"projects[0].networks[0]",
		},
		{
			"providers",
			"projects:\n  - id: a\n    providers:\n      -\n",
			"projects[0].providers[0]",
		},
		{
			"auth strategies",
			"projects:\n  - id: a\n    auth:\n      strategies:\n        -\n",
			"projects[0].auth.strategies[0]",
		},
		{
			"rate limit budgets",
			"rateLimiters:\n  budgets:\n    -\n",
			"rateLimiters.budgets[0]",
		},
		{
			"cache connectors",
			"database:\n  evmJsonRpcCache:\n    connectors:\n      -\n",
			"database.evmJsonRpcCache.connectors[0]",
		},
		{
			"cache policies",
			"database:\n  evmJsonRpcCache:\n    policies:\n      -\n",
			"database.evmJsonRpcCache.policies[0]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := afero.NewMemMapFs()
			require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(tc.yaml), 0o644))

			_, err := LoadConfig(fs, "erpc.yaml", &DefaultOptions{})
			require.Error(t, err, "an empty list item must be rejected, not dereferenced")
			require.Contains(t, err.Error(), tc.wantPath,
				"the operator needs the path of the item to fix the file")
		})
	}
}

// A config MAP holds objects too, and an empty value there reaches the same
// dereference. `providers[].overrides` panicked at common/defaults.go:1751.
func TestLoadConfig_AnEmptyMapValueNamesItselfInsteadOfPanicking(t *testing.T) {
	cases := []struct {
		name     string
		yaml     string
		wantPath string
	}{
		{
			"provider overrides",
			"projects:\n  - id: a\n    providers:\n      - vendor: alchemy\n        overrides:\n          evm:1:\n",
			"projects[0].providers[0].overrides[evm:1]",
		},
		{
			"method definitions",
			"projects:\n  - id: a\n    networks:\n      - architecture: evm\n        evm: { chainId: 1 }\n        methods:\n          definitions:\n            eth_foo:\n",
			"projects[0].networks[0].methods.definitions[eth_foo]",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := afero.NewMemMapFs()
			require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(tc.yaml), 0o644))

			_, err := LoadConfig(fs, "erpc.yaml", &DefaultOptions{})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantPath)
		})
	}
}

// A JSON-RPC null inside a params list is legitimate data, not an empty list
// item. The check must not reject it.
func TestLoadConfig_ANullParamSurvivesTheEmptyItemCheck(t *testing.T) {
	src := strings.Join([]string{
		"database:",
		"  evmJsonRpcCache:",
		"    connectors:",
		"      - id: memory",
		"        driver: memory",
		"    policies:",
		"      - connector: memory",
		"        network: '*'",
		"        method: eth_getLogs",
		"        params: [null, \"0x1\"]",
		"        ttl: 5s",
		"",
	}, "\n")

	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(src), 0o644))

	cfg, err := LoadConfig(fs, "erpc.yaml", &DefaultOptions{})
	require.NoError(t, err)
	require.Len(t, cfg.Database.EvmJsonRpcCache.Policies, 1)
	require.Len(t, cfg.Database.EvmJsonRpcCache.Policies[0].Params, 2)
	require.Nil(t, cfg.Database.EvmJsonRpcCache.Policies[0].Params[0])
}
