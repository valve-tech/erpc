package common

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/rs/zerolog"
)

type Vendor interface {
	Name() string
	OwnsUpstream(upstream *UpstreamConfig) bool
	GenerateConfigs(ctx context.Context, logger *zerolog.Logger, baseConfig *UpstreamConfig, settings VendorSettings) ([]*UpstreamConfig, error)
	SupportsNetwork(ctx context.Context, logger *zerolog.Logger, settings VendorSettings, networkId string) (bool, error)
	GetVendorSpecificErrorIfAny(req *NormalizedRequest, resp *http.Response, bodyObject interface{}, details map[string]interface{}) error
}

// CreditUnitsProvider is an OPTIONAL capability a Vendor may implement to
// price upstream calls in its own credit units (Alchemy compute units,
// QuickNode API credits, …). It backs the opt-in cost accounting behind the
// X-ERPC-Credits response header: the upstream Forward path asks the vendor
// to price every physical attempt, so operators see the true upstream cost
// of a request (retries, hedges and consensus fan-out included; cache hits
// cost zero by construction).
//
// The VENDOR owns the pricing logic — nothing is hard-coded in the erpc
// layer. Most vendors resolve their publicly documented per-method table
// merged with the operator's per-method override (`upstream.CreditUnits`,
// populated from `providers[].settings.creditUnits`) via
// ResolveCreditUnits, but a vendor is free to price on anything it knows —
// request params, response classes, plan tiers, extra keys it reads from
// its settings at config-generation time. Values are the vendor's OWN
// units: deliberately not normalized, not comparable across vendors, not
// money. Vendors that do NOT implement this interface are costed at a flat
// 1 credit per request (opt out with `creditUnits: {"*": 0}`).
type CreditUnitsProvider interface {
	// CreditUnits prices ONE physical attempt of req against the given
	// upstream, in the vendor's own units. Called once per attempt by the
	// upstream Forward path when cost accounting is active.
	CreditUnits(req *NormalizedRequest, upstream *UpstreamConfig) int64
}

// ResolveCreditUnits is the shared table-resolution convention most
// CreditUnitsProvider implementations delegate to: the operator override
// wins per method over the vendor defaults, "*" is the per-table fallback
// for unlisted methods, and an entirely unpriced method costs a flat
// 1 credit per request (explicit "*": 0 opts out). Vendors remain free to
// price without it.
func ResolveCreditUnits(defaults, override map[string]int64, method string) int64 {
	if units, ok := override[method]; ok {
		return units
	}
	if units, ok := defaults[method]; ok {
		return units
	}
	if units, ok := override["*"]; ok {
		return units
	}
	if units, ok := defaults["*"]; ok {
		return units
	}
	return 1
}

// EvmChainId returns the EVM chain id the operator configured, or zero when
// there is no `evm` block at all. Vendors read the chain id through this
// method: a missing block then reaches the same "chainId is not defined"
// config error every vendor already reports, instead of dereferencing a nil
// pointer and killing the process at bootstrap.
func (u *UpstreamConfig) EvmChainId() int64 {
	if u == nil || u.Evm == nil {
		return 0
	}
	return u.Evm.ChainId
}

// GenerateVendorConfigs calls one vendor's GenerateConfigs and converts a panic
// into an error that names the vendor.
//
// A vendor reads config fields the operator may have left out, and every
// caller runs at bootstrap. The vendors themselves report a missing field by
// name, which is the message an operator wants. This guard is the fallthrough
// under them: a vendor that forgets a check — including a vendor nobody has
// written yet — reports a config error against its own name instead of
// crashing the process. Both callers of GenerateConfigs go through here.
func GenerateVendorConfigs(
	ctx context.Context,
	vendor Vendor,
	logger *zerolog.Logger,
	upstream *UpstreamConfig,
	settings VendorSettings,
) (cfgs []*UpstreamConfig, err error) {
	// Read the name inside the guarded scope: a vendor that panics here is
	// still a vendor that must not kill the process.
	name := "unknown"
	defer func() {
		if rec := recover(); rec != nil {
			cfgs = nil
			err = fmt.Errorf("%s vendor failed to generate upstream configs: %v (check the upstream config for a missing block, e.g. `evm`)", name, rec)
			if logger != nil {
				logger.Error().
					Str("vendor", name).
					Interface("panic", rec).
					Str("stack", string(debug.Stack())).
					Msg("vendor panicked while generating upstream configs")
			}
		}
	}()
	name = vendor.Name()
	return vendor.GenerateConfigs(ctx, logger, upstream, settings)
}
