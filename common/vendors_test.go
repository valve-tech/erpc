package common

import (
	"context"
	"net/http"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// panickingVendor stands in for the vendor nobody has written yet: it reads a
// config block the operator did not fill in, exactly as eight shipped vendors
// once did.
type panickingVendor struct {
	name string
}

func (v *panickingVendor) Name() string                          { return v.name }
func (v *panickingVendor) OwnsUpstream(ups *UpstreamConfig) bool { return false }

func (v *panickingVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, baseConfig *UpstreamConfig, settings VendorSettings) ([]*UpstreamConfig, error) {
	// The dereference every guarded vendor protects itself from.
	return []*UpstreamConfig{{Endpoint: "https://example.com"}}, nil
}

func (v *panickingVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings VendorSettings, networkId string) (bool, error) {
	return true, nil
}

func (v *panickingVendor) GetVendorSpecificErrorIfAny(req *NormalizedRequest, resp *http.Response, bodyObject interface{}, details map[string]interface{}) error {
	return nil
}

// nilEvmVendor reads the chain id straight off the evm block, with no guard.
type nilEvmVendor struct{ panickingVendor }

func (v *nilEvmVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, baseConfig *UpstreamConfig, settings VendorSettings) ([]*UpstreamConfig, error) {
	_ = baseConfig.Evm.ChainId // panics when the operator omits the evm block
	return nil, nil
}

// A vendor that forgets a nil check must not kill the process at bootstrap.
// Both callers of GenerateConfigs run during startup, so this guard is what
// keeps the ninth vendor — the one nobody has written yet — safe.
func TestGenerateVendorConfigs_AVendorPanicBecomesAConfigError(t *testing.T) {
	logger := zerolog.Nop()
	v := &nilEvmVendor{panickingVendor{name: "ninth"}}

	var cfgs []*UpstreamConfig
	var err error
	require.NotPanics(t, func() {
		cfgs, err = GenerateVendorConfigs(context.Background(), v, &logger, &UpstreamConfig{}, VendorSettings{})
	}, "the boundary must absorb a vendor panic")

	require.Error(t, err)
	assert.Nil(t, cfgs)
	assert.Contains(t, err.Error(), "ninth vendor failed to generate upstream configs",
		"the error must name the vendor the operator has to fix")
	assert.Contains(t, err.Error(), "evm",
		"the error must point at the block that is missing")
}

// The guard is a fallthrough, not a filter: a vendor that succeeds keeps its
// configs and its nil error.
func TestGenerateVendorConfigs_PassesAWorkingVendorThrough(t *testing.T) {
	logger := zerolog.Nop()
	v := &panickingVendor{name: "working"}

	cfgs, err := GenerateVendorConfigs(context.Background(), v, &logger, &UpstreamConfig{}, VendorSettings{})
	require.NoError(t, err)
	require.Len(t, cfgs, 1)
	assert.Equal(t, "https://example.com", cfgs[0].Endpoint)
}

// EvmChainId is how every vendor reads the chain id, so it must answer for a
// nil receiver and a missing evm block as well as for a real one.
func TestUpstreamConfig_EvmChainId_ReportsZeroWhenThereIsNoEvmBlock(t *testing.T) {
	var nilCfg *UpstreamConfig
	assert.Equal(t, int64(0), nilCfg.EvmChainId())
	assert.Equal(t, int64(0), (&UpstreamConfig{}).EvmChainId())
	assert.Equal(t, int64(0), (&UpstreamConfig{Evm: &EvmUpstreamConfig{}}).EvmChainId())
	assert.Equal(t, int64(137), (&UpstreamConfig{Evm: &EvmUpstreamConfig{ChainId: 137}}).EvmChainId())
}
