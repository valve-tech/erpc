package clients

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// The watchdog and the pool constructor both have refusal paths that only run
// when something has already gone wrong — a dial that fails, a replacement that
// turns out to serve another chain. Those are exactly the paths that must not
// make things worse, and none of them ran under test.

// A replacement that PROVABLY answers for another chain must not be installed.
// Installing it would hand the watchdog's own recovery path the cross-wire it
// exists to escape: the wedged conn is replaced by a conn serving a different
// chain's data, and every later request is answered wrongly rather than slowly.
func TestReplaceConn_RefusesAReplacementServingAnotherChain(t *testing.T) {
	addr, srv, stop := startSwitchableChainServer(t, 137)
	defer stop()

	// A long maintain interval keeps the maintainer out of this test; the
	// watchdog path is driven directly.
	compressMaintainTimers(t, time.Hour, 0, time.Millisecond)
	p := newMaintainPool(t, addr, 1, 137)

	original := connAt(p, 0)

	// The endpoint is re-pointed at another chain's server, so every fresh
	// dial reaches the wrong chain.
	srv.chainID.Store(999)
	p.replaceConn(original)

	require.Same(t, original, connAt(p, 0),
		"the watchdog installed a conn that answers for chainId 999")
	require.NotEqual(t, connectivity.Shutdown, original.conn.GetState(),
		"the watchdog closed the old conn even though it had no replacement to install")
}

// The watchdog must install a healthy replacement when the endpoint is fine —
// otherwise the test above would pass on a watchdog that refuses everything.
func TestReplaceConn_InstallsAReplacementOnAMatchingChain(t *testing.T) {
	addr, _, stop := startSwitchableChainServer(t, 137)
	defer stop()

	compressMaintainTimers(t, time.Hour, 0, time.Millisecond)
	p := newMaintainPool(t, addr, 1, 137)

	original := connAt(p, 0)
	p.replaceConn(original)

	require.NotSame(t, original, connAt(p, 0),
		"the watchdog left the wedged conn in place against a healthy endpoint")
	require.Equal(t, connectivity.Shutdown, original.conn.GetState(),
		"the replaced conn must be closed, or its socket leaks")
}

// A dial that grpc-go refuses must fail the whole pool, naming the target. A
// pool that came up with nil slots instead would answer Pick() with nil and
// turn every request into "no available connections" with nothing in the logs
// pointing at the cause.
func TestNewBdsPool_FailsLoudlyWhenADialIsRefused(t *testing.T) {
	lg := zerolog.New(io.Discard)
	// An unparseable default service config is rejected by grpc.NewClient, so
	// the very first dial errors.
	p, err := newBdsPool(context.Background(), &lg, "prj1", "bds1",
		"dns:///127.0.0.1:50051",
		insecure.NewCredentials(), "{not json", 3, 0)
	require.Error(t, err, "a pool was built despite every dial being refused")
	require.Nil(t, p, "a failed constructor must not hand back a half-built pool")
	require.Contains(t, err.Error(), "failed to dial gRPC server at dns:///127.0.0.1:50051",
		"the error must name the target so an operator can find the misconfigured upstream")
}

// newBdsPool is called with a nil context by callers that have none. It must
// substitute a background context rather than store nil, because the maintainer
// selects on appCtx.Done() every tick and a nil context panics there.
func TestNewBdsPool_SubstitutesABackgroundContextForNil(t *testing.T) {
	addr, _, stop := startHappyServer(t, 1, 1)
	defer stop()

	lg := zerolog.New(io.Discard)
	//nolint:staticcheck // passing nil is the case under test
	p, err := newBdsPool(nil, &lg, "prj1", "bds1", "dns:///"+addr,
		insecure.NewCredentials(), identityServiceConfig, 1, 0)
	require.NoError(t, err)
	t.Cleanup(p.Shutdown)
	require.NotNil(t, p.appCtx, "a nil app context was stored as-is; the maintainer panics on it")
	require.NoError(t, p.appCtx.Err())
}
