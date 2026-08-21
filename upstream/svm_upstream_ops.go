package upstream

import (
	"context"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
)

// svmVerifyGenesisHash guards against a mis-configured Solana upstream pointing
// at the wrong cluster (e.g. an upstream listed under mainnet-beta that actually
// serves devnet). It runs once at bootstrap.
//
// Known clusters (mainnet-beta, devnet, testnet) issue one getGenesisHash RPC
// at bootstrap and compare the result against the hardcoded genesis-hash
// table — this catches mis-pointed upstreams that a purely local check would
// miss. For known clusters this is fail-closed: both a hash mismatch AND a
// fetch failure reject the upstream, so we never register one we could not
// verify against the table. Unknown clusters are verified via the same RPC
// only when CheckGenesisHash:true is set; otherwise we skip silently to
// support private/local clusters where no genesis hash is known up front.
func (u *Upstream) svmVerifyGenesisHash(ctx context.Context) error {
	cfg := u.config
	if cfg == nil || cfg.Svm == nil {
		return nil
	}
	cluster := cfg.Svm.Cluster
	if cluster == "" {
		return nil
	}
	chain := cfg.Svm.Chain

	expected, known := common.KnownGenesisHash(chain, cluster)

	if !known && !cfg.Svm.CheckGenesisHash {
		u.logger.Debug().Str("cluster", cluster).
			Msg("skipping svm genesis hash validation: unknown cluster without checkGenesisHash")
		return nil
	}

	actual, err := u.svmFetchGenesisHash(ctx)
	if err != nil {
		// Fail closed. Control only reaches here when the cluster is known or
		// CheckGenesisHash is set (unknown + no-check returned above), so in
		// every remaining case we required validation — a fetch failure means
		// we could not verify the upstream and must not register it.
		return common.NewErrUpstreamClientInitialization(
			fmt.Errorf("svm getGenesisHash failed: %w", err),
			u,
		)
	}

	if known && expected != "" && !strings.EqualFold(actual, expected) {
		return common.NewErrUpstreamClientInitialization(
			fmt.Errorf("svm genesis hash mismatch for chain=%q cluster=%q: expected %s, got %s", common.ResolveSvmChain(chain), cluster, expected, actual),
			u,
		)
	}
	u.logger.Debug().Str("cluster", cluster).Str("genesisHash", actual).
		Msg("svm genesis hash validated")
	return nil
}

func (u *Upstream) svmFetchGenesisHash(ctx context.Context) (string, error) {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"getGenesisHash","params":[]}`))
	resp, err := u.Forward(ctx, req, true, false)
	if resp != nil {
		defer resp.Release()
	}
	if err != nil {
		return "", err
	}
	// KEPT DELIBERATELY: the next two checks do not fire today. Forward already
	// turns a JSON-RPC error answer into a returned error, so the check above
	// catches it (see TestSvmGenesisGate_RejectsAnUpstreamItCouldNotVerify), and
	// JsonRpcResponse reports a parse failure as jrr.Error rather than as err.
	// They stay because this is the identity gate: it decides whether an
	// upstream joins the pool, and the failure it prevents — a devnet node
	// serving mainnet queries — is silent. Both checks make it fail closed if
	// Forward ever passes an error answer through. jrr == nil is a documented
	// return of JsonRpcResponse for a nil response, and reading .Error on it
	// would panic.
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return "", err
	}
	if jrr == nil {
		return "", fmt.Errorf("no json-rpc response for getGenesisHash")
	}
	if jrr.Error != nil {
		return "", jrr.Error
	}
	var hash string
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &hash); err != nil {
		return "", fmt.Errorf("decode genesis hash: %w", err)
	}
	return hash, nil
}

// SvmStatePoller exposes the per-upstream SVM slot tracker for hooks and tests.
func (u *Upstream) SvmStatePoller() common.SvmStatePoller {
	return u.svmStatePoller
}
