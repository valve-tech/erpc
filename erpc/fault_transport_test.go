package erpc

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// A response only exists once it reaches the client. Every write path in this
// package therefore has a second half that no ordinary test sees: the client
// hangs up, the socket refuses, and eRPC must stop, report, and release what it
// was holding. This file is the fixture for that half.
//
// Three pieces:
//
//   - faultSink  — the write recorder and fault plan. It answers what reached
//     the peer, when the peer refused, and how many writes the caller
//     attempted AFTER the refusal. That last count is the one that catches
//     real defects: an unchecked write leaves the count above zero.
//   - faultResponseWriter — a faultSink shaped as an http.ResponseWriter,
//     http.Flusher and http.Hijacker. Hand it to any handler.
//   - faultConn — a faultSink shaped as a net.Conn, plus newFaultWsConnection,
//     which upgrades a synthetic handshake onto it and returns the server-side
//     WsConnection. The websocket write helpers then run exactly as in
//     production over a socket the test can break at any byte.
//
// Reuse: build one, arm it, run the code, then assert on the recorder.
//
//	w := newFaultResponseWriter()
//	w.FailAfterBytes(12)          // the socket takes 12 bytes and then dies
//	handler.ServeHTTP(w, req)
//	require.Zero(t, w.WritesAfterFailure())
//
// The arming methods are FailAfterBytes, FailOnWrite, FailOnFlush, FailWith
// and HangUp. They are declared on *faultSink, so call them as statements
// rather than chaining off the constructor.

// errPeerHungUp is the transport error every unarmed fault reports. Tests match
// on this value, so a handler that swallows it and substitutes its own message
// fails the assertion instead of passing quietly.
var errPeerHungUp = errors.New("fault: peer hung up")

// faultSink records writes and refuses them on a schedule the test sets.
type faultSink struct {
	mu sync.Mutex

	// failAfterBytes refuses the write that would push the accepted total past
	// this many bytes, reporting the partial count first, the way a socket
	// does. Negative disables it. Zero refuses everything.
	failAfterBytes int
	// failOnWrite refuses the Nth Write call outright, counting from 1.
	// Zero disables it.
	failOnWrite int
	// failOnFlush kills the sink on the Nth Flush, counting from 1. Flush
	// cannot report an error, so the peer's death shows up on the next write.
	// Zero disables it.
	failOnFlush int
	// err is what every refusal returns. Nil means errPeerHungUp.
	err error
	// panicValue makes Write panic instead of returning. net/http's own
	// ResponseWriter does this with http.ErrAbortHandler, so a handler's
	// panic recovery has to survive it.
	panicValue interface{}

	accepted    bytes.Buffer
	writes      int
	writesAfter int
	flushes     int
	failed      bool
}

func newFaultSink() *faultSink { return &faultSink{failAfterBytes: -1} }

// FailAfterBytes accepts n bytes and refuses everything from there on.
func (f *faultSink) FailAfterBytes(n int) *faultSink {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failAfterBytes = n
	return f
}

// FailOnWrite refuses the nth Write call and every call after it.
func (f *faultSink) FailOnWrite(n int) *faultSink {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnWrite = n
	return f
}

// FailOnFlush kills the sink on the nth Flush.
func (f *faultSink) FailOnFlush(n int) *faultSink {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOnFlush = n
	return f
}

// FailWith replaces errPeerHungUp, so a test can check that a specific
// transport error survives the code under test unchanged.
func (f *faultSink) FailWith(err error) *faultSink {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
	return f
}

// HangUp refuses every write from the first byte — the client that is already
// gone when eRPC starts to answer.
func (f *faultSink) HangUp() *faultSink { return f.FailAfterBytes(0) }

// PanicOnWrite makes every Write panic with v, the way net/http aborts a
// handler with http.ErrAbortHandler.
func (f *faultSink) PanicOnWrite(v interface{}) *faultSink {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.panicValue = v
	return f
}

// reason must be called with the lock held.
func (f *faultSink) reason() error {
	if f.err != nil {
		return f.err
	}
	return errPeerHungUp
}

func (f *faultSink) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.failed {
		f.writesAfter++
	}
	f.writes++

	if f.panicValue != nil {
		panic(f.panicValue)
	}
	if f.failOnWrite > 0 && f.writes >= f.failOnWrite {
		f.failed = true
		return 0, f.reason()
	}
	if f.failAfterBytes >= 0 {
		room := f.failAfterBytes - f.accepted.Len()
		if room <= 0 {
			f.failed = true
			return 0, f.reason()
		}
		if len(p) > room {
			n, _ := f.accepted.Write(p[:room])
			f.failed = true
			return n, f.reason()
		}
	}
	if f.failed {
		return 0, f.reason()
	}
	return f.accepted.Write(p)
}

func (f *faultSink) flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	if f.failOnFlush > 0 && f.flushes >= f.failOnFlush {
		f.failed = true
	}
}

// Reset forgets everything recorded so far but keeps the fault plan. The
// websocket helper uses it to drop the handshake bytes.
func (f *faultSink) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepted.Reset()
	f.writes = 0
	f.writesAfter = 0
	f.flushes = 0
	f.failed = false
}

// Bytes returns what the peer actually accepted.
func (f *faultSink) Bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.accepted.Bytes()...)
}

// Body returns the accepted bytes as a string.
func (f *faultSink) Body() string { return string(f.Bytes()) }

// Writes counts every Write call, accepted or refused.
func (f *faultSink) Writes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

// WritesAfterFailure counts the writes attempted once the sink is already
// dead. A caller that checks its errors leaves this at zero — except when the
// sink was killed by FailOnFlush, where the first refused write is itself the
// one that learns, so one is the correct answer and two is the defect.
func (f *faultSink) WritesAfterFailure() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writesAfter
}

// Flushes counts Flush calls.
func (f *faultSink) Flushes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushes
}

// Failed reports whether the sink has refused at least one write.
func (f *faultSink) Failed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed
}

//
// --- http.ResponseWriter ---
//

// faultResponseWriter is a faultSink an HTTP handler can write to. It records
// the header map and the status code, and it always advertises http.Hijacker —
// set HijackTo first, or Hijack reports that nothing is configured.
type faultResponseWriter struct {
	*faultSink

	hdr         http.Header
	sent        http.Header
	status      int
	headerCalls int

	hijackConn net.Conn
	hijackErr  error
	hijacked   bool
}

func newFaultResponseWriter() *faultResponseWriter {
	return &faultResponseWriter{faultSink: newFaultSink(), hdr: http.Header{}}
}

func (w *faultResponseWriter) Header() http.Header { return w.hdr }

// WriteHeader keeps the FIRST status code and snapshots the header map, the
// way net/http does. Everything a handler sets afterwards is lost to the
// client, so SentHeader — not Header — is what a test must assert on.
func (w *faultResponseWriter) WriteHeader(code int) {
	w.headerCalls++
	if w.headerCalls == 1 {
		w.status = code
		w.sent = w.hdr.Clone()
		if w.sent == nil {
			w.sent = http.Header{}
		}
	}
}

// Status is the first status code the handler sent, or 0 if it sent none.
func (w *faultResponseWriter) Status() int { return w.status }

// SentHeader is the header block the client actually received. It is empty
// until WriteHeader fires.
func (w *faultResponseWriter) SentHeader() http.Header {
	if w.sent == nil {
		return http.Header{}
	}
	return w.sent
}

// HeaderWrites counts WriteHeader calls.
func (w *faultResponseWriter) HeaderWrites() int { return w.headerCalls }

func (w *faultResponseWriter) Flush() { w.flush() }

// HijackTo makes Hijack hand back conn.
func (w *faultResponseWriter) HijackTo(conn net.Conn) *faultResponseWriter {
	w.hijackConn = conn
	return w
}

// HijackFails makes Hijack refuse, standing in for a server that has already
// taken the connection back.
func (w *faultResponseWriter) HijackFails(err error) *faultResponseWriter {
	w.hijackErr = err
	return w
}

func (w *faultResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.hijackErr != nil {
		return nil, nil, w.hijackErr
	}
	if w.hijackConn == nil {
		return nil, nil, errors.New("fault: no hijackable connection configured")
	}
	w.hijacked = true
	brw := bufio.NewReadWriter(
		bufio.NewReaderSize(w.hijackConn, 4096),
		bufio.NewWriterSize(w.hijackConn, 4096),
	)
	return w.hijackConn, brw, nil
}

var (
	_ http.ResponseWriter = (*faultResponseWriter)(nil)
	_ http.Flusher        = (*faultResponseWriter)(nil)
	_ http.Hijacker       = (*faultResponseWriter)(nil)
)

//
// --- net.Conn ---
//

// faultAddr is the address both ends of a faultConn report.
type faultAddr struct{}

func (faultAddr) Network() string { return "fault" }
func (faultAddr) String() string  { return "fault:0" }

// faultConn is a net.Conn a test drives from both ends. Reads come from frames
// the test queues with Feed; writes obey the fault plan.
type faultConn struct {
	*faultSink

	readMu  sync.Mutex
	readCh  chan []byte
	pending []byte

	closeOnce sync.Once
	closed    chan struct{}
}

func newFaultConn() *faultConn {
	return &faultConn{
		faultSink: newFaultSink(),
		readCh:    make(chan []byte, 16),
		closed:    make(chan struct{}),
	}
}

// Feed queues bytes for the next Read. It never blocks the test: once the
// connection is closed the bytes are dropped.
func (c *faultConn) Feed(b []byte) {
	select {
	case c.readCh <- b:
	case <-c.closed:
	}
}

func (c *faultConn) Read(p []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for len(c.pending) == 0 {
		select {
		case b := <-c.readCh:
			c.pending = b
		case <-c.closed:
			return 0, io.EOF
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *faultConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *faultConn) LocalAddr() net.Addr                { return faultAddr{} }
func (c *faultConn) RemoteAddr() net.Addr               { return faultAddr{} }
func (c *faultConn) SetDeadline(t time.Time) error      { return nil }
func (c *faultConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *faultConn) SetWriteDeadline(t time.Time) error { return nil }

var _ net.Conn = (*faultConn)(nil)

//
// --- WebSocket ---
//

// newFaultWsConn upgrades a synthetic handshake onto a faultConn and returns
// the server-side gorilla connection over it. The handshake bytes are cleared
// before it returns, so a test counts only the frames it caused.
func newFaultWsConn(t *testing.T) (*websocket.Conn, *faultConn) {
	t.Helper()

	sock := newFaultConn()
	w := newFaultResponseWriter().HijackTo(sock)

	r := httptest.NewRequest(http.MethodGet, "http://erpc.test/proj/evm/123", nil)
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := up.Upgrade(w, r, nil)
	require.NoError(t, err, "the fixture must complete the handshake before a test arms any fault")

	t.Cleanup(func() {
		_ = conn.Close()
		_ = sock.Close()
	})

	sock.Reset()
	return conn, sock
}

// newFaultWsConnection wraps that connection in the WsConnection the server
// uses, so the write helpers under test run their production code over a
// socket the test can break at any byte.
func newFaultWsConnection(t *testing.T) (*WsConnection, *faultConn) {
	t.Helper()

	conn, sock := newFaultWsConn(t)
	lg := zerolog.New(io.Discard)
	return &WsConnection{
		id:     "ws-fault",
		conn:   conn,
		logger: &lg,
	}, sock
}
