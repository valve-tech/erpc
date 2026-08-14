package util

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ReadAll is on the hot path for every upstream response body. It hands the
// caller a pooled buffer, so two things must hold: the bytes are exact, and
// a failed read returns the buffer to the pool instead of leaking it.

func TestReadAll_ReturnsExactBytesForEveryHintedSize(t *testing.T) {
	body := strings.Repeat("0123456789", 500) // 5 KB
	for _, hint := range []int{0, -1, 1, len(body), len(body) * 4} {
		data, release, err := ReadAll(strings.NewReader(body), hint)
		require.NoError(t, err, "hint %d", hint)
		require.Equal(t, body, string(data), "hint %d must not change the bytes read", hint)
		require.NotNil(t, release, "caller needs a release func to return the pooled buffer")
		release()
	}
}

func TestReadAll_EmptyBodyYieldsEmptyData(t *testing.T) {
	data, release, err := ReadAll(strings.NewReader(""), 0)
	require.NoError(t, err)
	require.Empty(t, data)
	require.NotNil(t, release)
	release()
}

func TestReadAll_HugeSizeHintDoesNotBlowUpTheAllocation(t *testing.T) {
	// A hostile or broken Content-Length must not turn into a multi-GB
	// pre-grow; the cap keeps one bad upstream from OOMing the proxy.
	data, release, err := ReadAll(strings.NewReader("tiny"), 1<<40)
	require.NoError(t, err)
	require.Equal(t, "tiny", string(data))
	release()
}

var errBoom = errors.New("boom")

type failingReader struct {
	prefix []byte
	sent   bool
}

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.sent {
		f.sent = true
		n := copy(p, f.prefix)
		return n, nil
	}
	return 0, errBoom
}

func TestReadAll_PropagatesReadErrorAndReturnsNoBuffer(t *testing.T) {
	// On a mid-body upstream failure the caller must get the error, and
	// must NOT get a release func — calling one would double-return the
	// buffer to the pool and hand the same memory to two requests.
	data, release, err := ReadAll(&failingReader{prefix: []byte("partial")}, 0)
	require.ErrorIs(t, err, errBoom)
	require.Nil(t, data)
	require.Nil(t, release, "a failed read must not hand back a release func")
}

func TestReadAll_MatchesStdlibForABinaryBody(t *testing.T) {
	raw := make([]byte, 4096)
	for i := range raw {
		raw[i] = byte(i % 251)
	}
	want, err := io.ReadAll(bytes.NewReader(raw))
	require.NoError(t, err)

	got, release, err := ReadAll(bytes.NewReader(raw), len(raw))
	require.NoError(t, err)
	require.Equal(t, want, got)
	release()
}
