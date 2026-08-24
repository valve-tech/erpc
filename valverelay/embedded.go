package valverelay

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/erpc"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

// embeddedBackend forwards inside this process. It holds a real *erpc.ERPC and
// calls Network.Forward directly, so the HTTP server, its 590-line request
// closure, its gzip writer and its status-code mapping are all bypassed.
//
// This is the construction internal/simulator/orchestrator.go:269 already uses:
// NewERPC with nil shared state and nil caches, Bootstrap, GetNetwork,
// Network.Bootstrap, Network.Forward. It is copied rather than reinvented
// because that path is the one the fork has already run.
type embeddedBackend struct {
	e         *erpc.ERPC
	projectID string

	mu       sync.Mutex
	networks map[int64]*erpc.Network
}

// NewEmbeddedBackend boots eRPC from cfg and forwards to it in-process.
//
// cfg must already have been through SetDefaults and Validate;
// common.LoadConfig does both. projectID is required rather than derived from
// the config: a config with several projects has no obvious default, and
// guessing one bills the wrong project's traffic without anything going red.
func NewEmbeddedBackend(ctx context.Context, logger *zerolog.Logger, cfg *common.Config, projectID string) (Backend, error) {
	if cfg == nil {
		return nil, fmt.Errorf("valverelay: embedded backend needs a config")
	}
	if projectID == "" {
		return nil, fmt.Errorf("valverelay: embedded backend needs a project id; it is not guessed from the config")
	}
	e, err := erpc.NewERPC(ctx, logger, nil, nil, nil, cfg)
	if err != nil {
		return nil, fmt.Errorf("valverelay: NewERPC: %w", err)
	}
	e.Bootstrap(ctx)
	if _, err := e.GetProject(projectID); err != nil {
		// Fail at boot rather than on the first request. A project id that
		// does not exist would otherwise look like a per-chain routing fault
		// on every single call.
		return nil, fmt.Errorf("valverelay: project %q: %w", projectID, err)
	}
	return &embeddedBackend{e: e, projectID: projectID, networks: map[int64]*erpc.Network{}}, nil
}

// network resolves and bootstraps the network for one chain, once.
//
// Networks are resolved lazily because the chain id arrives with the request.
// eRPC's own registry already caches and bootstraps them; the map here only
// saves the repeated lookup and the repeated Network.Bootstrap. Two goroutines
// racing on a cold chain can both bootstrap it, which is harmless — Bootstrap
// re-registers the network with the policy engine and nothing else.
func (b *embeddedBackend) network(ctx context.Context, chainID int64) (*erpc.Network, error) {
	b.mu.Lock()
	n, ok := b.networks[chainID]
	b.mu.Unlock()
	if ok {
		return n, nil
	}

	netID := util.EvmNetworkId(chainID)
	net, err := b.e.GetNetwork(ctx, b.projectID, netID)
	if err != nil {
		return nil, fmt.Errorf("valverelay: GetNetwork %s: %w", netID, err)
	}
	if err := net.Bootstrap(ctx); err != nil {
		return nil, fmt.Errorf("valverelay: network Bootstrap %s: %w", netID, err)
	}

	b.mu.Lock()
	b.networks[chainID] = net
	b.mu.Unlock()
	return net, nil
}

// Forward runs the request through eRPC and returns the JSON-RPC answer.
//
// An error from Network.Forward means eRPC produced no answer, so the caller
// bills nothing. A response carrying a JSON-RPC error is an answer, and it is
// returned with a nil error so it is billed.
func (b *embeddedBackend) Forward(ctx context.Context, chainID int64, body []byte) ([]byte, error) {
	net, err := b.network(ctx, chainID)
	if err != nil {
		return nil, err
	}

	resp, err := net.Forward(ctx, common.NewNormalizedRequest(body))
	if err != nil {
		// eRPC can hand back both an error and a response (a last valid
		// response it decided not to use). Release it: nobody will read it.
		if resp != nil {
			resp.Release()
		}
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("valverelay: eRPC returned neither a response nor an error for chain %d", chainID)
	}
	defer resp.Release()

	// WriteTo, not MarshalJSON. JsonRpcResponse.MarshalJSON is a hard error by
	// construction ("must not be used"), so marshalling here would turn every
	// successful request into a failure — and, on the billing path, into a
	// request the customer is not charged for. WriteTo is what eRPC's own HTTP
	// server uses to put the same bytes on the wire.
	var buf bytes.Buffer
	if _, err := resp.WriteTo(&buf); err != nil {
		return nil, fmt.Errorf("valverelay: serialising the eRPC response for chain %d: %w", chainID, err)
	}
	if buf.Len() == 0 {
		return nil, fmt.Errorf("valverelay: eRPC produced an empty body for chain %d", chainID)
	}
	return buf.Bytes(), nil
}

// Close is a no-op. The eRPC instance's lifetime is the context passed to
// NewEmbeddedBackend; cancelling it is what shuts eRPC down.
func (b *embeddedBackend) Close() error { return nil }
