package evm

import (
	"context"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	promUtil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// With `enforceHighestBlock` on, an eth_blockNumber answer below the tip is a
// lagging upstream. The hook replaces the answer with the tip and charges the
// lag to that upstream, which is what an operator alarms on. Both halves matter:
// a correction with no attribution hides the vendor that caused it.

// staleBlockNumberUpstream builds an upstream that reports the given id and
// vendor, which are the labels the stale-block counter carries.
func staleBlockNumberUpstream(id, vendor string) *mockEvmUpstream {
	u := new(mockEvmUpstream)
	u.On("Id").Return(id).Maybe()
	u.On("VendorName").Return(vendor).Maybe()
	u.On("NetworkId").Return("evm:1").Maybe()
	u.On("NetworkLabel").Return("evm:1").Maybe()
	return u
}

// blockNumberResponse builds an eth_blockNumber result served by `ups`.
func blockNumberResponse(blockNumber int64, ups common.Upstream) *common.NormalizedResponse {
	hex, _ := common.NormalizeHex(blockNumber)
	nr := common.NewNormalizedResponse().WithJsonRpcResponse(
		common.MustNewJsonRpcResponseFromBytes([]byte(`1`), []byte(`"`+hex+`"`), nil),
	)
	if ups != nil {
		nr.SetUpstream(ups)
	}
	return nr
}

// enforcingBlockNumberRequest builds an eth_blockNumber request that asks for
// highest-block enforcement.
func enforcingBlockNumberRequest() *common.NormalizedRequest {
	nq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	nq.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: true})
	return nq
}

func TestNetworkPostForward_BlockNumber_ChargesTheLagToTheUpstreamThatServedIt(t *testing.T) {
	ctx := context.Background()
	const project = "bn-stale-attributed"

	n := new(mockNetwork)
	n.On("ProjectId").Return(project).Maybe()
	n.On("Id").Return("evm:1").Maybe()
	n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()
	n.On("EvmHighestLatestBlockNumber", mock.Anything).Return(int64(110)).Maybe()

	ups := staleBlockNumberUpstream("lagging-rpc", "vendorx")
	counter := telemetry.MetricUpstreamStaleLatestBlock.
		WithLabelValues(project, "vendorx", "evm:1", "lagging-rpc", "eth_blockNumber")
	before := promUtil.ToFloat64(counter)

	out, err := networkPostForward_eth_blockNumber(
		ctx, n, enforcingBlockNumberRequest(), blockNumberResponse(100, ups), nil)
	require.NoError(t, err)
	require.NotNil(t, out)

	jrr, err := out.JsonRpcResponse()
	require.NoError(t, err)
	assert.JSONEq(t, `"0x6e"`, string(jrr.GetResultBytes()),
		"the client must get the tip, not the lagging upstream's answer")

	assert.Equal(t, before+1, promUtil.ToFloat64(counter),
		"the lag must be charged to the upstream that served it")
	require.NotNil(t, out.Upstream(),
		"the corrected response must keep the upstream attribution")
	assert.Equal(t, "lagging-rpc", out.Upstream().Id())
}

// A cache hit carries no upstream. The hook must still correct the answer, and
// it must not invent an attribution for a response no upstream produced.
func TestNetworkPostForward_BlockNumber_CorrectsAnUnattributedAnswer(t *testing.T) {
	ctx := context.Background()
	const project = "bn-stale-unattributed"

	n := new(mockNetwork)
	n.On("ProjectId").Return(project).Maybe()
	n.On("Id").Return("evm:1").Maybe()
	n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()
	n.On("EvmHighestLatestBlockNumber", mock.Anything).Return(int64(110)).Maybe()

	out, err := networkPostForward_eth_blockNumber(
		ctx, n, enforcingBlockNumberRequest(), blockNumberResponse(100, nil), nil)
	require.NoError(t, err)
	require.NotNil(t, out)

	jrr, err := out.JsonRpcResponse()
	require.NoError(t, err)
	assert.JSONEq(t, `"0x6e"`, string(jrr.GetResultBytes()))
	assert.Nil(t, out.Upstream(), "an unattributed answer must stay unattributed")
}

// Without the directive the hook is a pass-through, even when the answer lags
// the tip. Enforcement is opt-in and must stay that way.
func TestNetworkPostForward_BlockNumber_LeavesTheAnswerAloneWithoutTheDirective(t *testing.T) {
	ctx := context.Background()
	const project = "bn-stale-nodirective"

	n := new(mockNetwork)
	n.On("ProjectId").Return(project).Maybe()
	n.On("Id").Return("evm:1").Maybe()
	n.On("Config").Return(&common.NetworkConfig{Evm: &common.EvmNetworkConfig{}}).Maybe()
	n.On("EvmHighestLatestBlockNumber", mock.Anything).Return(int64(110)).Maybe()

	ups := staleBlockNumberUpstream("lagging-rpc", "vendory")
	counter := telemetry.MetricUpstreamStaleLatestBlock.
		WithLabelValues(project, "vendory", "evm:1", "lagging-rpc", "eth_blockNumber")
	before := promUtil.ToFloat64(counter)

	nq := common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`))
	// Directives are present and say no. That is the case that separates
	// "enforcement is off" from "the request carries no directives at all".
	nq.SetDirectives(&common.RequestDirectives{EnforceHighestBlock: false})
	nr := blockNumberResponse(100, ups)

	out, err := networkPostForward_eth_blockNumber(ctx, n, nq, nr, nil)
	require.NoError(t, err)
	assert.Same(t, nr, out, "without the directive the original response must survive")
	assert.Equal(t, before, promUtil.ToFloat64(counter),
		"a pass-through must not report anyone as stale")
}
