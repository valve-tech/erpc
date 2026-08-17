package data

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// breakProxy is a TCP proxy that sits between a connector and a real server.
// It forwards bytes in both directions. On command it severs every live
// connection, or it refuses new ones. That turns "the database went away and
// came back" into a deterministic event instead of a hope.
//
// Reuse it for any connector that dials TCP. Point the connector's address at
// Addr() instead of the container, then:
//
//	proxy.Break()        // every live connection dies now
//	proxy.Refuse(true)   // new connections are accepted and dropped at once
//	proxy.Refuse(false)  // new connections are forwarded again
//
// The proxy never blocks a caller: Break and Refuse return immediately, and
// Close is registered with t.Cleanup by newBreakProxy.
type breakProxy struct {
	target string
	ln     net.Listener

	mu   sync.Mutex
	live map[net.Conn]struct{}

	refusing atomic.Bool
	accepted atomic.Int64
	severed  atomic.Int64
	closing  atomic.Bool
	wg       sync.WaitGroup
}

// newBreakProxy starts a proxy in front of target ("host:port") and returns it
// listening on a loopback port. The proxy is closed when the test ends.
func newBreakProxy(t *testing.T, target string) *breakProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "the break proxy could not listen")

	p := &breakProxy{
		target: target,
		ln:     ln,
		live:   make(map[net.Conn]struct{}),
	}
	p.wg.Add(1)
	go p.acceptLoop()
	t.Cleanup(p.Close)
	return p
}

// Addr reports the "host:port" a client must dial to reach the proxy.
func (p *breakProxy) Addr() string { return p.ln.Addr().String() }

// Accepted reports how many connections the proxy has accepted so far. A
// reconnect test uses it to prove the client really re-dialled.
func (p *breakProxy) Accepted() int64 { return p.accepted.Load() }

// Severed reports how many connection ends Break has closed so far.
func (p *breakProxy) Severed() int64 { return p.severed.Load() }

// Refuse turns new connections away when on is true. The proxy still accepts
// the TCP connection, then closes it without dialling the target, so the
// client observes the socket dying during its handshake.
func (p *breakProxy) Refuse(on bool) { p.refusing.Store(on) }

// Break closes every live connection in both directions and reports how many
// connection ends it closed. Connections opened after Break are unaffected.
func (p *breakProxy) Break() int {
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.live))
	for c := range p.live {
		conns = append(conns, c)
	}
	p.live = make(map[net.Conn]struct{})
	p.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
	p.severed.Add(int64(len(conns)))
	return len(conns)
}

// LiveConns reports how many connection ends the proxy currently forwards.
func (p *breakProxy) LiveConns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.live)
}

// Close stops the proxy, severs everything it still forwards, and waits for
// its goroutines to exit. Calling it twice is safe.
func (p *breakProxy) Close() {
	if !p.closing.CompareAndSwap(false, true) {
		return
	}
	_ = p.ln.Close()
	p.Break()
	p.wg.Wait()
}

func (p *breakProxy) acceptLoop() {
	defer p.wg.Done()
	for {
		down, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.accepted.Add(1)

		if p.refusing.Load() {
			_ = down.Close()
			continue
		}

		up, derr := net.DialTimeout("tcp", p.target, 5*time.Second)
		if derr != nil {
			_ = down.Close()
			continue
		}

		p.track(down, up)
		p.wg.Add(2)
		go p.pipe(down, up)
		go p.pipe(up, down)
	}
}

func (p *breakProxy) track(a, b net.Conn) {
	p.mu.Lock()
	if p.closing.Load() {
		// Close already ran; do not adopt a connection nobody will sever.
		p.mu.Unlock()
		_ = a.Close()
		_ = b.Close()
		return
	}
	p.live[a] = struct{}{}
	p.live[b] = struct{}{}
	p.mu.Unlock()
}

// pipe copies src into dst until either end goes away, then closes both so the
// peer sees the break too.
func (p *breakProxy) pipe(src, dst net.Conn) {
	defer p.wg.Done()
	_, _ = io.Copy(dst, src)
	_ = src.Close()
	_ = dst.Close()
	p.mu.Lock()
	delete(p.live, src)
	delete(p.live, dst)
	p.mu.Unlock()
}

// waitFor polls cond until it holds or the deadline passes. Every reconnect
// assertion in this package goes through it, so no wait can hang: a mutation
// that stalls the code under test fails the test instead of wedging the run.
func waitFor(t *testing.T, timeout time.Duration, step time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(step)
	}
}
