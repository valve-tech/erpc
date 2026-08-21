package util

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// B2Str / S2Bytes skip a copy on the JSON-RPC hot path. They are unsafe by
// construction, so the tests pin the two things callers rely on: the
// contents round-trip exactly, and the empty case does NOT dereference a
// zero-length slice (which would panic on every empty upstream body).

func TestB2Str_RoundTripsContentIncludingNulAndUTF8(t *testing.T) {
	for _, b := range [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1}`),
		{0x00, 0x01, 0xff},
		[]byte("héllo ☃"),
		{' '},
	} {
		require.Equal(t, string(b), B2Str(b))
	}
}

func TestB2Str_EmptyAndNilSlicesYieldEmptyString(t *testing.T) {
	require.Equal(t, "", B2Str(nil))
	require.Equal(t, "", B2Str([]byte{}))
	require.Equal(t, "", B2Str(make([]byte, 0, 16)))
}

func TestS2Bytes_RoundTripsContent(t *testing.T) {
	for _, s := range []string{`{"result":"0x1"}`, "héllo ☃", " "} {
		require.Equal(t, []byte(s), S2Bytes(s))
	}
}

func TestS2Bytes_EmptyStringYieldsNilNotAPanic(t *testing.T) {
	require.Nil(t, S2Bytes(""))
	require.Len(t, S2Bytes(""), 0)
}

func TestB2Str_S2Bytes_AreInverses(t *testing.T) {
	s := `{"jsonrpc":"2.0","result":[1,2,3]}`
	require.Equal(t, s, B2Str(S2Bytes(s)))
}

func TestStringToReaderCloser_ReadsThenClosesCleanly(t *testing.T) {
	rc := StringToReaderCloser(`{"id":1}`)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, `{"id":1}`, string(got))
	require.NoError(t, rc.Close(), "Close must be safe — callers defer it on every request")
	require.NoError(t, rc.Close(), "a second Close must not error either")
}

func TestStringToReaderCloser_EmptyStringReadsAsEOF(t *testing.T) {
	rc := StringToReaderCloser("")
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, rc.Close())
}
