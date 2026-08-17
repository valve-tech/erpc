package clients

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// The pool maintainer is the only thing that ever notices a connection has
// gone wrong BETWEEN requests: a conn closed in place by a failed replacement,
// a backend that started answering for another chain, a conn older than its
// max age. None of that ran under test, because the maintainer only wakes once
// a minute and Shutdown did not join it — a test that compressed the interval
// raced the maintainer's read of the package var, and the race detector said
// so. bdsPool now waits for the maintainer inside Shutdown, which both fixes a
// real leak (see TestPoolShutdown_JoinsTheMaintainer) and gives these tests the
// happens-before edge they need to restore the timers.

// compressMaintainTimers shrinks the maintainer's timers for one test and
// restores them on cleanup. Register it BEFORE creating a pool: t.Cleanup runs
// last-in-first-out, so the pool's Shutdown — which joins the maintainer —
// runs first and the restore below cannot race the maintainer's reads.
func compressMaintainTimers(t *testing.T, interval, maxAge, linger time.Duration) {
	t.Helper()
	oldInterval, oldMaxAge, oldLinger := bdsMaintainInterval, bdsConnMaxAge, bdsAgeRecycleLinger
	bdsMaintainInterval, bdsConnMaxAge, bdsAgeRecycleLinger = interval, maxAge, linger
	t.Cleanup(func() {
		bdsMaintainInterval, bdsConnMaxAge, bdsAgeRecycleLinger = oldInterval, oldMaxAge, oldLinger
	})
}

// newMaintainPool builds a pool of poolSize conns against addr and joins its
// maintainer on cleanup.
func newMaintainPool(t *testing.T, addr string, poolSize int, expectedChainId uint64) *bdsPool {
	t.Helper()
	lg := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := newBdsPool(ctx, &lg, "prj1", "bds1", "dns:///"+addr,
		insecure.NewCredentials(), identityServiceConfig, poolSize, expectedChainId)
	require.NoError(t, err)
	t.Cleanup(p.Shutdown)
	return p
}

// connAt reads one slot under the pool lock.
func connAt(p *bdsPool, i int) *bdsConn {
	p.poolMu.RLock()
	defer p.poolMu.RUnlock()
	return p.conns[i]
}

// waitFor polls cond until it holds or the bound expires.
func waitFor(t *testing.T, bound time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition never held within %v: %s", bound, msg)
}

// A conn closed in place — the state a failed replacement leaves behind —
// serves nothing, forever. The maintainer must notice it is Shutdown and dial a
// replacement into its slot, or the pool silently loses a third of its capacity
// for the process lifetime.
func TestMaintainLoop_ReplacesAConnClosedInPlace(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 1)
	defer stop()

	compressMaintainTimers(t, 20*time.Millisecond, 0, time.Millisecond)
	p := newMaintainPool(t, addr, 2, 0)

	original := connAt(p, 0)
	require.NoError(t, original.conn.Close())

	waitFor(t, 5*time.Second, "slot 0 was never re-dialed", func() bool {
		return connAt(p, 0) != original
	})
	require.Equal(t, connectivity.Shutdown, original.conn.GetState(),
		"the closed conn must stay closed")
	require.NotEqual(t, connectivity.Shutdown, connAt(p, 0).conn.GetState(),
		"the replacement must be a live conn, not another closed one")
}

// A backend that starts answering for a DIFFERENT chain is cross-wired: a
// stale DNS record, or an address reused by another chain's server. The
// maintainer must close that conn on the spot. Leaving it open would let
// another chain's blocks answer this network's requests, and nothing
// downstream can tell them apart.
func TestMaintainLoop_QuarantinesAConnThatAnswersForAnotherChain(t *testing.T) {
	addr, srv, stop := startSwitchableChainServer(t, 137)
	defer stop()

	compressMaintainTimers(t, 20*time.Millisecond, 0, time.Millisecond)
	p := newMaintainPool(t, addr, 1, 137)

	original := connAt(p, 0)
	require.NotEqual(t, connectivity.Shutdown, original.conn.GetState())

	// The endpoint is re-pointed at another chain's server.
	srv.chainID.Store(999)

	waitFor(t, 5*time.Second, "a conn answering for chainId 999 was never quarantined", func() bool {
		return original.conn.GetState() == connectivity.Shutdown
	})

	// Every replacement is verified too, so while the endpoint stays wrong the
	// slot cannot be restored — the pool refuses to install a wrong-chain conn.
	require.Equal(t, connectivity.Shutdown, connAt(p, 0).conn.GetState(),
		"no live conn may be installed while the endpoint answers for the wrong chain")

	// Once the endpoint is right again, the maintainer restores capacity.
	srv.chainID.Store(137)
	waitFor(t, 5*time.Second, "the slot was never restored after the endpoint recovered", func() bool {
		c := connAt(p, 0)
		return c != original && c.conn.GetState() != connectivity.Shutdown
	})
}

// Age recycling is capped at ONE conn per tick. Without the cap a pool whose
// conns were dialled together recycles in lockstep and drops all of its
// capacity at the same instant — exactly the stampede the jitter and the cap
// exist to prevent.
func TestMaintainLoop_AgeRecyclesAtMostOneConnPerTick(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 1)
	defer stop()

	// A max age far shorter than the tick makes all three conns over-age well
	// before the maintainer first wakes, so the cap is the only thing that can
	// keep the first tick from recycling every one of them.
	const tick = 300 * time.Millisecond
	compressMaintainTimers(t, tick, 20*time.Millisecond, time.Millisecond)
	p := newMaintainPool(t, addr, 3, 0)

	originals := []*bdsConn{connAt(p, 0), connAt(p, 1), connAt(p, 2)}
	recycled := func() int {
		n := 0
		for i, o := range originals {
			if connAt(p, i) != o {
				n++
			}
		}
		return n
	}

	waitFor(t, 5*time.Second, "no slot was ever age-recycled", func() bool {
		return recycled() > 0
	})
	// The next tick is a full tick away, so this reads the state the FIRST
	// tick left behind. Recycling in bulk would show all three here.
	require.Equal(t, 1, recycled(),
		"one tick must recycle at most one aged conn; recycling in lockstep drops the whole pool's capacity at once")
}

// Shutdown must JOIN the maintainer, not merely signal it. A tick already in
// flight can dial and swap a fresh conn into a slot; if Shutdown has already
// walked the slots by then, that socket is never closed and leaks for the
// process lifetime. The test pins the join directly: with the maintainer
// parked inside a verification probe, Shutdown must not return.
func TestPoolShutdown_JoinsTheMaintainer(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	addr, stop := startGatedChainIdServer(t, 137, gate, entered)
	defer stop()

	compressMaintainTimers(t, 50*time.Millisecond, 0, time.Millisecond)

	lg := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Built unarmed so construction does not probe, then armed so the FIRST
	// thing the maintainer does is a verification probe — which the server
	// holds open until this test releases it.
	p, err := newBdsPool(ctx, &lg, "prj1", "bds1", "dns:///"+addr,
		insecure.NewCredentials(), identityServiceConfig, 1, 0)
	require.NoError(t, err)
	p.expectedChainId.Store(137)

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(gate)
		t.Fatal("the maintainer never reached its verification probe")
	}

	done := make(chan struct{})
	go func() {
		p.Shutdown()
		close(done)
	}()

	select {
	case <-done:
		close(gate)
		t.Fatal("Shutdown returned while the maintainer was still running; a tick in flight can install a conn after the slots are walked, and that socket leaks")
	case <-time.After(200 * time.Millisecond):
	}

	close(gate)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never returned after the maintainer was released")
	}

	require.Equal(t, connectivity.Shutdown, connAt(p, 0).conn.GetState(),
		"every pooled conn must be closed once Shutdown returns")
}

// Shutdown is called from both the app-context watcher and the construction
// failure path, so it must tolerate being called twice. The second call must
// not panic on the already-closed stop channel, nor block on the WaitGroup.
func TestPoolShutdown_IsIdempotent(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 1)
	defer stop()

	compressMaintainTimers(t, 20*time.Millisecond, 0, time.Millisecond)
	p := newMaintainPool(t, addr, 2, 0)

	done := make(chan struct{})
	go func() {
		p.Shutdown()
		p.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a second Shutdown blocked")
	}
}

// ───────────────────────────── test servers ─────────────────────────────

// switchableChainServer answers ChainId with whatever the test last stored,
// modelling an endpoint that gets re-pointed at another chain's backend.
type switchableChainServer struct {
	evm.UnimplementedRPCQueryServiceServer
	chainID atomic.Uint64
}

func (s *switchableChainServer) ChainId(_ context.Context, _ *evm.ChainIdRequest) (*evm.ChainIdResponse, error) {
	return &evm.ChainIdResponse{ChainId: s.chainID.Load()}, nil
}

func startSwitchableChainServer(t *testing.T, chainID uint64) (string, *switchableChainServer, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	impl := &switchableChainServer{}
	impl.chainID.Store(chainID)
	evm.RegisterRPCQueryServiceServer(srv, impl)
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String(), impl, srv.Stop
}

// gatedChainIdServer holds every ChainId call until gate is closed, and
// signals the first arrival on entered. It parks the maintainer inside a
// verification probe so a test can observe what Shutdown does while a tick is
// genuinely in flight.
type gatedChainIdServer struct {
	evm.UnimplementedRPCQueryServiceServer
	chainID uint64
	gate    <-chan struct{}
	entered chan<- struct{}
}

func (s *gatedChainIdServer) ChainId(ctx context.Context, _ *evm.ChainIdRequest) (*evm.ChainIdResponse, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &evm.ChainIdResponse{ChainId: s.chainID}, nil
}

func startGatedChainIdServer(t *testing.T, chainID uint64, gate chan struct{}, entered chan struct{}) (string, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	evm.RegisterRPCQueryServiceServer(srv, &gatedChainIdServer{chainID: chainID, gate: gate, entered: entered})
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String(), srv.Stop
}
