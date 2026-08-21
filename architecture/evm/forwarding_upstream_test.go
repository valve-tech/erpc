package evm

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// forwardingUpstream is a common.EvmUpstream double that really ANSWERS
// Forward. The other doubles in this package return (nil, nil) or a canned
// error, so every code path that reads a response body stays dark. This one
// routes each request to a per-method handler the test registers, which is
// what the state-poller probes and the per-method hooks need.
//
// How to reuse it:
//
//	up := newForwardingUpstream(123)
//	up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
//	    return jsonResult(req, `{"number":"0x10","hash":"0xabc"}`)
//	})
//	p := newForwardingPoller(t, up)
//
// Anything NOT registered goes to the fallback, which by default returns a
// typed ErrEndpointUnsupported rather than (nil, nil). That is deliberate: a
// nil/nil pair makes a caller nil-deref or silently succeed, and a test that
// forgets to script a method should fail loudly instead of passing for the
// wrong reason.
//
// Every call is recorded (method + the raw request body), so a test can assert
// on the REQUEST the production code built — the discriminating property for
// most probe behaviour, since several probes return the same (false, false,
// nil) triple by different routes.
type forwardingUpstream struct {
	id        string
	networkId string
	cfg       *common.UpstreamConfig
	logger    zerolog.Logger

	mu       sync.Mutex
	handlers map[string]forwardHandler
	fallback forwardHandler
	calls    []forwardedCall
	cordons  []string

	chainId  string
	chainErr error

	latestBlock    int64
	finalizedBlock int64
	syncingState   common.EvmSyncingState
	statePoller    common.EvmStatePoller
	lowerBound     int64
	upperBound     int64
}

type forwardHandler func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)

// forwardedCall is one observed Forward invocation.
type forwardedCall struct {
	method string
	body   string
}

func newForwardingUpstream(chainId int64) *forwardingUpstream {
	return &forwardingUpstream{
		id:        "fwd-ups",
		networkId: "evm:123",
		cfg: &common.UpstreamConfig{
			Id:   "fwd-ups",
			Type: common.UpstreamTypeEvm,
			Evm:  &common.EvmUpstreamConfig{ChainId: chainId},
		},
		logger:       zerolog.Nop(),
		handlers:     map[string]forwardHandler{},
		chainId:      fmt.Sprintf("%d", chainId),
		syncingState: common.EvmSyncingStateUnknown,
		lowerBound:   math.MinInt64,
		upperBound:   math.MaxInt64,
	}
}

// on registers the handler for one JSON-RPC method.
func (u *forwardingUpstream) on(method string, h forwardHandler) *forwardingUpstream {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.handlers[method] = h
	return u
}

// onFallback replaces the unregistered-method handler.
func (u *forwardingUpstream) onFallback(h forwardHandler) *forwardingUpstream {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.fallback = h
	return u
}

// methodCalls returns the bodies of every request seen for one method, oldest
// first.
func (u *forwardingUpstream) methodCalls(method string) []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var out []string
	for _, c := range u.calls {
		if c.method == method {
			out = append(out, c.body)
		}
	}
	return out
}

// callCount counts Forward invocations for one method.
func (u *forwardingUpstream) callCount(method string) int {
	return len(u.methodCalls(method))
}

// allCalls returns the method name of every Forward invocation, in order.
func (u *forwardingUpstream) allCalls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, 0, len(u.calls))
	for _, c := range u.calls {
		out = append(out, c.method)
	}
	return out
}

func (u *forwardingUpstream) cordonReasons() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.cordons...)
}

func (u *forwardingUpstream) setChainId(detected string, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.chainId = detected
	u.chainErr = err
}

// --- common.Upstream ---

func (u *forwardingUpstream) Id() string                     { return u.id }
func (u *forwardingUpstream) VendorName() string             { return "test" }
func (u *forwardingUpstream) NetworkId() string              { return u.networkId }
func (u *forwardingUpstream) NetworkLabel() string           { return u.networkId }
func (u *forwardingUpstream) Config() *common.UpstreamConfig { return u.cfg }
func (u *forwardingUpstream) Logger() *zerolog.Logger        { return &u.logger }
func (u *forwardingUpstream) Vendor() common.Vendor          { return nil }
func (u *forwardingUpstream) Tracker() common.HealthTracker  { return nil }
func (u *forwardingUpstream) IgnoreMethod(string)            {}
func (u *forwardingUpstream) Uncordon(_, _ string)           {}
func (u *forwardingUpstream) ShouldHandleMethod(string) (bool, error) {
	return true, nil
}

func (u *forwardingUpstream) Cordon(_, reason string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.cordons = append(u.cordons, reason)
}

func (u *forwardingUpstream) Forward(ctx context.Context, req *common.NormalizedRequest, _, _ bool) (*common.NormalizedResponse, error) {
	method, err := req.Method()
	if err != nil {
		return nil, err
	}

	u.mu.Lock()
	u.calls = append(u.calls, forwardedCall{method: method, body: requestWire(req)})
	h, ok := u.handlers[method]
	if !ok {
		h = u.fallback
	}
	u.mu.Unlock()

	if h == nil {
		return nil, common.NewErrEndpointUnsupported(
			fmt.Errorf("forwardingUpstream: no handler registered for %q", method),
		)
	}
	return h(ctx, req)
}

// --- common.EvmUpstream ---

func (u *forwardingUpstream) EvmGetChainId(context.Context) (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.chainId, u.chainErr
}
func (u *forwardingUpstream) EvmIsBlockFinalized(context.Context, int64, bool) (bool, error) {
	return false, nil
}
func (u *forwardingUpstream) EvmAssertBlockAvailability(context.Context, string, common.AvailbilityConfidence, bool, int64) (bool, error) {
	return true, nil
}
func (u *forwardingUpstream) EvmSyncingState() common.EvmSyncingState { return u.syncingState }
func (u *forwardingUpstream) EvmStatePoller() common.EvmStatePoller   { return u.statePoller }
func (u *forwardingUpstream) EvmEffectiveLatestBlock() int64          { return u.latestBlock }
func (u *forwardingUpstream) EvmEffectiveFinalizedBlock() int64       { return u.finalizedBlock }
func (u *forwardingUpstream) EvmBlockAvailabilityBounds() (int64, int64) {
	return u.lowerBound, u.upperBound
}

var _ common.EvmUpstream = (*forwardingUpstream)(nil)

// forwardingNetwork is a common.Network / common.EvmNetwork double that really
// answers Forward, so the network-level hooks (which re-forward a corrected
// request) can be driven end to end. The package's other network doubles
// return (nil, nil) from Forward, which leaves the whole re-forward branch
// dark.
//
// Set highestLatest / highestFinalized to the tips the hook should compare
// against, and register a handler with `on` exactly as for forwardingUpstream.
type forwardingNetwork struct {
	forwardingUpstream // reuses the handler table, the call log and Forward routing

	cfg              *common.NetworkConfig
	highestLatest    int64
	highestFinalized int64
	leader           common.Upstream
	finality         common.DataFinalityState
}

func newForwardingNetwork(chainId int64) *forwardingNetwork {
	n := &forwardingNetwork{
		forwardingUpstream: *newForwardingUpstream(chainId),
		cfg: &common.NetworkConfig{
			Architecture: common.ArchitectureEvm,
			Evm:          &common.EvmNetworkConfig{ChainId: chainId},
		},
	}
	return n
}

func (n *forwardingNetwork) Id() string                    { return n.networkId }
func (n *forwardingNetwork) Label() string                 { return n.networkId }
func (n *forwardingNetwork) ProjectId() string             { return "test-project" }
func (n *forwardingNetwork) Config() *common.NetworkConfig { return n.cfg }
func (n *forwardingNetwork) Architecture() common.NetworkArchitecture {
	return common.ArchitectureEvm
}
func (n *forwardingNetwork) GetMethodMetrics(string) common.TrackedMetrics { return nil }
func (n *forwardingNetwork) GetFinality(context.Context, *common.NormalizedRequest, *common.NormalizedResponse) common.DataFinalityState {
	return n.finality
}
func (n *forwardingNetwork) Bootstrap(context.Context) error { return nil }
func (n *forwardingNetwork) EvmHighestLatestBlockNumber(context.Context) int64 {
	return n.highestLatest
}
func (n *forwardingNetwork) EvmHighestFinalizedBlockNumber(context.Context) int64 {
	return n.highestFinalized
}
func (n *forwardingNetwork) EvmLeaderUpstream(context.Context) common.Upstream { return n.leader }

// Forward drops the two upstream-only flags and reuses the shared router.
func (n *forwardingNetwork) Forward(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	return n.forwardingUpstream.Forward(ctx, req, true, false)
}

var _ common.EvmNetwork = (*forwardingNetwork)(nil)

// --- response builders ---

// jsonResult wraps a raw JSON result payload in a NormalizedResponse.
func jsonResult(req *common.NormalizedRequest, raw string) (*common.NormalizedResponse, error) {
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte(raw), nil)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

// jsonRpcError wraps a JSON-RPC error object in a NormalizedResponse. The
// caller gets a 200-OK-with-error shape, which is the one the probes branch on
// (code -32601 means "unsupported", anything else means "not available").
func jsonRpcError(req *common.NormalizedRequest, code int, msg string) (*common.NormalizedResponse, error) {
	jrr, err := common.NewJsonRpcResponse(1, nil, common.NewErrJsonRpcExceptionExternal(code, msg, ""))
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

// jsonResultBesideError builds the malformed-but-real shape where a 200-OK
// carries BOTH a result and an error member. Some vendors and load balancers
// emit it. It is the only way to tell "the reader honoured the error member"
// apart from "the result happened to be empty".
func jsonResultBesideError(req *common.NormalizedRequest, raw string, code int, msg string) (*common.NormalizedResponse, error) {
	errBytes := fmt.Sprintf(`{"code":%d,"message":%q}`, code, msg)
	jrr, err := common.NewJsonRpcResponseFromBytes(nil, []byte(raw), []byte(errBytes))
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithRequest(req).WithJsonRpcResponse(jrr), nil
}

// --- request helpers ---

// requestWire renders a request the way it would go on the wire. Requests
// parsed from bytes already carry a body; requests the hooks BUILD (the
// re-forwards, the internal probes) carry only a JsonRpcRequest, so marshal
// that instead. Recording an empty string for the built ones would make every
// "the hook sent the right request" assertion vacuous.
func requestWire(req *common.NormalizedRequest) string {
	if b := req.Body(); len(b) > 0 {
		return string(b)
	}
	jrq, err := req.JsonRpcRequest()
	if err != nil || jrq == nil {
		return ""
	}
	out, err := common.SonicCfg.Marshal(jrq)
	if err != nil {
		return ""
	}
	return string(out)
}

// requestedBlockNumber reads params[0] of an eth_* request as a block number.
// Returns ok=false when params[0] is a tag ("latest") rather than a hex number.
func requestedBlockNumber(t *testing.T, req *common.NormalizedRequest) (int64, bool) {
	t.Helper()
	jrq, err := req.JsonRpcRequest()
	if err != nil || jrq == nil || len(jrq.Params) == 0 {
		return 0, false
	}
	s, ok := jrq.Params[0].(string)
	if !ok || !strings.HasPrefix(s, "0x") {
		return 0, false
	}
	n, err := common.HexToInt64(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// blockHeader renders the minimal eth_getBlockByNumber result the poller and
// the probes read: number, hash, timestamp.
func blockHeader(block int64) string {
	return fmt.Sprintf(`{"number":"0x%x","hash":"0x%064x","timestamp":"0x%x"}`,
		block, block, 1_700_000_000+block)
}
