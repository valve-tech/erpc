package common

import (
	"context"
	"testing"
)

// emptyishRotationResponse builds a NormalizedResponse whose JSON-RPC result
// is empty — what IsResultEmptyish classifies as an emptyish result (e.g.
// what a zero balanceOf, a zero allowance, or "no logs matched" looks like
// on the wire). Mirrors createEmptyNormalizedResponse in request_test.go.
func emptyishRotationResponse(t *testing.T) *NormalizedResponse {
	t.Helper()
	jrr, err := NewJsonRpcResponse(1, nil, nil)
	if err != nil {
		t.Fatalf("building response: %v", err)
	}
	return NewNormalizedResponse().WithJsonRpcResponse(jrr)
}

func TestMarkUpstreamCompleted_AcceptedEmptyDoesNotFreeUpstream(t *testing.T) {
	ctx := context.Background()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_call"}`))
	up := newMockUpstream("up-1")
	req.SetUpstreams([]Upstream{up})

	selected, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("NextUpstream should succeed: %v", err)
	}

	req.MarkUpstreamCompleted(ctx, selected, emptyishRotationResponse(t), nil)

	if _, stillConsumed := req.ConsumedUpstreams.Load(selected); !stillConsumed {
		t.Error("an accepted empty (eth_call) must leave the upstream consumed, not free it for rotation")
	}
	if _, hasErr := req.ErrorsByUpstream.Load(selected); hasErr {
		t.Error("an accepted empty (eth_call) must not be recorded as a missing-data error")
	}
}

func TestMarkUpstreamCompleted_PointLookupEmptyStillRotates(t *testing.T) {
	ctx := context.Background()
	req := NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber"}`))
	up := newMockUpstream("up-1")
	req.SetUpstreams([]Upstream{up})

	selected, err := req.NextUpstream()
	if err != nil {
		t.Fatalf("NextUpstream should succeed: %v", err)
	}

	req.MarkUpstreamCompleted(ctx, selected, emptyishRotationResponse(t), nil)

	if _, stillConsumed := req.ConsumedUpstreams.Load(selected); stillConsumed {
		t.Error("a point-lookup empty (eth_getBlockByNumber) must free the upstream so another can be tried")
	}
	if _, hasErr := req.ErrorsByUpstream.Load(selected); !hasErr {
		t.Error("a point-lookup empty (eth_getBlockByNumber) must be recorded as missing-data")
	}
}
