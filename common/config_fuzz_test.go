package common

import (
	"testing"

	"github.com/spf13/afero"
)

// FuzzLoadConfigYaml drives an arbitrary YAML document through the whole
// config pipeline an operator triggers at boot: strict decode, the legacy
// migration hook, SetDefaults and Validate. A malformed config must produce an
// error, never a panic — eRPC has no chance to log anything useful once the
// process dies at boot.
//
// The seeds are REAL configs from common/config_test.go and from the shipped
// erpc.dist.yaml.
//
// The TypeScript branch of LoadConfig is NOT fuzzed here: it hands the input
// to esbuild and then executes it in a sobek VM, so a fuzzer generates
// non-terminating programs by construction and every finding would be "the
// input ran forever", not a parser defect.
func FuzzLoadConfigYaml(f *testing.F) {
	seeds := []string{
		"logLevel: DEBUG\n",
		"invalid yaml",
		"",
		`
logLevel: error
projects:
  - id: main
    upstreams:
      - id: alc-eth-mainnet
        endpoint: https://eth.example/
        evm: { chainId: 1 }
    networks:
      - architecture: evm
        evm: { chainId: 1 }
`,
		`
logLevel: error
projects:
  - id: prod-shape
    scoreMetricsWindowSize: 10m
    scoreMetricsMode: compact
    upstreamDefaults:
      routing:
        scoreLatencyQuantile: 0.9
        scoreMultipliers:
          - finality: [realtime, unfinalized]
            respLatency: 10
            errorRate: 2
    upstreams:
      - id: alc-eth-mainnet
        endpoint: https://eth.example/
        evm: { chainId: 1 }
        routing:
          scoreMultipliers:
            - overall: 0.2
    networks:
      - architecture: evm
        evm: { chainId: 1 }
`,
		`
server:
  httpPort: 4000
  maxTimeout: 30s
database:
  evmJsonRpcCache:
    connectors:
      - id: memory
        driver: memory
        memory:
          maxItems: 1000
`,
		`
projects:
  - id: main
    networks:
      - architecture: evm
        evm:
          chainId: 1
        failsafe:
          - matchMethod: "*"
            timeout:
              duration: 30s
            retry:
              maxAttempts: 3
`,
		"projects:\n  - id: a\n    upstreams: []\n",
		"projects: []\n",
		"unknownTopLevelKey: 1\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		fs := afero.NewMemMapFs()
		if err := afero.WriteFile(fs, "erpc.yaml", data, 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(fs, "erpc.yaml", &DefaultOptions{})
		if err != nil {
			return
		}
		if cfg == nil {
			t.Fatal("LoadConfig returned a nil config and a nil error")
		}
		// A config that loads must survive the accessors the boot path calls
		// on it right afterwards.
		_ = cfg.Validate()
	})
}
