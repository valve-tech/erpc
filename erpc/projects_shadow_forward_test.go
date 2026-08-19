package erpc

import (
	"context"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shadow_test.go drives executeShadowRequests directly and says so: "the
// project-level plumbing that calls it is one clone and a `go`". That clone is
// the part an operator depends on and nobody tests. PreparedProject.Forward
// copies the served JSON-RPC response, re-attaches the upstream, cache flag and
// attempt counts, and hands the copy to the mirror. If the copy loses the
// result, every candidate provider is compared against an empty answer and the
// mismatch counter condemns a provider that agreed.
//
// The tests below call Forward, not executeShadowRequests, so the clone is
// under test rather than assumed.

// forwardShadowRequest sends one JSON-RPC request through PreparedProject.Forward.
func forwardShadowRequest(t *testing.T, ctx context.Context, project *PreparedProject, method, params string) *common.NormalizedResponse {
	t.Helper()
	req := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":` + params + `}`))
	resp, err := project.Forward(ctx, "evm:123", req)
	require.NoError(t, err, "the primary upstream must answer the request")
	require.NotNil(t, resp)
	return resp
}

// TestProjectForward_MirrorsTheServedAnswerToTheShadowUpstream is the whole
// shadow path from one client request. The candidate must receive the same
// method, and the comparison must read the served result — so an agreeing
// candidate counts as identical. Asserting only that the candidate was called
// would pass even if the clone arrived empty.
func TestProjectForward_MirrorsTheServedAnswerToTheShadowUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// evmNode answers eth_blockNumber with its latestNum (1100 = 0x44c). Give
	// the candidate the same answer so agreement is the property under test.
	node := newShadowNode(t, "0x44c")
	project, network, shadows := startShadowErpc(t, ctx,
		&common.ShadowUpstreamConfig{Enabled: true}, node)

	before := shadowIdentical(t, shadows[0], network, "eth_blockNumber")

	resp := forwardShadowRequest(t, ctx, project, "eth_blockNumber", `[]`)
	jrr, err := resp.JsonRpcResponse(ctx)
	require.NoError(t, err)
	require.Equal(t, `"0x44c"`, string(jrr.GetResultBytes()),
		"the client must be served the primary upstream's answer, not the candidate's")

	require.Eventually(t, func() bool {
		return node.sawMethod("eth_blockNumber")
	}, 5*time.Second, 20*time.Millisecond,
		"Forward must mirror the request to the shadow upstream")

	require.Eventually(t, func() bool {
		return shadowIdentical(t, shadows[0], network, "eth_blockNumber") > before
	}, 5*time.Second, 20*time.Millisecond,
		"the cloned response must still carry the served result, so an agreeing candidate counts as identical")
}

// TestProjectForward_CountsADisagreeingCandidateAsMismatch is the same path
// with the candidate answering differently. Without it, a clone that dropped
// the result would still satisfy the test above whenever the candidate also
// returned nothing.
//
// It uses eth_getBlockByNumber rather than eth_blockNumber on purpose. The
// shadow counters are process-global Prometheus vectors keyed by project,
// upstream and method, and shadow_test.go asserts an absolute zero on the
// eth_blockNumber mismatch counter for this same project and upstream. Any test
// in the binary that mirrors a disagreement on that exact label set breaks it.
func TestProjectForward_CountsADisagreeingCandidateAsMismatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The candidate answers eth_getBlockByNumber with hash 0xabc; the primary
	// answers the same block with its own hash, so the two disagree.
	node := newShadowNode(t, "0x44c")
	project, network, shadows := startShadowErpc(t, ctx,
		&common.ShadowUpstreamConfig{Enabled: true}, node)

	mismatchBefore := shadowMismatch(t, shadows[0], network, "eth_getBlockByNumber")
	identicalBefore := shadowIdentical(t, shadows[0], network, "eth_getBlockByNumber")

	forwardShadowRequest(t, ctx, project, "eth_getBlockByNumber", `["0x64",false]`)

	require.Eventually(t, func() bool {
		return shadowMismatch(t, shadows[0], network, "eth_getBlockByNumber") > mismatchBefore
	}, 20*time.Second, 20*time.Millisecond,
		"a candidate that answers differently must be counted as a mismatch")

	assert.Equal(t, identicalBefore, shadowIdentical(t, shadows[0], network, "eth_getBlockByNumber"),
		"a disagreeing candidate must never also be counted as identical")
}
