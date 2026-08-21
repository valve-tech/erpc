package evm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
)

// The five hooks are the only door between the generic router and EVM-specific
// behaviour. Every request in the system passes through all of them. Two
// properties matter more than any individual hook: an unknown method must pass
// through untouched, and an error must arrive at the far side still being the
// error that was raised.

func hookCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func hookRequest(t *testing.T, method string) *common.NormalizedRequest {
	t.Helper()
	return common.NewNormalizedRequestFromJsonRpcRequest(
		common.NewJsonRpcRequest(method, []interface{}{}))
}

func TestHooks_PassAnUnknownMethodStraightThrough(t *testing.T) {
	t.Parallel()

	// The unknown-method path is the primary one: every method eRPC has never
	// heard of, on every chain it has never seen, arrives here. A hook that
	// claimed to handle one would short-circuit a request the upstream could
	// have answered.
	ctx := hookCtx(t)
	req := hookRequest(t, "some_brandNewMethodNobodyHasSeen")

	if handled, resp, err := HandleProjectPreForward(ctx, nil, req); handled || resp != nil || err != nil {
		t.Fatalf("HandleProjectPreForward = (%v, %v, %v), want a clean pass-through", handled, resp, err)
	}
	if handled, resp, err := HandleNetworkPreForward(ctx, nil, nil, req); handled || resp != nil || err != nil {
		t.Fatalf("HandleNetworkPreForward = (%v, %v, %v), want a clean pass-through", handled, resp, err)
	}
	if handled, resp, err := HandleUpstreamPreForward(ctx, nil, nil, req, false); handled || resp != nil || err != nil {
		t.Fatalf("HandleUpstreamPreForward = (%v, %v, %v), want a clean pass-through", handled, resp, err)
	}
}

func TestHooks_ReturnTheUpstreamsAnswerUnchangedForAnUnknownMethod(t *testing.T) {
	t.Parallel()

	// The post-forward hooks must hand back the SAME response object, not a copy
	// and not a rebuilt one. A rebuilt response loses the upstream attribution
	// the cache and the tracker read afterwards.
	ctx := hookCtx(t)
	req := hookRequest(t, "some_brandNewMethodNobodyHasSeen")
	resp := common.NewNormalizedResponse().WithRequest(req)

	got, err := HandleNetworkPostForward(ctx, nil, req, resp, nil)
	if err != nil {
		t.Fatalf("HandleNetworkPostForward returned an error for a clean unknown-method reply: %v", err)
	}
	if got != resp {
		t.Fatal("HandleNetworkPostForward replaced the response object; upstream attribution would be lost")
	}
}

func TestHooks_KeepTheOriginalCauseReachableThroughPostForward(t *testing.T) {
	t.Parallel()

	// This codebase's recurring failure is a layer returning its own tidy error.
	// Retry, failover and circuit-breaking all read the CAUSE, so an error that
	// arrives wrapped past recognition silently disables every one of them.
	ctx := hookCtx(t)
	sentinel := errors.New("the upstream refused the connection")
	upstreamErr := common.NewErrEndpointServerSideException(sentinel, nil, 500)
	// A real network: the specialised hooks log through it, so nil would only
	// prove the default arm works.
	net := &tagTestNetwork{
		cfg:           &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}},
		highestLatest: 0x1000,
	}

	for _, method := range []string{
		"some_brandNewMethodNobodyHasSeen",
		"eth_blockNumber",
		"eth_getBlockByNumber",
		"eth_getLogs",
		"eth_sendRawTransaction",
	} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			req := hookRequest(t, method)
			_, err := HandleNetworkPostForward(ctx, net, req, nil, upstreamErr)
			if err == nil {
				t.Fatal("the hook swallowed the upstream error entirely")
			}
			if !errors.Is(err, sentinel) {
				t.Fatalf("the original cause is no longer reachable: %T %v", err, err)
			}
		})
	}
}

func TestHooks_SurfaceAMalformedRequestInsteadOfGuessingAMethod(t *testing.T) {
	t.Parallel()

	// A body eRPC cannot read a method out of must fail loudly. Treating it as
	// "no method matched" would forward garbage to an upstream and bill the
	// operator for the round trip.
	ctx := hookCtx(t)
	bad := common.NewNormalizedRequest([]byte(`not json at all`))

	if _, _, err := HandleProjectPreForward(ctx, nil, bad); err == nil {
		t.Fatal("HandleProjectPreForward accepted an unreadable request")
	}
	if _, _, err := HandleNetworkPreForward(ctx, nil, nil, bad); err == nil {
		t.Fatal("HandleNetworkPreForward accepted an unreadable request")
	}
	if _, _, err := HandleUpstreamPreForward(ctx, nil, nil, bad, false); err == nil {
		t.Fatal("HandleUpstreamPreForward accepted an unreadable request")
	}
	if _, err := HandleNetworkPostForward(ctx, nil, bad, nil, nil); err == nil {
		t.Fatal("HandleNetworkPostForward accepted an unreadable request")
	}
}

func TestHooks_LeaveAStateQueryAloneWhileNoProberIsRunning(t *testing.T) {
	t.Parallel()

	// The state-proven boundary is defined by a CLASS of methods, not a list, so
	// eth_getBalance falls into the default arm of the dispatch and is then
	// tested against the class. With no prober running for the network the
	// boundary must be completely inert — the feature must cost nothing when it
	// is off.
	ctx := hookCtx(t)
	net := &tagTestNetwork{highestLatest: 0x1000}
	req := hookRequest(t, "eth_getBalance")

	handled, resp, err := HandleUpstreamPreForward(ctx, net, nil, req, false)
	if handled || resp != nil || err != nil {
		t.Fatalf("HandleUpstreamPreForward = (%v, %v, %v); the boundary must be inert with no prober", handled, resp, err)
	}
}

func TestArchitectureHandler_IsRegisteredAndDelegatesToTheSameCode(t *testing.T) {
	t.Parallel()

	// The router reaches EVM only through this registry entry. If the
	// registration were missing, or a wrapper delegated to the wrong function,
	// every EVM-specific behaviour would vanish silently: requests would still
	// be answered, just without any of the hooks.
	h, err := common.GetArchitectureHandler(common.ArchitectureEvm)
	if err != nil {
		t.Fatalf("no handler registered for the evm architecture: %v", err)
	}

	ctx := hookCtx(t)
	req := hookRequest(t, "some_brandNewMethodNobodyHasSeen")

	if handled, resp, err := h.HandleProjectPreForward(ctx, nil, req); handled || resp != nil || err != nil {
		t.Fatalf("registered HandleProjectPreForward = (%v, %v, %v)", handled, resp, err)
	}
	if handled, resp, err := h.HandleNetworkPreForward(ctx, nil, nil, req); handled || resp != nil || err != nil {
		t.Fatalf("registered HandleNetworkPreForward = (%v, %v, %v)", handled, resp, err)
	}
	if handled, resp, err := h.HandleUpstreamPreForward(ctx, nil, nil, req, false); handled || resp != nil || err != nil {
		t.Fatalf("registered HandleUpstreamPreForward = (%v, %v, %v)", handled, resp, err)
	}

	// Each wrapper must reach the function that matches its own name. A
	// cross-wired pair is invisible for an unknown method, so use a request the
	// pre-forward hooks would treat differently and check the error survives.
	sentinel := errors.New("upstream refused")
	if _, err := h.HandleNetworkPostForward(ctx, nil, req, nil,
		common.NewErrEndpointServerSideException(sentinel, nil, 500)); !errors.Is(err, sentinel) {
		t.Fatalf("registered HandleNetworkPostForward lost the cause: %T %v", err, err)
	}

	bad := common.NewNormalizedRequest([]byte(`not json at all`))
	if _, _, err := h.HandleProjectPreForward(ctx, nil, bad); err == nil {
		t.Fatal("registered HandleProjectPreForward accepted an unreadable request")
	}

	if h.NewJsonRpcErrorExtractor() == nil {
		t.Fatal("the registered handler offers no json-rpc error extractor; every upstream error would stay unclassified")
	}
}
