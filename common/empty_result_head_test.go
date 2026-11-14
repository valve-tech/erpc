package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authoritativeFake builds an upstream that lists eth_getLogs in
// emptyResultAuthoritativeMethods, with a poller at latest/finalized
// (nil poller when latest < 0).
func authoritativeFake(latest, finalized int64, avail *EvmBlockAvailabilityConfig) Upstream {
	var opts []func(*FakeUpstream)
	if latest >= 0 {
		opts = append(opts, WithEvmStatePoller(NewFakeEvmStatePoller(latest, finalized)))
	}
	u := NewFakeUpstream("own-node", opts...)
	u.Config().Evm = &EvmUpstreamConfig{
		EmptyResultAuthoritativeMethods: []string{"eth_getLogs"},
		BlockAvailability:               avail,
	}
	return u
}

// emptyLogsAt builds a request whose resolved block is bn (0 leaves it
// unresolved) and a [] response to it.
func emptyLogsAt(t *testing.T, bn int64, confidence AvailbilityConfidence) (*NormalizedRequest, *NormalizedResponse) {
	t.Helper()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{}]}`))
	if bn > 0 {
		req.SetEvmBlockNumber(bn)
	}
	req.SetNetwork(&covMiscNetwork{id: "evm:1", cfg: &NetworkConfig{
		Evm: &EvmNetworkConfig{EmptyResultConfidence: confidence},
	}})
	jrr, err := NewJsonRpcResponse(1, []interface{}{}, nil)
	require.NoError(t, err)
	return req, NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
}

func TestUpstreamEmptyIsAuthoritative_HeadCheck(t *testing.T) {
	cases := []struct {
		name       string
		latest     int64 // -1: no poller
		finalized  int64
		avail      *EvmBlockAvailabilityConfig
		block      int64
		confidence AvailbilityConfidence
		want       bool
	}{
		{name: "below head", latest: 100, finalized: 90, block: 99, want: true},
		{name: "at head", latest: 100, finalized: 90, block: 100, want: true},
		{name: "above head (the tip case)", latest: 100, finalized: 90, block: 101, want: false},
		{name: "head unknown (zero)", latest: 0, block: 1, want: false},
		{name: "no state poller", latest: -1, block: 1, want: false},
		{name: "block unresolved", latest: 100, finalized: 90, block: 0, want: false},
		{name: "finalized confidence, at finalized", latest: 100, finalized: 90, block: 90,
			confidence: AvailbilityConfidenceFinalized, want: true},
		{name: "finalized confidence, above finalized", latest: 100, finalized: 90, block: 95,
			confidence: AvailbilityConfidenceFinalized, want: false},
		{name: "upper bound lag margin, inside", latest: 100, finalized: 90, block: 95,
			avail: &EvmBlockAvailabilityConfig{Upper: &EvmAvailabilityBoundConfig{LatestBlockMinus: ptrInt64(5)}}, want: true},
		{name: "upper bound lag margin, outside", latest: 100, finalized: 90, block: 96,
			avail: &EvmBlockAvailabilityConfig{Upper: &EvmAvailabilityBoundConfig{LatestBlockMinus: ptrInt64(5)}}, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := authoritativeFake(c.latest, c.finalized, c.avail)
			req, resp := emptyLogsAt(t, c.block, c.confidence)
			assert.Equal(t, c.want, UpstreamEmptyIsAuthoritative(u, "eth_getLogs", req, resp))
			// A nil req falls back to the response's request.
			assert.Equal(t, c.want, UpstreamEmptyIsAuthoritative(u, "eth_getLogs", nil, resp))
		})
	}
}

// null is never authoritative, even for a block well below the head.
func TestUpstreamEmptyIsAuthoritative_NullNeverFinal(t *testing.T) {
	u := authoritativeFake(100, 90, nil)
	req, _ := emptyLogsAt(t, 50, AvailbilityConfidenceBlockHead)
	jrr, err := NewJsonRpcResponse(1, nil, nil)
	require.NoError(t, err)
	resp := NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr)
	assert.False(t, UpstreamEmptyIsAuthoritative(u, "eth_getLogs", req, resp))
}

// An upstream that does not list the method is never authoritative.
func TestUpstreamEmptyIsAuthoritative_UnlistedMethod(t *testing.T) {
	u := authoritativeFake(100, 90, nil)
	req, resp := emptyLogsAt(t, 50, AvailbilityConfidenceBlockHead)
	assert.False(t, UpstreamEmptyIsAuthoritative(u, "eth_getBalance", req, resp))
}

func ptrInt64(v int64) *int64 { return &v }

func TestAcceptedEmptyIsFinal_HeadCheck(t *testing.T) {
	cases := []struct {
		name   string
		latest int64 // -1: no poller
		block  int64
		want   bool
	}{
		{name: "at head", latest: 100, block: 100, want: true},
		{name: "above head (the tip case)", latest: 100, block: 101, want: false},
		{name: "head unknown", latest: 0, block: 50, want: false},
		{name: "no state poller", latest: -1, block: 50, want: false},
		// No resolved block: the check cannot run, so today's final answer stays.
		{name: "block unresolved", latest: 100, block: 0, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := authoritativeFake(c.latest, 0, nil)
			req, _ := emptyLogsAt(t, c.block, AvailbilityConfidenceBlockHead)
			assert.Equal(t, c.want, AcceptedEmptyIsFinal(u, "eth_getLogs", req))
		})
	}
}

// A non-EVM upstream (Solana's getFirstAvailableBlock, say) keeps today's
// accept-list behaviour.
func TestAcceptedEmptyIsFinal_NonEvmUnaffected(t *testing.T) {
	req, _ := emptyLogsAt(t, 50, AvailbilityConfidenceBlockHead)
	assert.True(t, AcceptedEmptyIsFinal(nil, "eth_getLogs", req))
}

// Only methods in evm.emptyResultHeadCheckMethods are checked. A value read,
// or trace_block on the authoritative path, keeps today's final answer even
// above the upstream's head.
func TestEmptyResultHeadCheck_OnlyListedMethods(t *testing.T) {
	u := NewFakeUpstream("own-node", WithEvmStatePoller(NewFakeEvmStatePoller(100, 90)))
	u.Config().Evm = &EvmUpstreamConfig{EmptyResultAuthoritativeMethods: []string{"trace_block", "eth_getLogs"}}
	req, resp := emptyLogsAt(t, 101, AvailbilityConfidenceBlockHead)

	assert.True(t, AcceptedEmptyIsFinal(u, "eth_call", req))
	assert.True(t, AcceptedEmptyIsFinal(u, "eth_getBalance", req))
	assert.True(t, UpstreamEmptyIsAuthoritative(u, "trace_block", req, resp))
	assert.False(t, AcceptedEmptyIsFinal(u, "eth_getLogs", req))
	assert.False(t, AcceptedEmptyIsFinal(u, "trace_filter", req))
	assert.False(t, UpstreamEmptyIsAuthoritative(u, "eth_getLogs", req, resp))
}

// An explicit empty list turns the check off; a configured list replaces the
// default.
func TestEmptyResultHeadCheck_ConfiguredList(t *testing.T) {
	u := authoritativeFake(100, 90, nil)
	req, _ := emptyLogsAt(t, 101, AvailbilityConfidenceBlockHead)

	req.Network().Config().Evm.EmptyResultHeadCheckMethods = []string{}
	assert.True(t, AcceptedEmptyIsFinal(u, "eth_getLogs", req))

	req.Network().Config().Evm.EmptyResultHeadCheckMethods = []string{"eth_call"}
	assert.True(t, AcceptedEmptyIsFinal(u, "eth_getLogs", req))
	assert.False(t, AcceptedEmptyIsFinal(u, "eth_call", req))
}
