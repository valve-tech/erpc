package data

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startEchoServer runs a loopback TCP echo server for the proxy's own tests.
func startEchoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// dialProxy opens a connection through the proxy with a bounded deadline.
func dialProxy(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, c.SetDeadline(time.Now().Add(5*time.Second)))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// The proxy is only useful if it forwards bytes untouched. A fixture that
// quietly mangles the stream would make every connector test lie.
func TestBreakProxy_ForwardsBytesBothWays(t *testing.T) {
	t.Parallel()
	proxy := newBreakProxy(t, startEchoServer(t))

	c := dialProxy(t, proxy.Addr())
	_, err := c.Write([]byte("hello"))
	require.NoError(t, err)

	buf := make([]byte, 5)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(buf))
	assert.Equal(t, int64(1), proxy.Accepted())
}

// Break must sever a live connection immediately, and the client must see it.
// This is the fixture's whole reason to exist: without it a reconnect test
// cannot tell a real recovery from a connection that never broke.
func TestBreakProxy_BreakSeversALiveConnection(t *testing.T) {
	t.Parallel()
	proxy := newBreakProxy(t, startEchoServer(t))

	c := dialProxy(t, proxy.Addr())
	_, err := c.Write([]byte("x"))
	require.NoError(t, err)
	buf := make([]byte, 1)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)

	severed := proxy.Break()
	assert.Equal(t, 2, severed, "both ends of the one live pair must be closed")

	_, err = c.Write([]byte("y"))
	if err == nil {
		_, err = c.Read(buf)
	}
	require.Error(t, err, "a severed connection must not keep working")
}

// A broken connection must not stop the next one. That is what makes the
// proxy model "the database went away and came back" rather than "the database
// went away".
func TestBreakProxy_ANewConnectionWorksAfterABreak(t *testing.T) {
	t.Parallel()
	proxy := newBreakProxy(t, startEchoServer(t))

	first := dialProxy(t, proxy.Addr())
	_, err := first.Write([]byte("a"))
	require.NoError(t, err)
	buf := make([]byte, 1)
	_, err = io.ReadFull(first, buf)
	require.NoError(t, err)
	proxy.Break()

	second := dialProxy(t, proxy.Addr())
	_, err = second.Write([]byte("b"))
	require.NoError(t, err)
	_, err = io.ReadFull(second, buf)
	require.NoError(t, err)
	assert.Equal(t, "b", string(buf))
	assert.Equal(t, int64(2), proxy.Accepted())
}

// While refusing, the proxy must drop every new connection without reaching
// the target, and it must forward again as soon as refusing is turned off.
func TestBreakProxy_RefusesAndThenRecovers(t *testing.T) {
	t.Parallel()
	proxy := newBreakProxy(t, startEchoServer(t))

	proxy.Refuse(true)
	refused, err := net.DialTimeout("tcp", proxy.Addr(), 5*time.Second)
	require.NoError(t, err, "the proxy still accepts the TCP connection")
	require.NoError(t, refused.SetDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, 1)
	// Write first, so a forwarded connection would echo the byte back. Only a
	// refused one ends the stream. Reading without writing would block until
	// the deadline and look like a refusal either way.
	_, _ = refused.Write([]byte("r"))
	_, err = io.ReadFull(refused, buf)
	require.Error(t, err, "a refused connection must be closed without an answer")
	require.NotContains(t, err.Error(), "timeout", "the stream must end, not merely go quiet")
	_ = refused.Close()

	proxy.Refuse(false)
	c := dialProxy(t, proxy.Addr())
	_, err = c.Write([]byte("z"))
	require.NoError(t, err)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)
	assert.Equal(t, "z", string(buf))
}

// Closing the proxy must sever what it still forwards and leave nothing live.
// A leaked pipe goroutine would outlive the test and hold a container port.
func TestBreakProxy_CloseSeversEverythingAndIsIdempotent(t *testing.T) {
	t.Parallel()
	proxy := newBreakProxy(t, startEchoServer(t))

	c := dialProxy(t, proxy.Addr())
	_, err := c.Write([]byte("q"))
	require.NoError(t, err)
	buf := make([]byte, 1)
	_, err = io.ReadFull(c, buf)
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		proxy.Close()
		proxy.Close()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("closing the proxy did not return")
	}

	assert.Zero(t, proxy.LiveConns())
	_, err = net.DialTimeout("tcp", proxy.Addr(), time.Second)
	assert.Error(t, err, "a closed proxy must stop listening")
}
