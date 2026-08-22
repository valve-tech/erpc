package common

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// These tests take a whole config the way an operator writes one — a YAML
// document — and run it through Config.SetDefaults. The per-struct tests
// elsewhere pin one default each; what only shows up here is the ORCHESTRATION:
// a value written at the top of the file has to survive every hand-off down to
// the leaf that reads it, and a legacy key has to arrive at the field the
// runtime actually consults.
//
// The legacy paths carry the most weight. An operator who upgrades eRPC keeps
// their old file and expects it to keep working; nothing else in the suite
// proves that the old spelling still lands on the new field.

// setDefaultsFromYAML decodes `doc` exactly as LoadConfig does — environment
// expansion, then a strict decoder that rejects unknown keys — and runs the
// result through Config.SetDefaults.
//
// Reuse it for any orchestration question about the config tree: write the
// smallest YAML that poses the question, then assert on the returned *Config.
// It deliberately stops short of Validate() so a fixture can stay minimal and
// still exercise the defaults pass.
func setDefaultsFromYAML(t *testing.T, doc string, opts *DefaultOptions) (*Config, error) {
	t.Helper()
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader([]byte(os.ExpandEnv(doc))))
	dec.KnownFields(true)
	require.NoError(t, dec.Decode(&cfg), "the fixture itself must be valid YAML for this schema")
	return &cfg, cfg.SetDefaults(opts)
}

// mustSetDefaultsFromYAML is setDefaultsFromYAML for the cases that must succeed.
func mustSetDefaultsFromYAML(t *testing.T, doc string, opts *DefaultOptions) *Config {
	t.Helper()
	cfg, err := setDefaultsFromYAML(t, doc, opts)
	require.NoError(t, err)
	return cfg
}

// onlyProject returns the single project of a fixture config.
func onlyProject(t *testing.T, cfg *Config) *ProjectConfig {
	t.Helper()
	require.Len(t, cfg.Projects, 1)
	return cfg.Projects[0]
}

// providerById finds a provider by id, failing the test when it is absent.
func providerById(t *testing.T, p *ProjectConfig, id string) *ProviderConfig {
	t.Helper()
	for _, pr := range p.Providers {
		if pr.Id == id {
			return pr
		}
	}
	t.Fatalf("no provider with id %q; got %d providers", id, len(p.Providers))
	return nil
}

func TestConfigSetDefaults_AnEmptyConfigBecomesARunnableSingleProjectServer(t *testing.T) {
	// `erpc` started with a config that declares nothing must still serve. This
	// is the first-run experience, and every value below is one an operator
	// never typed but immediately depends on.
	cfg := mustSetDefaultsFromYAML(t, `{}`, nil)

	require.Equal(t, "INFO", cfg.LogLevel)
	require.Equal(t, "erpc-default", cfg.ClusterKey)
	require.NotNil(t, cfg.Server)
	require.Equal(t, 4000, *cfg.Server.HttpPortV4)
	require.Equal(t, 5000, *cfg.Server.HttpPortV6)
	require.NotNil(t, cfg.HealthCheck)
	require.Equal(t, HealthCheckModeNetworks, cfg.HealthCheck.Mode)
	require.NotNil(t, cfg.Metrics)
	require.Equal(t, 4001, *cfg.Metrics.Port)

	p := onlyProject(t, cfg)
	require.Equal(t, "main", p.Id)

	// With exactly one project, requests may omit the project segment. Without
	// this rule every first-run request to /evm/1 would 404.
	require.NotNil(t, cfg.Server.Aliasing)
	require.Len(t, cfg.Server.Aliasing.Rules, 1)
	require.Equal(t, "*", cfg.Server.Aliasing.Rules[0].MatchDomain)
	require.Equal(t, "main", cfg.Server.Aliasing.Rules[0].ServeProject)

	// The synthesised project must itself have been defaulted, not just built.
	require.NotNil(t, p.NetworkDefaults)
	require.Len(t, p.NetworkDefaults.Failsafe, 1)
	require.Equal(t, 5, p.NetworkDefaults.Failsafe[0].Retry.MaxAttempts)
	require.Equal(t, 2, p.NetworkDefaults.Failsafe[0].Hedge.MaxCount)
	require.NotNil(t, p.UpstreamDefaults)
	require.Equal(t, int64(5000), p.UpstreamDefaults.Evm.GetLogsAutoSplittingRangeThreshold)

	// No upstreams and no endpoints, so the public repository is the fallback
	// source of endpoints. Losing it means a first run reaches nothing at all.
	providerById(t, p, "public")
	providerById(t, p, "envio")
}

func TestConfigSetDefaults_AnExplicitProjectSuppressesTheDefaultOne(t *testing.T) {
	// The 'main' project and its wildcard alias exist only to rescue an empty
	// config. Injecting them into a configured deployment would route a domain
	// the operator never mapped.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - endpoint: https://node.example/rpc
`, nil)

	p := onlyProject(t, cfg)
	require.Equal(t, "prod", p.Id)
	require.Nil(t, cfg.Server.Aliasing, "an explicit project must not gain a wildcard alias")
}

func TestConfigSetDefaults_SeedsUpstreamsFromCommandLineEndpoints(t *testing.T) {
	// `erpc --endpoint=...` is the quick-start path. The endpoints have to reach
	// the project as real upstreams; falling through to the public repository
	// instead would silently ignore what the operator asked for.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
`, &DefaultOptions{Endpoints: []string{"https://a.example/rpc", "https://b.example/rpc"}})

	p := onlyProject(t, cfg)
	require.Len(t, p.Upstreams, 2)
	require.Empty(t, p.Providers, "explicit endpoints must not also pull in the public repository")
	for _, u := range p.Upstreams {
		require.NotEmpty(t, u.Id, "every upstream needs an id for metrics and logs")
		require.Equal(t, UpstreamTypeEvm, u.Type)
		require.NotNil(t, u.JsonRpc)
		require.NotNil(t, u.Evm)
	}
	require.Contains(t, p.Upstreams[0].Id, "a.example")
	require.Contains(t, p.Upstreams[1].Id, "b.example")
}

func TestConfigSetDefaults_SurfacesAFailureFromDeepInsideTheTree(t *testing.T) {
	// A contradictory connector is a startup-time mistake. If SetDefaults
	// swallowed it, eRPC would boot with a shared-state store it cannot reach
	// and only fail later, under traffic, as unexplained cache misses.
	_, err := setDefaultsFromYAML(t, `
database:
  sharedState:
    connector:
      driver: redis
      redis:
        uri: redis://one:6379
        addr: two:6379
`, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "redis connector")
}

func TestConfigSetDefaults_TheClusterKeyReachesTheSharedStateStore(t *testing.T) {
	// Replicas coordinate through keys prefixed with the cluster key. If the
	// top-level value did not reach the shared-state block, two deployments
	// sharing one Redis would silently read each other's counters.
	cfg := mustSetDefaultsFromYAML(t, `
clusterKey: tenant-a
database:
  sharedState:
    connector:
      driver: memory
`, nil)
	ss := cfg.Database.SharedState
	require.Equal(t, "tenant-a", ss.ClusterKey)
	require.Equal(t, "memory", ss.Connector.Id, "an unnamed connector is named after its driver")
	require.Equal(t, Duration(3*time.Second), ss.FallbackTimeout)
	require.Equal(t, Duration(4*time.Second), ss.LockTtl)
	require.Equal(t, Duration(100*time.Millisecond), ss.LockMaxWait)
	require.Equal(t, Duration(50*time.Millisecond), ss.UpdateMaxWait)

	// An explicit cluster key on the block is the operator overriding the
	// top-level one, and must win.
	cfg = mustSetDefaultsFromYAML(t, `
clusterKey: tenant-a
database:
  sharedState:
    clusterKey: tenant-b
    connector:
      driver: memory
`, nil)
	require.Equal(t, "tenant-b", cfg.Database.SharedState.ClusterKey)
}

func TestConfigSetDefaults_ReachesTheTracingAndAdminBlocks(t *testing.T) {
	// Both blocks are optional, so both are easy to forget in the walk. An
	// undefaulted tracing block sends spans nowhere; an undefaulted admin block
	// serves the admin API with no CORS headers at all.
	cfg := mustSetDefaultsFromYAML(t, `
tracing:
  enabled: true
admin:
  auth:
    strategies:
      - secret:
          value: s3cret
`, nil)

	require.Equal(t, TracingProtocolGrpc, cfg.Tracing.Protocol)
	require.Equal(t, "localhost:4317", cfg.Tracing.Endpoint)
	require.Equal(t, 1.0, cfg.Tracing.SampleRate)
	require.Equal(t, "erpc", cfg.Tracing.ServiceName)

	require.NotNil(t, cfg.Admin.CORS)
	require.Equal(t, []string{"*"}, cfg.Admin.CORS.AllowedOrigins)
	require.NotNil(t, cfg.Admin.Auth)
	require.Equal(t, AuthTypeSecret, cfg.Admin.Auth.Strategies[0].Type)

	// An http endpoint must not be forced onto the grpc default.
	cfg = mustSetDefaultsFromYAML(t, `
tracing:
  enabled: true
  protocol: http
`, nil)
	require.Equal(t, "http://localhost:4318", cfg.Tracing.Endpoint)
}

func TestConfigSetDefaults_MigratesTheDeprecatedHttpPort(t *testing.T) {
	// `httpPort` predates the v4/v6 split. An operator upgrading eRPC keeps it
	// in their file, and the server must still bind the port they wrote —
	// otherwise the process comes up on 4000 and every client is pointed at a
	// closed port.
	cfg := mustSetDefaultsFromYAML(t, `
server:
  httpPort: 8080
`, nil)
	s := cfg.Server
	require.Equal(t, 8080, *s.HttpPortV4, "the deprecated port must become the v4 port")
	require.Equal(t, 9080, *s.HttpPortV6, "the v6 port is derived as httpPort + 1000")
	require.Equal(t, 8080, *s.HttpPort, "the deprecated field stays readable for back-compat")
	require.Equal(t, "0.0.0.0", *s.HttpHostV4)
	require.Equal(t, "[::]", *s.HttpHostV6)
	require.Equal(t, 8080, *s.GrpcPortV4, "grpc mirrors the resolved http ports")
	require.Equal(t, 9080, *s.GrpcPortV6)

	// An explicit v4 port wins: the deprecated field must never override the
	// current spelling.
	cfg = mustSetDefaultsFromYAML(t, `
server:
  httpPort: 8080
  httpPortV4: 7070
`, nil)
	require.Equal(t, 7070, *cfg.Server.HttpPortV4)
}

func TestConfigSetDefaults_ConvertsAShorthandUpstreamIntoAProvider(t *testing.T) {
	// `alchemy://KEY` is the shorthand an operator writes instead of a provider
	// block. It has to become a real provider, and the rest of the upstream's
	// settings have to survive the move — a dropped chainId or failsafe block
	// would quietly change how every one of that vendor's endpoints behaves.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - id: alch
        endpoint: alchemy://my-api-key
        rateLimitBudget: vendor-budget
        evm:
          chainId: 42161
      - id: direct
        endpoint: https://node.example/rpc
`, nil)

	p := onlyProject(t, cfg)
	require.Len(t, p.Upstreams, 1, "the shorthand upstream must be removed from the list")
	require.Equal(t, "direct", p.Upstreams[0].Id, "a plain http endpoint stays an upstream")

	pr := providerById(t, p, "alch")
	require.Equal(t, "alchemy", pr.Vendor)
	require.Equal(t, "my-api-key", pr.Settings["apiKey"])
	require.Equal(t, "<PROVIDER>-<NETWORK>", pr.UpstreamIdTemplate)

	ov := pr.Overrides["*"]
	require.NotNil(t, ov, "the upstream's own settings must survive as a wildcard override")
	require.Empty(t, ov.Endpoint, "the provider owns the endpoint now")
	// The provider names its upstreams from UpstreamIdTemplate, so the override
	// must not keep the shorthand's own id. It does not keep it — but note that
	// UpstreamConfig.SetDefaults then invents a synthetic id from the now-empty
	// endpoint (something like "-8"), so this asserts only what matters:
	// nothing downstream may resolve the override by the operator's id.
	require.NotEqual(t, "alch", ov.Id, "the provider owns upstream naming now")
	require.Equal(t, "vendor-budget", ov.RateLimitBudget)
	require.Equal(t, int64(42161), ov.Evm.ChainId)
}

func TestConfigSetDefaults_ShorthandEndpointsCarryEachVendorsOwnSetting(t *testing.T) {
	// Every vendor names its credential differently. A setting written under
	// the wrong key reaches the vendor as an empty credential, and the operator
	// sees "unauthorized" from an endpoint whose URL looks perfectly correct.
	for _, tc := range []struct {
		endpoint string
		vendor   string
		key      string
		want     interface{}
	}{
		{"alchemy://k1", "alchemy", "apiKey", "k1"},
		{"evm+alchemy://k1", "alchemy", "apiKey", "k1"},
		{"blastapi://k2", "blastapi", "apiKey", "k2"},
		{"drpc://k3", "drpc", "apiKey", "k3"},
		{"envio://rpc.hypersync.xyz", "envio", "rootDomain", "rpc.hypersync.xyz"},
		{"etherspot://k4", "etherspot", "apiKey", "k4"},
		{"infura://k5", "infura", "apiKey", "k5"},
		{"llama://k6", "llama", "apiKey", "k6"},
		{"pimlico://k7", "pimlico", "apiKey", "k7"},
		{"thirdweb://c1", "thirdweb", "clientId", "c1"},
		{"dwellir://k8", "dwellir", "apiKey", "k8"},
		{"conduit://k9", "conduit", "apiKey", "k9"},
		{"tenderly://k10", "tenderly", "apiKey", "k10"},
		{"onfinality://k11", "onfinality", "apiKey", "k11"},
		{"blockpi://k12", "blockpi", "apiKey", "k12"},
		{"ankr://k13", "ankr", "apiKey", "k13"},
		{"satelink://k14", "satelink", "apiKey", "k14"},
		{"blockdaemon://k15", "blockdaemon", "apiKey", "k15"},
		{"superchain://registry.example/chains.json", "superchain", "registryUrl", "registry.example/chains.json"},
		{"erpc://peer.example/main?secret=sh", "erpc", "endpoint", "https://peer.example/main"},
		{"routemesh://mesh.example/rpc/1/k16", "routemesh", "apiKey", "k16"},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - id: shorthand
        endpoint: `+tc.endpoint+`
`, nil)
			pr := providerById(t, onlyProject(t, cfg), "shorthand")
			require.Equal(t, tc.vendor, pr.Vendor, "the evm+ prefix is not part of the vendor name")
			require.Equal(t, tc.want, pr.Settings[tc.key])
		})
	}
}

func TestConfigSetDefaults_KeepsTheExtraSettingsAShorthandCarries(t *testing.T) {
	// Some shorthands carry more than a credential. Dropping the extra keys
	// leaves the vendor on its own defaults — a different tier, a different
	// registry, or a peer eRPC reached without its shared secret.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - id: gs
        endpoint: goldsky://edge-secret?tier=custom
      - id: peer
        endpoint: erpc://peer.example/main?secret=sh
      - id: repo
        endpoint: repository://repo.example/list.json?evmOnly=true
`, nil)
	p := onlyProject(t, cfg)

	gs := providerById(t, p, "gs")
	require.Equal(t, "edge-secret", gs.Settings["secret"])
	require.Equal(t, "custom", gs.Settings["tier"])

	peer := providerById(t, p, "peer")
	require.Equal(t, "https://peer.example/main", peer.Settings["endpoint"])
	require.Equal(t, "sh", peer.Settings["secret"])

	repo := providerById(t, p, "repo")
	require.Equal(t, "https://repo.example/list.json?evmOnly=true", repo.Settings["repositoryUrl"])
}

func TestConfigSetDefaults_RejectsAShorthandEndpointItCannotRoute(t *testing.T) {
	// An unknown scheme is a typo or a vendor eRPC does not ship. Accepting it
	// would produce a provider with no settings that fails at request time; the
	// error has to name the upstream so the operator can find the line.
	for name, doc := range map[string]string{
		"unknown vendor": `
projects:
  - id: prod
    upstreams:
      - id: typo
        endpoint: alchmey://k1
`,
		"routemesh without an api key in the path": `
projects:
  - id: prod
    upstreams:
      - id: mesh
        endpoint: routemesh://mesh.example/rpc/1
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := setDefaultsFromYAML(t, doc, nil)
			require.Error(t, err)
			require.Contains(t, err.Error(), "provider")
		})
	}
}

// requireRecentWindow asserts that an upstream's recent-block window is the
// given number of blocks behind the head, whichever field carries it. A config
// may state it as the legacy `maxAvailableRecentBlocks` or as an explicit
// `blockAvailability` window; SetDefaults migrates the first into the second.
func requireRecentWindow(t *testing.T, u *UpstreamConfig, blocks int64) {
	t.Helper()
	require.NotNil(t, u.Evm, "upstream %q has no evm block", u.Id)
	if u.Evm.MaxAvailableRecentBlocks == blocks {
		return
	}
	require.NotNil(t, u.Evm.BlockAvailability,
		"upstream %q carries the window in neither field", u.Id)
	require.NotNil(t, u.Evm.BlockAvailability.Lower,
		"upstream %q has a window with no lower bound", u.Id)
	// NotNil before the dereference: a nil here is a test CRASH, not a failure,
	// and a crash reports nothing about the other assertions in this file.
	require.NotNil(t, u.Evm.BlockAvailability.Lower.LatestBlockMinus,
		"upstream %q has a lower bound that is not a latest-minus window", u.Id)
	require.Equal(t, blocks, *u.Evm.BlockAvailability.Lower.LatestBlockMinus,
		"upstream %q must inherit the %d-block window", u.Id, blocks)
}

func TestProjectSetDefaults_UpstreamDefaultsReachEveryUpstream(t *testing.T) {
	// upstreamDefaults is a template an operator writes once. Every field that
	// fails to reach a bare upstream is a policy they believe is running and
	// is not — no rate-limit budget, no retry, no node-type window.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreamDefaults:
      rateLimitBudget: shared
      tags:
        - tier:default
      failsafe:
        - matchMethod: "*"
          retry:
            maxAttempts: 4
      evm:
        statePollerInterval: 7s
        maxAvailableRecentBlocks: 900
    upstreams:
      - id: bare
        endpoint: https://a.example/rpc
      - id: own
        endpoint: https://b.example/rpc
        rateLimitBudget: private
        tags:
          - tier:fallback
        evm:
          statePollerInterval: 11s
`, nil)
	p := onlyProject(t, cfg)
	require.Len(t, p.Upstreams, 2)

	bare, own := p.Upstreams[0], p.Upstreams[1]
	require.Equal(t, "shared", bare.RateLimitBudget)
	require.Equal(t, []string{"tier:default"}, bare.Tags)
	require.Len(t, bare.Failsafe, 1)
	require.Equal(t, 4, bare.Failsafe[0].Retry.MaxAttempts)
	require.Equal(t, Duration(7*time.Second), bare.Evm.StatePollerInterval)
	// The 900-block window reaches the upstream through BlockAvailability, not
	// through MaxAvailableRecentBlocks. Upstream migrates the legacy field into
	// the window and then deliberately stops carrying both, because the two are
	// enforced as independent lower bounds and keeping both would narrow the
	// configured window to whichever is smaller (common/defaults.go,
	// maxRecentBlocksFor). This test is about defaults REACHING an upstream, so
	// it asserts the bound, not the field that used to hold it.
	requireRecentWindow(t, bare, 900)

	require.Equal(t, "private", own.RateLimitBudget, "an upstream's own value must win")
	require.Equal(t, []string{"tier:fallback"}, own.Tags, "tags are inherited all-or-nothing")
	require.Equal(t, Duration(11*time.Second), own.Evm.StatePollerInterval)
	requireRecentWindow(t, own, 900)

	// A budget implies auto-tuning, which is what actually spends it.
	require.NotNil(t, bare.RateLimitAutoTune)
	require.True(t, *bare.RateLimitAutoTune.Enabled)
	require.Equal(t, Duration(time.Minute), bare.RateLimitAutoTune.AdjustmentPeriod)
	require.Equal(t, 0.1, bare.RateLimitAutoTune.ErrorRateThreshold)
}

func TestNetworkSetDefaults_MigratesEvmIntegrityIntoDirectiveDefaults(t *testing.T) {
	// `evm.integrity` is the old spelling of the enforcement directives. The
	// runtime reads only directiveDefaults, so a config that still uses the old
	// block must land there — including an explicit `false`, which is an
	// operator switching enforcement OFF and is exactly the value a naive
	// "if unset, default true" pass would overwrite.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - endpoint: https://a.example/rpc
    networks:
      - architecture: evm
        evm:
          chainId: 1
          integrity:
            enforceHighestBlock: false
            enforceGetLogsBlockRange: false
`, nil)
	dd := onlyProject(t, cfg).Networks[0].DirectiveDefaults
	require.NotNil(t, dd)
	require.False(t, *dd.EnforceHighestBlock, "the legacy false must survive the defaults pass")
	require.False(t, *dd.EnforceGetLogsBlockRange)
	require.True(t, *dd.EnforceNonNullTaggedBlocks, "an unmentioned check still defaults on")

	// The current spelling wins when both are present.
	cfg = mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - endpoint: https://a.example/rpc
    networks:
      - architecture: evm
        directiveDefaults:
          enforceHighestBlock: true
        evm:
          chainId: 1
          integrity:
            enforceHighestBlock: false
`, nil)
	require.True(t, *onlyProject(t, cfg).Networks[0].DirectiveDefaults.EnforceHighestBlock,
		"an explicit directiveDefaults entry must beat the legacy block")
}

func TestNetworkSetDefaults_MigratesDeprecatedValidationFlagsIntoIntegrityChecks(t *testing.T) {
	// The per-check `validate*` flags are the old data-integrity switches. The
	// runtime now reads only `integrity.checks`, so a flag left behind in an
	// upgraded config must translate — otherwise a check the operator believes
	// is guarding their data quietly stops running.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - endpoint: https://a.example/rpc
    networks:
      - architecture: evm
        evm:
          chainId: 1
        directiveDefaults:
          validateLogsBloomMatch: true
          enforceLogIndexStrictIncrements: true
          validateTransactionsRoot: false
`, nil)
	n := onlyProject(t, cfg).Networks[0]
	require.NotNil(t, n.Integrity)
	require.NotNil(t, n.Integrity.Checks["bloomMatch"])
	require.True(t, *n.Integrity.Checks["bloomMatch"].Enabled)
	require.NotNil(t, n.Integrity.Checks["logIndexContiguity"])
	require.True(t, *n.Integrity.Checks["logIndexContiguity"].Enabled)
	require.NotContains(t, n.Integrity.Checks, "transactionsRootConsistency",
		"a flag left off must not switch its check on")

	// An explicit integrity block is the current spelling and wins per check.
	cfg = mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - endpoint: https://a.example/rpc
    networks:
      - architecture: evm
        evm:
          chainId: 1
        integrity:
          checks:
            bloomMatch:
              enabled: false
        directiveDefaults:
          validateLogsBloomMatch: true
`, nil)
	n = onlyProject(t, cfg).Networks[0]
	require.False(t, *n.Integrity.Checks["bloomMatch"].Enabled,
		"an explicit check must not be re-enabled by a legacy flag")
}

func TestNetworkSetDefaults_LeavesTheIntegrityBlockAloneWithoutLegacyFlags(t *testing.T) {
	// The migration must be a no-op for a config that never used the old flags.
	// Inventing an integrity block would switch checks on for operators who
	// never asked, adding latency and rejections to a working deployment.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - endpoint: https://a.example/rpc
    networks:
      - architecture: evm
        evm:
          chainId: 1
`, nil)
	require.Nil(t, onlyProject(t, cfg).Networks[0].Integrity)
}

func TestUpstreamSetDefaults_MigratesMaxAvailableRecentBlocksIntoBlockAvailability(t *testing.T) {
	// `maxAvailableRecentBlocks` is the old way to say "this node only keeps
	// the last N blocks". Routing now reads blockAvailability, so a config that
	// still uses the old key must produce the bound — otherwise eRPC sends
	// archive requests to a pruned node and returns missing data.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - id: pruned
        endpoint: https://a.example/rpc
        evm:
          chainId: 1
          maxAvailableRecentBlocks: 5000
`, nil)
	ba := onlyProject(t, cfg).Upstreams[0].Evm.BlockAvailability
	require.NotNil(t, ba)
	require.NotNil(t, ba.Lower)
	require.Equal(t, int64(5000), *ba.Lower.LatestBlockMinus)

	// A full node with nothing else stated gets the shipped 128-block window.
	cfg = mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - id: full
        endpoint: https://a.example/rpc
        evm:
          chainId: 1
          nodeType: full
`, nil)
	u := onlyProject(t, cfg).Upstreams[0]
	require.Equal(t, int64(128), u.Evm.MaxAvailableRecentBlocks)
	require.Equal(t, int64(128), *u.Evm.BlockAvailability.Lower.LatestBlockMinus)

	// An explicit blockAvailability is the current spelling and must not be
	// overwritten by the legacy key.
	cfg = mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - id: both
        endpoint: https://a.example/rpc
        evm:
          chainId: 1
          maxAvailableRecentBlocks: 5000
          blockAvailability:
            lower:
              latestBlockMinus: 64
`, nil)
	ba = onlyProject(t, cfg).Upstreams[0].Evm.BlockAvailability
	require.Equal(t, int64(64), *ba.Lower.LatestBlockMinus)
}

func TestUpstreamSetDefaults_AnAllowListImpliesIgnoringEverythingElse(t *testing.T) {
	// allowMethods is a security control. If the implied "ignore everything
	// else" were dropped, an allow-list would widen to "allow everything",
	// which is the opposite of what the operator wrote.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - id: restricted
        endpoint: https://a.example/rpc
        allowMethods:
          - eth_call
`, nil)
	u := onlyProject(t, cfg).Upstreams[0]
	require.Equal(t, []string{"*"}, u.IgnoreMethods)

	// An explicit ignore list is the operator being specific, and must survive.
	cfg = mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - id: restricted
        endpoint: https://a.example/rpc
        allowMethods:
          - eth_call
        ignoreMethods:
          - debug_*
`, nil)
	require.Equal(t, []string{"debug_*"}, onlyProject(t, cfg).Upstreams[0].IgnoreMethods)
}

func TestNetworkSetDefaults_NetworkDefaultsReachEveryNetwork(t *testing.T) {
	// networkDefaults is the project-wide template for networks. It is applied
	// per network, so a field that fails to arrive is a policy the operator
	// wrote once and that runs on none of their chains.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - endpoint: https://a.example/rpc
    networkDefaults:
      rateLimitBudget: net-budget
      failsafe:
        - matchMethod: "*"
          retry:
            maxAttempts: 6
      directiveDefaults:
        retryEmpty: false
      selectionPolicy:
        evalInterval: 3s
      evm:
        getLogsMaxAllowedRange: 12345
    networks:
      - architecture: evm
        evm:
          chainId: 1
      - architecture: evm
        rateLimitBudget: own-budget
        evm:
          chainId: 137
          getLogsMaxAllowedRange: 99
`, nil)
	p := onlyProject(t, cfg)
	require.Len(t, p.Networks, 2)

	// The defaults themselves are defaulted before they are handed down —
	// except directiveDefaults, where a nil field is the signal the legacy
	// `evm.integrity` migration reads. Filling it here is what used to
	// cancel that migration, so the block goes down as the operator wrote
	// it and each network defaults its own copy afterwards.
	require.Equal(t, Duration(3*time.Second), p.NetworkDefaults.SelectionPolicy.EvalInterval)
	require.Nil(t, p.NetworkDefaults.DirectiveDefaults.EnforceHighestBlock,
		"a directive the operator did not name must stay nil at the defaults level")

	bare, own := p.Networks[0], p.Networks[1]
	require.Equal(t, "net-budget", bare.RateLimitBudget)
	require.Len(t, bare.Failsafe, 1)
	require.Equal(t, 6, bare.Failsafe[0].Retry.MaxAttempts)
	require.Equal(t, int64(12345), bare.Evm.GetLogsMaxAllowedRange)
	require.NotNil(t, bare.SelectionPolicy, "the shared selection policy must reach the network")
	require.Equal(t, Duration(3*time.Second), bare.SelectionPolicy.EvalInterval)
	require.False(t, *bare.DirectiveDefaults.RetryEmpty)
	require.NotNil(t, bare.DirectiveDefaults.EnforceHighestBlock,
		"and it must be defaulted by the time the network reads it")
	require.True(t, *bare.DirectiveDefaults.EnforceHighestBlock)

	require.Equal(t, "own-budget", own.RateLimitBudget, "a network's own value must win")
	require.Equal(t, int64(99), own.Evm.GetLogsMaxAllowedRange)
	require.Equal(t, int64(1), bare.Evm.ChainId, "sharing defaults must not share chain identity")
	require.Equal(t, int64(137), own.Evm.ChainId)
}

func TestNetworkDefaults_LegacyEvmIntegrityReachesTheNetworkDirectives(t *testing.T) {
	// The legacy `evm.integrity` block is also legal at the networkDefaults
	// level, and from there it must still reach every network's directives.
	cfg := mustSetDefaultsFromYAML(t, `
projects:
  - id: prod
    upstreams:
      - endpoint: https://a.example/rpc
    networkDefaults:
      evm:
        integrity:
          enforceHighestBlock: false
    networks:
      - architecture: evm
        evm:
          chainId: 1
`, nil)
	dd := onlyProject(t, cfg).Networks[0].DirectiveDefaults
	require.False(t, *dd.EnforceHighestBlock,
		"networkDefaults.evm.integrity must reach the per-network directives")
}

// An unrelated key under `networkDefaults.directiveDefaults` does not cancel
// the legacy `evm.integrity` migration.
//
// It used to. ProjectConfig.SetDefaults ran NetworkDefaults.SetDefaults
// first, which filled DirectiveDefaults.Enforce* with `true`; each network
// copied that block, and the migration only writes into a field that is
// still nil — so it found `true` already there and did nothing. An operator
// whose `enforceHighestBlock: false` worked yesterday lost it by adding
// `retryEmpty: false` beside it, with no warning, and saw only requests
// rejected for a highest-block violation.
//
// The sub-tests measure the two levels against each other, because the
// defect was that they disagreed.
func TestNetworkDefaults_ADirectiveDefaultsBlockDoesNotCancelTheLegacyIntegrityMigration(t *testing.T) {
	// legacyIntegrityConfig writes the same legacy `enforceHighestBlock:
	// false` at networkDefaults level, with whatever extra directiveDefaults
	// keys the case needs beside it.
	legacyIntegrityConfig := func(extraDirectives string) string {
		return `
projects:
  - id: prod
    upstreams:
      - endpoint: https://a.example/rpc
    networkDefaults:` + extraDirectives + `
      evm:
        integrity:
          enforceHighestBlock: false
    networks:
      - architecture: evm
        evm:
          chainId: 1
`
	}

	t.Run("WithAnUnrelatedDirectiveBeside", func(t *testing.T) {
		cfg := mustSetDefaultsFromYAML(t, legacyIntegrityConfig(`
      directiveDefaults:
        retryEmpty: false`), nil)
		n := onlyProject(t, cfg).Networks[0]
		require.False(t, *n.Evm.Integrity.EnforceHighestBlock,
			"the legacy block itself still arrives at the network")
		require.False(t, *n.DirectiveDefaults.EnforceHighestBlock,
			"an unrelated directive must not switch enforcement back on")
		require.False(t, *n.DirectiveDefaults.RetryEmpty,
			"the unrelated directive itself must still take effect")
	})

	// The same config without the extra key. Both cases must agree, or the
	// operator's result still depends on a key that means nothing here.
	t.Run("WithNothingBeside", func(t *testing.T) {
		cfg := mustSetDefaultsFromYAML(t, legacyIntegrityConfig(``), nil)
		n := onlyProject(t, cfg).Networks[0]
		require.False(t, *n.DirectiveDefaults.EnforceHighestBlock)
	})

	// Whatever the operator did not name still gets its default. Deleting
	// the early defaults pass must not leave nil fields behind.
	t.Run("TheOtherDirectivesStillGetTheirDefaults", func(t *testing.T) {
		cfg := mustSetDefaultsFromYAML(t, legacyIntegrityConfig(`
      directiveDefaults:
        retryEmpty: false`), nil)
		dd := onlyProject(t, cfg).Networks[0].DirectiveDefaults
		require.NotNil(t, dd.EnforceGetLogsBlockRange)
		require.True(t, *dd.EnforceGetLogsBlockRange)
		require.NotNil(t, dd.EnforceNonNullTaggedBlocks)
		require.True(t, *dd.EnforceNonNullTaggedBlocks)
	})

	// An explicit directive still beats the legacy block. The migration is
	// allowed to fill a gap, never to overwrite what the operator wrote.
	t.Run("AnExplicitDirectiveStillWins", func(t *testing.T) {
		cfg := mustSetDefaultsFromYAML(t, legacyIntegrityConfig(`
      directiveDefaults:
        enforceHighestBlock: true`), nil)
		n := onlyProject(t, cfg).Networks[0]
		require.True(t, *n.DirectiveDefaults.EnforceHighestBlock,
			"an explicit directiveDefaults key must beat the deprecated integrity block")
	})
}
