package erpc

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
)

// Passthrough subscriptions: every `<ns>_subscribe` that is not eth's.
//
// eRPC's subscription surface was eth-only and indexer-backed, so a
// `msgboard_subscribe` failed IsSubscriptionMethod, fell through to
// project.Forward, and went upstream as an ordinary one-shot request. reth
// really did create a subscription on whichever socket that forward used and
// returned a real id, which eRPC handed back verbatim — but nothing ever
// registered a handler for it. The id was live upstream and orphaned here, so
// the client held a healthy-looking subscription that could never deliver.
//
// This path is deliberately NOT the eth path. eth_subscribe keeps its indexer
// backing, which gives it filter dedup and fan-out across upstreams; a
// passthrough subscription must not inherit either, because it is bound to one
// upstream socket and nothing here knows how to merge two of them.
//
// WHAT HAPPENS WHEN THE UPSTREAM DROPS: this connection is closed, and the
// client resubscribes. That is the choice the brief asked to be written down.
// The alternative — resubscribe underneath and remap ids — is machinery the
// valve relay in front of eRPC already implements and proves
// (packages/relay/src/ws-sub-registry.ts), so building a second copy here
// would be an unforced commitment. A close is also the loud option: a
// subscription that silently stops is the exact defect this file exists to
// remove.

// PassthroughCloseCode tells a client that an upstream subscription socket
// dropped and every passthrough subscription on this connection is gone.
// Distinct from the ordinary codes so a client can tell "resubscribe" from
// "go away".
const PassthroughCloseCode = 4009

// IsGenericSubscribeMethod reports a `<ns>_subscribe` that is not eth's.
//
// The suffix is the whole test. eRPC cannot hold a list of namespaces: reth
// serves msgboard today and the set is open-ended, which is exactly the kind
// of commitment this repo's design razor tells us not to make. A method that
// ends in _subscribe and is not eth_subscribe is either a real subscription on
// some namespace, or a method the upstream will reject on its own — and the
// upstream's rejection is the right answer either way.
func IsGenericSubscribeMethod(method string) bool {
	return method != MethodEthSubscribe && strings.HasSuffix(method, "_subscribe")
}

// IsGenericUnsubscribeMethod is the matching teardown half.
func IsGenericUnsubscribeMethod(method string) bool {
	return method != MethodEthUnsubscribe && strings.HasSuffix(method, "_unsubscribe")
}

// IsPassthroughSubscriptionMethod reports either half.
func IsPassthroughSubscriptionMethod(method string) bool {
	return IsGenericSubscribeMethod(method) || IsGenericUnsubscribeMethod(method)
}

// passthroughSub is one live subscription, pinned to the upstream socket that
// created it. The subscription id is meaningful only on that socket, so the
// binding is the point: a passthrough subscription cannot ride the failover
// chain the way a one-shot request does.
type passthroughSub struct {
	subID      string
	upstreamID string
	wsClient   *clients.WsJsonRpcClient
	upstream   *upstream.Upstream
}

// passthroughSubscriptions holds one client connection's passthrough
// subscriptions. Keyed by the upstream's own subscription id, which eRPC
// passes through to the client unchanged — there is no second id space to keep
// in step, and nothing here has to translate.
type passthroughSubscriptions struct {
	mu   sync.Mutex
	subs map[string]*passthroughSub
}

func newPassthroughSubscriptions() *passthroughSubscriptions {
	return &passthroughSubscriptions{subs: make(map[string]*passthroughSub)}
}

func (p *passthroughSubscriptions) add(sub *passthroughSub) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.subs[sub.subID] = sub
}

func (p *passthroughSubscriptions) take(subID string) (*passthroughSub, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	sub, ok := p.subs[subID]
	if ok {
		delete(p.subs, subID)
	}
	return sub, ok
}

// drain empties the registry and returns what it held, so a caller can tear
// every subscription down exactly once.
func (p *passthroughSubscriptions) drain() []*passthroughSub {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*passthroughSub, 0, len(p.subs))
	for _, sub := range p.subs {
		out = append(out, sub)
	}
	p.subs = make(map[string]*passthroughSub)
	return out
}

func (p *passthroughSubscriptions) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.subs)
}

// handlePassthroughSubscribe forwards a `<ns>_subscribe` to ONE WS-capable
// upstream, registers a notification handler for the id it returns, and writes
// the id back to the client.
//
// The upstream answers first and eRPC registers second. That order leaves a
// window in which a notification can arrive before the handler exists, and the
// client would never see those first events. The window is the upstream's own
// round-trip and nothing in this process can close it — the id does not exist
// until the upstream mints it. It is recorded here rather than hidden: a
// subscription that misses an event in its first milliseconds is a different
// defect from one that never delivers at all, and only the second one is what
// this path was written to fix.
func (wsc *WsConnection) handlePassthroughSubscribe(
	ctx context.Context,
	nq *common.NormalizedRequest,
	method string,
) error {
	nw, err := wsc.project.GetNetwork(wsc.appCtx, wsc.networkId)
	if err != nil {
		return err
	}

	ups := nw.upstreamsRegistry.GetWsUpstreams(ctx, wsc.networkId)
	if len(ups) == 0 {
		return common.NewErrNoWsUpstreamAvailable(wsc.networkId)
	}

	// Try each WS upstream in turn. A passthrough subscription is pinned to
	// one socket, so this is the ONLY point at which another upstream may be
	// chosen: once an id exists, it means nothing anywhere else.
	var lastErr error
	for _, up := range ups {
		wsClient, ok := up.Client.(*clients.WsJsonRpcClient)
		if !ok || !wsClient.IsConnected() {
			continue
		}

		resp, err := up.Forward(ctx, nq, true, false)
		if err != nil {
			lastErr = err
			continue
		}

		subID, err := passthroughSubIDFromResponse(resp)
		resp.Release()
		if err != nil {
			lastErr = err
			continue
		}

		sub := &passthroughSub{
			subID:      subID,
			upstreamID: up.Id(),
			wsClient:   wsClient,
			upstream:   up,
		}
		wsc.registerPassthrough(sub)

		wsc.logger.Debug().
			Str("connId", wsc.id).
			Str("method", method).
			Str("upstreamId", up.Id()).
			Str("subscriptionId", subID).
			Msg("passthrough subscription established")

		return wsc.writeJSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      nq.ID(),
			"result":  subID,
		})
	}

	if lastErr != nil {
		return lastErr
	}
	return common.NewErrNoWsUpstreamAvailable(wsc.networkId)
}

// registerPassthrough wires the notification handler and the disconnect hook
// for one subscription, then records it.
func (wsc *WsConnection) registerPassthrough(sub *passthroughSub) {
	// Route by id. The handler is keyed on the subscription id alone, so the
	// upstream's notification METHOD NAME never has to be predicted here —
	// which matters, because it varies: reth emitted `msgboard_subscribe` up
	// to v2.5.1-pulse-3 and `msgboard_subscription` from pulse-4 on.
	sub.wsClient.RegisterSubscriptionHandler(sub.subID, func(method string, params []byte) {
		wsc.forwardPassthroughNotification(sub.subID, method, params)
	})

	// The subscription dies with its socket. Close the client connection so it
	// resubscribes, rather than leaving it holding an id that will never
	// deliver again — see the note at the top of this file.
	sub.wsClient.SetOnDisconnect(wsc.passthroughHookID(sub.subID), func() {
		wsc.onPassthroughUpstreamLost(sub.subID)
	})

	wsc.passthroughSubs.add(sub)
}

func (wsc *WsConnection) passthroughHookID(subID string) string {
	return "passthrough:" + wsc.id + ":" + subID
}

// handlePassthroughMethod routes a generic subscribe or unsubscribe and
// reports any failure to the client in the shape it expects.
func (wsc *WsConnection) handlePassthroughMethod(
	ctx context.Context,
	nq *common.NormalizedRequest,
	method string,
	startedAt *time.Time,
) {
	var err error
	if IsGenericSubscribeMethod(method) {
		err = wsc.handlePassthroughSubscribe(ctx, nq, method)
	} else {
		var handled bool
		handled, err = wsc.handlePassthroughUnsubscribe(ctx, nq)
		if !handled && err == nil {
			// Not a subscription this connection holds. Forward it like any
			// other request rather than answering for an upstream: the client
			// may be tearing down something eRPC never tracked.
			resp, ferr := wsc.project.Forward(ctx, wsc.networkId, nq)
			if ferr != nil {
				if resp != nil {
					go resp.Release()
				}
				wsc.writeErrorResponse(nq, ferr, startedAt, wsc.server.serverCfg.IncludeErrorDetails)
				common.EndRequestSpan(ctx, nil, ferr)
				return
			}
			wsc.writeNormalizedResponse(resp)
			common.EndRequestSpan(ctx, resp, nil)
			return
		}
	}

	if err != nil {
		wsc.writeErrorResponse(nq, err, startedAt, wsc.server.serverCfg.IncludeErrorDetails)
		common.EndRequestSpan(ctx, nil, err)
		return
	}
	common.EndRequestSpan(ctx, nil, nil)
}

// forwardPassthroughNotification relays one upstream notification to the
// client, preserving the upstream's own method name.
func (wsc *WsConnection) forwardPassthroughNotification(subID string, method string, params []byte) {
	if wsc.closed.Load() {
		return
	}
	var notifParams struct {
		Subscription string          `json:"subscription"`
		Result       json.RawMessage `json:"result"`
	}
	if err := common.SonicCfg.Unmarshal(params, &notifParams); err != nil {
		wsc.logger.Warn().Err(err).Str("connId", wsc.id).Str("subscriptionId", subID).
			Msg("failed to parse passthrough notification params")
		return
	}

	if err := wsc.WriteSubscriptionNotificationAs(method, subID, notifParams.Result); err != nil {
		wsc.logger.Debug().Err(err).Str("connId", wsc.id).Str("subscriptionId", subID).
			Msg("failed to write passthrough notification to client")
	}
}

// onPassthroughUpstreamLost tears down one subscription and closes the client
// connection. Deliberately not silent: the client is told to resubscribe.
func (wsc *WsConnection) onPassthroughUpstreamLost(subID string) {
	sub, ok := wsc.passthroughSubs.take(subID)
	if !ok {
		return
	}
	sub.wsClient.UnregisterSubscriptionHandler(sub.subID)
	sub.wsClient.RemoveOnDisconnect(wsc.passthroughHookID(sub.subID))

	wsc.logger.Info().
		Str("connId", wsc.id).
		Str("upstreamId", sub.upstreamID).
		Str("subscriptionId", subID).
		Msg("passthrough subscription lost its upstream; closing the client connection so it resubscribes")

	wsc.closeWithCode(PassthroughCloseCode, "upstream subscription connection lost; resubscribe")
}

// handlePassthroughUnsubscribe forwards a `<ns>_unsubscribe` to the upstream
// that owns the subscription and drops the local registration.
//
// An id this connection does not hold is NOT an error here. It is forwarded to
// an upstream like any other request, because the client may be unsubscribing
// something eRPC never tracked, and answering "unknown" ourselves would be a
// claim we cannot support.
func (wsc *WsConnection) handlePassthroughUnsubscribe(
	ctx context.Context,
	nq *common.NormalizedRequest,
) (handled bool, err error) {
	subID, ok := passthroughSubIDFromRequest(nq)
	if !ok {
		return false, nil
	}
	sub, ok := wsc.passthroughSubs.take(subID)
	if !ok {
		return false, nil
	}

	sub.wsClient.UnregisterSubscriptionHandler(sub.subID)
	sub.wsClient.RemoveOnDisconnect(wsc.passthroughHookID(sub.subID))

	// Send to the SAME upstream that holds it. Any other one would answer
	// about a subscription it has never heard of.
	resp, err := sub.upstream.Forward(ctx, nq, true, false)
	if err != nil {
		return true, err
	}
	defer resp.Release()

	wsc.writeNormalizedResponse(resp)
	return true, nil
}

// cleanupPassthroughSubscriptions unsubscribes everything this connection
// holds. Called on close, so subHandlers cannot leak on a long-lived upstream
// socket that outlives many client connections.
func (wsc *WsConnection) cleanupPassthroughSubscriptions() {
	for _, sub := range wsc.passthroughSubs.drain() {
		sub.wsClient.UnregisterSubscriptionHandler(sub.subID)
		sub.wsClient.RemoveOnDisconnect(wsc.passthroughHookID(sub.subID))
	}
}

// passthroughSubIDFromResponse reads the subscription id an upstream returned.
func passthroughSubIDFromResponse(resp *common.NormalizedResponse) (string, error) {
	if resp == nil {
		return "", common.NewErrJsonRpcExceptionInternal(
			0, common.JsonRpcErrorServerSideException,
			"upstream returned no response to a subscribe", nil, nil,
		)
	}
	jrr, err := resp.JsonRpcResponse()
	if err != nil {
		return "", err
	}
	if jrr == nil {
		return "", common.NewErrJsonRpcExceptionInternal(
			0, common.JsonRpcErrorServerSideException,
			"upstream returned an empty response to a subscribe", nil, nil,
		)
	}
	if jrr.Error != nil {
		return "", jrr.Error
	}
	var subID string
	if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &subID); err != nil || subID == "" {
		return "", common.NewErrJsonRpcExceptionInternal(
			0, common.JsonRpcErrorServerSideException,
			"upstream returned no subscription id", err, nil,
		)
	}
	return subID, nil
}

// passthroughSubIDFromRequest reads params[0] of an unsubscribe.
func passthroughSubIDFromRequest(nq *common.NormalizedRequest) (string, bool) {
	jrq, err := nq.JsonRpcRequest()
	if err != nil || jrq == nil {
		return "", false
	}
	if len(jrq.Params) == 0 {
		return "", false
	}
	subID, ok := jrq.Params[0].(string)
	if !ok || subID == "" {
		return "", false
	}
	return subID, true
}
