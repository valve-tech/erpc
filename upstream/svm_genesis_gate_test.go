package upstream

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The genesis-hash gate is what stops eRPC registering a node listed under
// mainnet-beta that actually serves devnet. It is fail-closed on purpose: an
// upstream that cannot be verified never joins the pool, because the failure
// mode it prevents — answering mainnet queries with devnet state — is silent.

const svmTestEndpoint = "http://svm1.localhost"

// newSvmUpstream builds a real Upstream with a real HTTP client, so the genesis
// fetch goes through the same Forward path production uses.
func newSvmUpstream(t *testing.T, svm *common.SvmUpstreamConfig) *Upstream {
	t.Helper()
	reg, _ := newBootstrapTestRegistry(t)
	ups, err := reg.NewUpstream(&common.UpstreamConfig{
		Id:       "svm1",
		Type:     common.UpstreamTypeSvm,
		Endpoint: svmTestEndpoint,
		Svm:      svm,
	})
	require.NoError(t, err)
	ups.networkId.Store(util.SvmNetworkId(svm.Chain, svm.Cluster))
	return ups
}

func mockGenesisHash(hash string) {
	gock.New(svmTestEndpoint).
		Post("").
		Persist().
		Filter(gockBodyContains("getGenesisHash")).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + hash + `"}`))
}

func TestSvmGenesisGate_AcceptsAnUpstreamOnTheRightCluster(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	mockGenesisHash("5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d")

	u := newSvmUpstream(t, &common.SvmUpstreamConfig{Chain: "solana", Cluster: "mainnet-beta"})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	require.NoError(t, u.svmVerifyGenesisHash(ctx))
}

func TestSvmGenesisGate_RejectsAnUpstreamOnTheWrongCluster(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	// The devnet genesis hash, served by a node the operator listed as mainnet.
	mockGenesisHash("EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG")

	u := newSvmUpstream(t, &common.SvmUpstreamConfig{Chain: "solana", Cluster: "mainnet-beta"})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err := u.svmVerifyGenesisHash(ctx)

	require.Error(t, err, "a devnet node registered as mainnet would answer mainnet queries with devnet state")
	// The operator has to be able to read BOTH hashes to see what happened.
	assert.Contains(t, err.Error(), "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d")
	assert.Contains(t, err.Error(), "EtWTRABZaYq6iMfeYKouRu166VU2xqa1wcaWoxPkrZBG")
	assert.Contains(t, err.Error(), "mainnet-beta")
}

func TestSvmGenesisGate_RejectsAnUpstreamItCouldNotVerify(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	// The node answers the RPC with an error, so nothing can be compared.
	gock.New(svmTestEndpoint).
		Post("").
		Persist().
		Filter(gockBodyContains("getGenesisHash")).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))

	u := newSvmUpstream(t, &common.SvmUpstreamConfig{Chain: "solana", Cluster: "mainnet-beta"})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err := u.svmVerifyGenesisHash(ctx)

	// Fail CLOSED. Accepting an unverifiable node is the same risk as accepting
	// a wrong one, because the wrong one also cannot be verified.
	require.Error(t, err, "an unverifiable upstream must not be registered")
	assert.Contains(t, err.Error(), "getGenesisHash")
	// The NODE's own words have to survive. "method not found" tells the
	// operator this build does not implement getGenesisHash; a tidy
	// eRPC-authored replacement would send them looking at the wrong thing.
	assert.Contains(t, err.Error(), "method not found",
		"the upstream's own error was replaced on the way out")
}

func TestSvmGenesisGate_UnknownClusterOptsInWithCheckGenesisHash(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	// A private cluster: no entry in the table, so any hash is acceptable —
	// the check only proves the node answers and speaks Solana RPC.
	mockGenesisHash("SomePrivateClusterGenesisHash11111111111111")

	u := newSvmUpstream(t, &common.SvmUpstreamConfig{
		Chain: "solana", Cluster: "localnet", CheckGenesisHash: true,
	})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	assert.NoError(t, u.svmVerifyGenesisHash(ctx),
		"an unknown cluster has no expected hash to mismatch against")
}

func TestSvmGenesisGate_UnknownClusterThatOptedInStillFailsClosed(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.New(svmTestEndpoint).
		Post("").
		Persist().
		Filter(gockBodyContains("getGenesisHash")).
		Reply(500).
		JSON([]byte(`{"error":{"code":-32603,"message":"boom"}}`))

	u := newSvmUpstream(t, &common.SvmUpstreamConfig{
		Chain: "solana", Cluster: "localnet", CheckGenesisHash: true,
	})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	// Opting in means the check is required; a node that cannot answer it is
	// exactly as unverified as a known cluster that cannot answer it.
	require.Error(t, u.svmVerifyGenesisHash(ctx),
		"checkGenesisHash:true must not degrade to a best-effort check")
}

// TestSvmGenesisGate_RejectsAResultThatIsNotAHash covers the decode step. A
// node that answers getGenesisHash with a number rather than a base58 string
// speaks JSON-RPC well enough for Forward to call it a success, so the decode
// is the only thing between that answer and a registered upstream.
func TestSvmGenesisGate_RejectsAResultThatIsNotAHash(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.New(svmTestEndpoint).
		Post("").
		Persist().
		Filter(gockBodyContains("getGenesisHash")).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":12345}`))

	u := newSvmUpstream(t, &common.SvmUpstreamConfig{Chain: "solana", Cluster: "mainnet-beta"})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err := u.svmVerifyGenesisHash(ctx)

	require.Error(t, err, "a non-string result must not be registered as a genesis hash")
	// Discriminating: the mismatch branch reports "genesis hash mismatch". This
	// one has to say the result could not be read at all, or an operator goes
	// looking for a cluster problem that is not there.
	assert.Contains(t, err.Error(), "decode genesis hash")
}
