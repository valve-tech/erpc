package thirdparty

import (
	"context"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
)

// unguardedVendor reads the chain id straight off the evm block, the way eight
// shipped vendors once did. It stands in for the ninth vendor — the one nobody
// has written yet.
type unguardedVendor struct{ common.Vendor }

func (v *unguardedVendor) Name() string { return "unguarded" }

func (v *unguardedVendor) OwnsUpstream(upstream *common.UpstreamConfig) bool { return false }

func (v *unguardedVendor) SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings common.VendorSettings, networkId string) (bool, error) {
	return true, nil
}

func (v *unguardedVendor) GetVendorSpecificErrorIfAny(req *common.NormalizedRequest, resp *http.Response, bodyObject interface{}, details map[string]interface{}) error {
	return nil
}

func (v *unguardedVendor) GenerateConfigs(ctx context.Context, logger *zerolog.Logger, upstream *common.UpstreamConfig, settings common.VendorSettings) ([]*common.UpstreamConfig, error) {
	_ = upstream.Evm.ChainId // panics when the operator omits the evm block
	return []*common.UpstreamConfig{upstream}, nil
}

// A provider builds its upstreams on a bootstrap task, so a vendor panic there
// took the process down at startup. The provider reports the vendor instead.
// This is the fallthrough that keeps a vendor nobody has reviewed from
// crashing eRPC.
func TestProvider_GenerateUpstreamConfigs_AVendorPanicBecomesAnError(t *testing.T) {
	logger := zerolog.Nop()

	provider := NewProvider(&logger, &common.ProviderConfig{
		Id:                 "unguarded-provider",
		Vendor:             "unguarded",
		UpstreamIdTemplate: "<PROVIDER>-<NETWORK>",
		// The override carries no evm block, which is what the vendor
		// dereferences. A non-evm network id keeps the provider from filling
		// one in on its way past.
		Overrides: map[string]*common.UpstreamConfig{
			"*": {Type: "svm"},
		},
	}, &unguardedVendor{}, nil)

	var cfgs []*common.UpstreamConfig
	var err error
	assert.NotPanics(t, func() {
		cfgs, err = provider.GenerateUpstreamConfigs(context.Background(), &logger, "svm:solana-mainnet")
	}, "a vendor panic must not reach the bootstrap task")

	assert.Error(t, err)
	assert.Nil(t, cfgs)
	assert.Contains(t, err.Error(), "unguarded vendor failed to generate upstream configs")
}
