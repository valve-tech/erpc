package svm

import (
	"context"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog/log"
)

// memConnector is the connector config every test here reuses.
func memConnector(id string) *common.ConnectorConfig {
	return &common.ConnectorConfig{
		Id:     id,
		Driver: common.DriverMemory,
		Memory: &common.MemoryConnectorConfig{MaxItems: 100, MaxTotalSize: "1MB"},
	}
}

func buildCache(t *testing.T, cfg *common.CacheConfig) (*SvmJsonRpcCache, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewSvmJsonRpcCache(ctx, &log.Logger, cfg)
}

// A policy naming a connector that does not exist is a config typo. Building
// the cache anyway would leave that policy silently doing nothing, so every
// request it was meant to cache goes to an upstream while the operator
// believes it is cached.
func TestNewSvmJsonRpcCache_PolicyNamingAnUnknownConnectorIsRefused(t *testing.T) {
	_, err := buildCache(t, &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{memConnector("mem")},
		Policies: []*common.CachePolicyConfig{
			{Connector: "typo", Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized},
		},
	})
	if err == nil {
		t.Fatal("a policy pointing at a non-existent connector was accepted")
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Fatalf("error = %v, want it to name the connector the operator mistyped", err)
	}
}

// A connector eRPC cannot build must fail at boot rather than at first
// request. A cache that comes up half-wired hides a storage misconfiguration
// until traffic arrives.
func TestNewSvmJsonRpcCache_UnbuildableConnectorIsRefusedAtBoot(t *testing.T) {
	_, err := buildCache(t, &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{{Id: "nope", Driver: common.ConnectorDriverType("no-such-driver")}},
	})
	if err == nil {
		t.Fatal("an unknown connector driver was accepted")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error = %v, want it to name the offending connector", err)
	}
}

// A policy eRPC cannot build must also fail at boot, with the reason attached.
// An unparseable item-size limit is the realistic case: starting anyway would
// leave a size guard that never applies, so oversized Solana blocks reach a
// store sized for small entries.
func TestNewSvmJsonRpcCache_UnbuildablePolicyIsRefusedAtBoot(t *testing.T) {
	badSize := "one-and-a-bit-megabytes"
	_, err := buildCache(t, &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{memConnector("mem")},
		Policies: []*common.CachePolicyConfig{
			{Connector: "mem", Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized,
				MaxItemSize: &badSize},
		},
	})
	if err == nil {
		t.Fatal("a policy with an unparseable maxItemSize was accepted")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "policy") {
		t.Fatalf("error = %v, want it to name the policy", err)
	}
}

// The decoder must exist even when compression is OFF. Entries written while
// compression was enabled stay in the store after an operator turns it off; a
// cache with no decoder would then serve compressed bytes as a result.
func TestNewSvmJsonRpcCache_DecoderExistsEvenWithCompressionDisabled(t *testing.T) {
	c, err := buildCache(t, &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{memConnector("mem")},
		Policies: []*common.CachePolicyConfig{
			{Connector: "mem", Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized},
		},
	})
	if err != nil {
		t.Fatalf("NewSvmJsonRpcCache: %v", err)
	}
	if c.decoder == nil {
		t.Fatal("no zstd decoder; entries written under a previous compression setting become unreadable")
	}
	if c.encoder != nil {
		t.Fatal("an encoder was built while compression is disabled")
	}
	if c.compressionThreshold != 0 {
		t.Fatalf("compressionThreshold = %d with compression off, want 0", c.compressionThreshold)
	}
}

// Every documented zstd level must be accepted, and an unknown one must fall
// back rather than fail the process. Solana getBlock payloads routinely exceed
// a megabyte, so refusing to start over a typo'd level costs more than
// quietly using the default.
func TestNewSvmJsonRpcCache_CompressionLevels(t *testing.T) {
	for _, level := range []string{"", "fastest", "default", "better", "best", "wildly-wrong"} {
		t.Run("level="+level, func(t *testing.T) {
			c, err := buildCache(t, &common.CacheConfig{
				Connectors: []*common.ConnectorConfig{memConnector("mem")},
				Policies: []*common.CachePolicyConfig{
					{Connector: "mem", Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized},
				},
				Compression: &common.CompressionConfig{Enabled: &common.TRUE, ZstdLevel: level},
			})
			if err != nil {
				t.Fatalf("zstd level %q was rejected: %v", level, err)
			}
			if c.encoder == nil {
				t.Fatalf("zstd level %q produced no encoder", level)
			}
			if c.compressionThreshold != 512 {
				t.Fatalf("default threshold = %d, want 512", c.compressionThreshold)
			}
		})
	}
}

// A configured threshold must override the default, or an operator cannot tune
// how small a payload is worth compressing.
func TestNewSvmJsonRpcCache_ConfiguredThresholdOverridesTheDefault(t *testing.T) {
	c, err := buildCache(t, &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{memConnector("mem")},
		Policies: []*common.CachePolicyConfig{
			{Connector: "mem", Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized},
		},
		Compression: &common.CompressionConfig{Enabled: &common.TRUE, Threshold: 4096},
	})
	if err != nil {
		t.Fatalf("NewSvmJsonRpcCache: %v", err)
	}
	if c.compressionThreshold != 4096 {
		t.Fatalf("compressionThreshold = %d, want the configured 4096", c.compressionThreshold)
	}
}

// Compression must stay OFF unless it is explicitly enabled. Turning it on by
// default would rewrite every entry format without the operator asking.
func TestNewSvmJsonRpcCache_CompressionStaysOffUnlessEnabled(t *testing.T) {
	cases := map[string]*common.CompressionConfig{
		"absent section":   nil,
		"explicitly false": {Enabled: &common.FALSE},
		"enabled unset":    {ZstdLevel: "best"},
	}
	for name, comp := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := buildCache(t, &common.CacheConfig{
				Connectors: []*common.ConnectorConfig{memConnector("mem")},
				Policies: []*common.CachePolicyConfig{
					{Connector: "mem", Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized},
				},
				Compression: comp,
			})
			if err != nil {
				t.Fatalf("NewSvmJsonRpcCache: %v", err)
			}
			if c.encoder != nil {
				t.Fatal("compression was enabled without the operator asking")
			}
		})
	}
}

// WithProjectId must produce a copy that SHARES the policies, connectors and
// codecs. Copying them per project would open a second connection pool per
// project against the same store.
func TestWithProjectId_SharesTheUnderlyingPoliciesAndCodecs(t *testing.T) {
	base, err := buildCache(t, &common.CacheConfig{
		Connectors: []*common.ConnectorConfig{memConnector("mem")},
		Policies: []*common.CachePolicyConfig{
			{Connector: "mem", Network: "*", Method: "*", Finality: common.DataFinalityStateFinalized},
		},
		Compression: &common.CompressionConfig{Enabled: &common.TRUE},
	})
	if err != nil {
		t.Fatalf("NewSvmJsonRpcCache: %v", err)
	}

	scoped := base.WithProjectId("prj-a")

	if scoped == base {
		t.Fatal("WithProjectId returned the same cache; the project id would leak between projects")
	}
	if scoped.projectId != "prj-a" {
		t.Fatalf("projectId = %q, want prj-a", scoped.projectId)
	}
	if base.projectId != "" {
		t.Fatalf("the base cache was mutated to project %q", base.projectId)
	}
	if scoped.encoder != base.encoder || scoped.decoder != base.decoder {
		t.Fatal("WithProjectId built new zstd codecs instead of sharing them")
	}
	if len(scoped.policies) != len(base.policies) {
		t.Fatal("WithProjectId did not carry the policies over; the scoped cache would never hit")
	}
	if scoped.compressionThreshold != base.compressionThreshold {
		t.Fatalf("compressionThreshold = %d in the scoped copy, want %d",
			scoped.compressionThreshold, base.compressionThreshold)
	}
}

// A cache with no policies must report itself null so callers skip it
// entirely. Dispatching to it would cost a lookup on every request for no
// possible hit.
func TestIsObjectNull_NilAndPolicylessCachesReportNull(t *testing.T) {
	var nilCache *SvmJsonRpcCache
	if !nilCache.IsObjectNull() {
		t.Fatal("a nil cache did not report itself null")
	}

	empty := &SvmJsonRpcCache{}
	if !empty.IsObjectNull() {
		t.Fatal("a cache with no policies did not report itself null")
	}

	empty.SetPolicies(make([]*data.CachePolicy, 1))
	if empty.IsObjectNull() {
		t.Fatal("a cache with a policy still reports itself null; it would never be consulted")
	}
}
