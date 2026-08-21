package util

import (
	"bytes"
	"io"
	"testing"

	"github.com/klauspost/compress/flate"
	"github.com/stretchr/testify/require"
)

// The three pools in this package sit on the per-request HTTP path. A pool
// that accepts a buffer it must reject grows without bound; a pool that
// panics on a nil argument takes the whole request down. These tests cover
// the guard branches the happy-path pool tests never reach.

func TestReturnBuf_NilIsANoOp(t *testing.T) {
	// Callers return a borrowed buffer from a defer. When the borrow failed,
	// the defer still fires with nil. A panic here kills the request.
	require.NotPanics(t, func() { ReturnBuf(nil) })
}

func TestReturnBuf_KeepsTheOversizedBufferOutOfThePool(t *testing.T) {
	// A single huge response must not pin its buffer in the pool forever.
	// sync.Pool hands an item back to the same goroutine through a per-P
	// private slot, so a put-then-borrow pair normally round-trips. The
	// assertion is the invariant, not the round-trip: the oversized buffer
	// must NEVER come back, however many times we try.
	const attempts = 100

	// Control: a normal-sized buffer does round-trip in this environment.
	// Without it, a pool that returned nothing every time would make the
	// real assertion below pass for the wrong reason.
	normalReturns := 0
	for i := 0; i < attempts; i++ {
		normal := bytes.NewBuffer(make([]byte, 0, 1024))
		ReturnBuf(normal)
		if BorrowBuf() == normal {
			normalReturns++
		}
	}
	require.Greater(t, normalReturns, 0,
		"control failed: the pool never returned a normal buffer, so the oversize check below proves nothing")

	oversizedReturns := 0
	for i := 0; i < attempts; i++ {
		oversized := bytes.NewBuffer(make([]byte, 0, 4*maxBufCap+1))
		ReturnBuf(oversized)
		if BorrowBuf() == oversized {
			oversizedReturns++
		}
	}
	require.Equal(t, 0, oversizedReturns,
		"an oversized buffer was put back in the pool; the pool now holds memory it will not reuse")
}

func TestGzipReaderPool_PutNilIsANoOp(t *testing.T) {
	// GetReset returns (nil, err) on a corrupt stream. Callers that Put in a
	// defer hand that nil straight back.
	p := NewGzipReaderPool()
	require.NotPanics(t, func() { p.Put(nil) })
}

func TestGzipWriterPool_PutNilIsANoOp(t *testing.T) {
	p := NewGzipWriterPool()
	require.NotPanics(t, func() { p.Put(nil) })
}

func TestEofReader_SatisfiesFlateReaderAndAlwaysReportsEOF(t *testing.T) {
	// GzipReaderPool.Put resets the pooled reader against eofReader so the
	// next Reset reuses the existing bufio.Reader instead of allocating one.
	// That only works while eofReader implements flate.Reader, and only
	// terminates while both methods report EOF. A ReadByte that returned
	// (0, nil) would spin the gzip header parser forever.
	var fr flate.Reader = eofReader{}

	b, err := fr.ReadByte()
	require.Equal(t, io.EOF, err)
	require.Equal(t, byte(0), b)

	n, err := fr.Read(make([]byte, 8))
	require.Equal(t, io.EOF, err)
	require.Equal(t, 0, n)
}
