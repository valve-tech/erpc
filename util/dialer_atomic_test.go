package util

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDefaultOutboundDialer_SetsAKeepAliveTheKernelActsOn(t *testing.T) {
	// Every hand-built outbound http.Transport in the tree takes its
	// DialContext from here. When KeepAlive is zero, Go leaves the socket on
	// the OS default (7200s on Linux), so a wedged flow stays invisible for
	// two hours. The exact value matters less than "small enough that the
	// kernel reaps a dead flow in seconds".
	d := DefaultOutboundDialer()
	require.Positive(t, d.KeepAlive, "a zero KeepAlive falls back to the 7200s OS default")
	require.LessOrEqual(t, d.KeepAlive, time.Minute)
	require.Positive(t, d.Timeout, "a dial with no timeout can hang a request forever")
}

func TestDefaultOutboundDialer_ReturnsAnIndependentDialerPerCall(t *testing.T) {
	// Callers keep the returned dialer for the lifetime of a transport. A
	// shared instance would let one transport's tuning change every other
	// transport in the process.
	a := DefaultOutboundDialer()
	b := DefaultOutboundDialer()
	require.NotSame(t, a, b)

	a.KeepAlive = 99 * time.Second
	require.NotEqual(t, a.KeepAlive, b.KeepAlive)
}

func TestAtomicValue_StoresTheValueItWasGiven(t *testing.T) {
	av := AtomicValue("evm:123")
	require.Equal(t, "evm:123", av.Load())

	// The returned value is usable as a normal atomic.Value: a later Store
	// of the same concrete type replaces the contents.
	av.Store("evm:456")
	require.Equal(t, "evm:456", av.Load())
}
