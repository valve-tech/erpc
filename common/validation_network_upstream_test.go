package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Network and upstream validation is the last gate before erpc starts routing.
// Anything wrong that gets past it shows up as traffic going to the wrong place
// — or nowhere — with no config error to point at.

func validEvmNetwork() *EvmNetworkConfig {
	return &EvmNetworkConfig{
		ChainId:                     1,
		FallbackFinalityDepth:       128,
		FallbackStatePollerDebounce: Duration(time.Second),
		GetLogsMaxAllowedRange:      10000,
	}
}

func TestNetworkConfig_Validate_ArchitectureNeedsItsBlock(t *testing.T) {
	cfg := &Config{}

	require.ErrorContains(t, (&NetworkConfig{}).Validate(cfg), "network.*.architecture is required")

	require.ErrorContains(t,
		(&NetworkConfig{Architecture: ArchitectureEvm}).Validate(cfg),
		"network.*.evm is required for evm networks")
	require.ErrorContains(t,
		(&NetworkConfig{Architecture: ArchitectureSvm}).Validate(cfg),
		"network.*.svm is required for svm networks")

	require.NoError(t, (&NetworkConfig{Architecture: ArchitectureEvm, Evm: validEvmNetwork()}).Validate(cfg))
}

func TestNetworkConfig_Validate_PropagatesChildErrors(t *testing.T) {
	cfg := &Config{}
	base := func() *NetworkConfig {
		return &NetworkConfig{Architecture: ArchitectureEvm, Evm: validEvmNetwork()}
	}

	t.Run("failsafe", func(t *testing.T) {
		n := base()
		n.Failsafe = []*FailsafeConfig{{}}
		require.ErrorContains(t, n.Validate(cfg), "matchMethod cannot be empty")
	})

	t.Run("selectionPolicy", func(t *testing.T) {
		n := base()
		n.SelectionPolicy = &SelectionPolicyConfig{}
		require.ErrorContains(t, n.Validate(cfg), "evalInterval must be greater than 0")
	})

	// A static response that names no method would shadow nothing, or worse,
	// everything. The index of the bad entry must be in the message.
	t.Run("staticResponses index is reported", func(t *testing.T) {
		n := base()
		n.StaticResponses = []*StaticResponseConfig{
			{Method: "eth_chainId", Response: &StaticResponseBodyConfig{Result: "0x1"}},
			{Method: ""},
		}
		err := n.Validate(cfg)
		require.ErrorContains(t, err, "staticResponses[1]")
		require.ErrorContains(t, err, "method is required")
	})

	t.Run("integrity", func(t *testing.T) {
		n := base()
		n.Integrity = &IntegrityConfig{HeaderMode: "loud"}
		err := n.Validate(cfg)
		require.ErrorContains(t, err, "network.*:")
		require.ErrorContains(t, err, "headerMode 'loud' is invalid")
	})
}

func TestNetworkConfig_Validate_AliasAndRateLimitBudget(t *testing.T) {
	base := func() *NetworkConfig {
		return &NetworkConfig{Architecture: ArchitectureEvm, Evm: validEvmNetwork()}
	}

	// The alias becomes part of a URL path. A slash or a space in it would
	// produce a route no client can reach.
	t.Run("alias must be an identifier", func(t *testing.T) {
		n := base()
		n.Alias = "main net"
		require.ErrorContains(t, n.Validate(&Config{}), "must contain only alphanumeric characters")

		n.Alias = "main-net_1"
		require.NoError(t, n.Validate(&Config{}))
	})

	t.Run("rateLimitBudget must exist", func(t *testing.T) {
		n := base()
		n.RateLimitBudget = "ghost"
		err := n.Validate(&Config{})
		require.ErrorContains(t, err, "does not exist in config.rateLimiters")

		cfg := &Config{RateLimiters: &RateLimiterConfig{
			Budgets: []*RateLimitBudgetConfig{{Id: "ghost"}},
		}}
		require.NoError(t, n.Validate(cfg))
	})
}

// A static response replaces an upstream call entirely. An entry that sets both
// a result and an error, or neither, has no defined meaning — serving one half
// arbitrarily would give clients a different answer per restart.
func TestStaticResponseConfig_Validate_ExactlyOneOfResultOrError(t *testing.T) {
	require.ErrorContains(t, (*StaticResponseConfig)(nil).Validate(), "entry is nil")
	require.ErrorContains(t, (&StaticResponseConfig{}).Validate(), "method is required")
	require.ErrorContains(t, (&StaticResponseConfig{Method: "eth_chainId"}).Validate(), "response is required")

	require.ErrorContains(t,
		(&StaticResponseConfig{Method: "m", Response: &StaticResponseBodyConfig{}}).Validate(),
		"exactly one of result or error, got neither")

	require.ErrorContains(t,
		(&StaticResponseConfig{Method: "m", Response: &StaticResponseBodyConfig{
			Result: "0x1",
			Error:  &StaticResponseErrorConfig{Code: -32000, Message: "boom"},
		}}).Validate(),
		"exactly one of result or error, got both")

	// A JSON-RPC error object with no message is not a usable error for a
	// client library, so it must be refused.
	require.ErrorContains(t,
		(&StaticResponseConfig{Method: "m", Response: &StaticResponseBodyConfig{
			Error: &StaticResponseErrorConfig{Code: -32000},
		}}).Validate(),
		"response.error.message is required")

	require.NoError(t,
		(&StaticResponseConfig{Method: "m", Response: &StaticResponseBodyConfig{Result: "0x1"}}).Validate())
	require.NoError(t,
		(&StaticResponseConfig{Method: "m", Response: &StaticResponseBodyConfig{
			Error: &StaticResponseErrorConfig{Code: -32000, Message: "not supported"},
		}}).Validate())
}

func TestEvmNetworkConfig_Validate_RequiredNumbers(t *testing.T) {
	require.NoError(t, validEvmNetwork().Validate())

	e := validEvmNetwork()
	e.FallbackFinalityDepth = 0
	require.ErrorContains(t, e.Validate(), "fallbackFinalityDepth must be greater than 0")

	e = validEvmNetwork()
	e.FallbackStatePollerDebounce = 0
	require.ErrorContains(t, e.Validate(), "fallbackStatePollerDebounce is required")

	e = validEvmNetwork()
	e.GetLogsMaxAllowedRange = 0
	require.ErrorContains(t, e.Validate(), "getLogsMaxAllowedRange must be greater than 0")
}

func TestUpstreamConfig_Validate_EndpointAndChildErrors(t *testing.T) {
	cfg := &Config{}
	base := func() *UpstreamConfig {
		return &UpstreamConfig{Id: "up", Endpoint: "https://rpc.example.com"}
	}

	require.NoError(t, base().Validate(cfg, false))

	// A provider override has no endpoint of its own — the provider builds it —
	// so the endpoint check must be skippable.
	require.ErrorContains(t, (&UpstreamConfig{Id: "up"}).Validate(cfg, false), "upstream.*.endpoint is required")
	require.NoError(t, (&UpstreamConfig{Id: "up"}).Validate(cfg, true))

	cases := []struct {
		name    string
		mutate  func(*UpstreamConfig)
		wantSub string
	}{
		{"evm", func(u *UpstreamConfig) { u.Evm = &EvmUpstreamConfig{} }, "statePollerInterval is required"},
		{"svm", func(u *UpstreamConfig) {
			u.Type = UpstreamTypeSvm
			u.Svm = &SvmUpstreamConfig{}
		}, "svm.cluster is required"},
		{"failsafe", func(u *UpstreamConfig) { u.Failsafe = []*FailsafeConfig{{}} }, "matchMethod cannot be empty"},
		{"jsonRpc", func(u *UpstreamConfig) {
			yes := true
			u.JsonRpc = &JsonRpcUpstreamConfig{SupportsBatch: &yes}
		}, "batchMaxWait is required"},
		{"grpc", func(u *UpstreamConfig) { u.Grpc = &GrpcUpstreamConfig{PoolSize: -1} }, "poolSize must not be negative"},
		{"rateLimitAutoTune", func(u *UpstreamConfig) {
			yes := true
			u.RateLimitAutoTune = &RateLimitAutoTuneConfig{Enabled: &yes}
		}, "adjustmentPeriod is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := base()
			tc.mutate(u)
			require.ErrorContains(t, u.Validate(cfg, false), tc.wantSub)
		})
	}

	t.Run("rateLimitBudget must exist", func(t *testing.T) {
		u := base()
		u.RateLimitBudget = "ghost"
		require.ErrorContains(t, u.Validate(cfg, false), "does not exist in config.rateLimiters")
	})
}

func TestEvmUpstreamConfig_Validate_ChainIdAndNodeType(t *testing.T) {
	base := func(endpoint string) (*EvmUpstreamConfig, *UpstreamConfig) {
		return &EvmUpstreamConfig{StatePollerInterval: Duration(10 * time.Second)},
			&UpstreamConfig{Id: "up", Endpoint: endpoint}
	}

	e, u := base("https://rpc.example.com")
	e.ChainId = 1
	require.NoError(t, e.Validate(u))

	// A non-native endpoint (an alias like `alchemy://...`) resolves its chain
	// from the provider. A chainId there would be silently ignored, so it is
	// rejected instead.
	e, u = base("alchemy://key")
	e.ChainId = 1
	err := e.Validate(u)
	require.ErrorContains(t, err, "chainId must be 0 for non-http endpoints")

	e, u = base("alchemy://key")
	require.NoError(t, e.Validate(u), "chainId 0 on a provider endpoint is the normal case")

	t.Run("statePollerInterval is required", func(t *testing.T) {
		u := &UpstreamConfig{Id: "up", Endpoint: "https://rpc.example.com"}
		require.ErrorContains(t, (&EvmUpstreamConfig{}).Validate(u), "statePollerInterval is required")
	})

	t.Run("nodeType is a closed set", func(t *testing.T) {
		e, u := base("https://rpc.example.com")
		for _, nt := range []EvmNodeType{EvmNodeTypeUnknown, EvmNodeTypeArchive, EvmNodeTypeFull} {
			e.NodeType = nt
			require.NoError(t, e.Validate(u), "nodeType %s", nt)
		}
		e.NodeType = "pruned"
		require.ErrorContains(t, e.Validate(u), "nodeType 'pruned' is invalid")
	})

	t.Run("blockAvailability errors propagate", func(t *testing.T) {
		e, u := base("https://rpc.example.com")
		e.BlockAvailability = &EvmBlockAvailabilityConfig{Lower: &EvmAvailabilityBoundConfig{}}
		require.ErrorContains(t, e.Validate(u), "blockAvailability.lower is invalid")
	})
}

// Block-availability bounds decide which upstream may answer for a given block.
// A bound that reads as "serves nothing" removes the upstream from every
// request while the health check still reports it up.
func TestEvmAvailabilityBoundConfig_Validate_ExactlyOneBoundKind(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }

	require.ErrorContains(t, (&EvmAvailabilityBoundConfig{}).Validate(),
		"must set exactly one of: exactBlock, latestBlockMinus, earliestBlockPlus")

	require.ErrorContains(t,
		(&EvmAvailabilityBoundConfig{ExactBlock: i64(1), LatestBlockMinus: i64(1)}).Validate(),
		"are mutually exclusive")

	t.Run("exactBlock forbids probe and updateRate", func(t *testing.T) {
		require.NoError(t, (&EvmAvailabilityBoundConfig{ExactBlock: i64(0)}).Validate())

		// A fixed block never moves, so a probe or a refresh rate on it would
		// suggest a liveness check that never runs.
		require.ErrorContains(t,
			(&EvmAvailabilityBoundConfig{ExactBlock: i64(1), Probe: EvmProbeBlockHeader}).Validate(),
			"probe must be empty when exactBlock is set")
		require.ErrorContains(t,
			(&EvmAvailabilityBoundConfig{ExactBlock: i64(1), UpdateRate: Duration(time.Second)}).Validate(),
			"updateRate must be 0 when exactBlock is set")
		require.ErrorContains(t,
			(&EvmAvailabilityBoundConfig{ExactBlock: i64(-1)}).Validate(),
			"exactBlock must be >= 0")
	})

	t.Run("relative bounds must be non-negative", func(t *testing.T) {
		require.ErrorContains(t,
			(&EvmAvailabilityBoundConfig{LatestBlockMinus: i64(-1)}).Validate(),
			"latestBlockMinus must be >= 0")
		require.ErrorContains(t,
			(&EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(-1)}).Validate(),
			"earliestBlockPlus must be >= 0")
		require.ErrorContains(t,
			(&EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(1), UpdateRate: Duration(-time.Second)}).Validate(),
			"updateRate must be >= 0")
	})

	t.Run("probe is a closed set", func(t *testing.T) {
		for _, p := range []EvmAvailabilityProbeType{
			EvmProbeBlockHeader, EvmProbeEventLogs, EvmProbeCallState, EvmProbeTraceData,
		} {
			require.NoError(t,
				(&EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(0), Probe: p}).Validate(), "probe %s", p)
		}
		// An unknown probe name would fall through to the default probe, so the
		// operator would believe a check is running that is not.
		require.ErrorContains(t,
			(&EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(0), Probe: "receipts"}).Validate(),
			"probe 'receipts' is invalid")
	})

	// updateRate is ignored for latestBlockMinus, so it only warns.
	t.Run("latestBlockMinus with updateRate only warns", func(t *testing.T) {
		require.NoError(t,
			(&EvmAvailabilityBoundConfig{LatestBlockMinus: i64(128), UpdateRate: Duration(time.Minute)}).Validate())
	})
}

// Both bounds together describe a window. An inverted window serves nothing,
// which looks identical to an upstream that is simply never selected.
func TestEvmBlockAvailabilityConfig_Validate_LowerMustNotExceedUpper(t *testing.T) {
	i64 := func(v int64) *int64 { return &v }

	require.NoError(t, (&EvmBlockAvailabilityConfig{}).Validate())

	t.Run("exact blocks", func(t *testing.T) {
		c := &EvmBlockAvailabilityConfig{
			Lower: &EvmAvailabilityBoundConfig{ExactBlock: i64(200)},
			Upper: &EvmAvailabilityBoundConfig{ExactBlock: i64(100)},
		}
		require.ErrorContains(t, c.Validate(), "lower.exactBlock must be <= upper.exactBlock")

		c.Lower.ExactBlock = i64(100)
		c.Upper.ExactBlock = i64(200)
		require.NoError(t, c.Validate())
	})

	// latestBlockMinus counts backwards, so the lower bound must be the LARGER
	// offset. Getting this backwards is the easy mistake and silently empties
	// the window.
	t.Run("latestBlockMinus is inverted", func(t *testing.T) {
		c := &EvmBlockAvailabilityConfig{
			Lower: &EvmAvailabilityBoundConfig{LatestBlockMinus: i64(10)},
			Upper: &EvmAvailabilityBoundConfig{LatestBlockMinus: i64(100)},
		}
		require.ErrorContains(t, c.Validate(), "lower.latestBlockMinus must be >= upper.latestBlockMinus")

		c.Lower.LatestBlockMinus = i64(100)
		c.Upper.LatestBlockMinus = i64(10)
		require.NoError(t, c.Validate())
	})

	t.Run("earliestBlockPlus counts forwards", func(t *testing.T) {
		c := &EvmBlockAvailabilityConfig{
			Lower: &EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(100)},
			Upper: &EvmAvailabilityBoundConfig{EarliestBlockPlus: i64(10)},
		}
		require.ErrorContains(t, c.Validate(), "lower.earliestBlockPlus must be <= upper.earliestBlockPlus")

		c.Lower.EarliestBlockPlus = i64(10)
		c.Upper.EarliestBlockPlus = i64(100)
		require.NoError(t, c.Validate())
	})

	t.Run("upper bound errors are located", func(t *testing.T) {
		c := &EvmBlockAvailabilityConfig{Upper: &EvmAvailabilityBoundConfig{}}
		require.ErrorContains(t, c.Validate(), "blockAvailability.upper is invalid")
	})
}
