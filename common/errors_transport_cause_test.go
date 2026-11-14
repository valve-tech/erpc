package common

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The endpoint URL carries a credential. Every assertion below uses this exact
// key, so a leak on any surface fails the test.
const transportTestSecret = "SUPERSECRETAPIKEY"

func transportTestURL(t *testing.T) *url.URL {
	t.Helper()
	u, err := url.Parse("https://rpc.example.com/v1/" + transportTestSecret)
	require.NoError(t, err)
	return u
}

// TestErrEndpointTransportFailure_PreservesSentinelIdentity proves that the
// transport failure keeps the identity of the error the HTTP client returned.
// Before the fix the constructor rebuilt the cause with errors.New, so every
// errors.Is check below returned false.
func TestErrEndpointTransportFailure_PreservesSentinelIdentity(t *testing.T) {
	u := transportTestURL(t)

	cases := []struct {
		name     string
		sentinel error
	}{
		{"deadline exceeded", context.DeadlineExceeded},
		{"canceled", context.Canceled},
		{"eof", io.EOF},
		{"unexpected eof", io.ErrUnexpectedEOF},
		{"net closed", net.ErrClosed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Shape of a real net/http failure: the URL is inside the message.
			cause := fmt.Errorf("Post %q: %w", u.String(), tc.sentinel)
			err := NewErrEndpointTransportFailure(u, cause)

			require.True(t, errors.Is(err, tc.sentinel),
				"errors.Is must reach the sentinel through the transport failure")
		})
	}
}

// TestErrEndpointTransportFailure_PreservesConcreteType proves errors.As reaches
// the original concrete error object, not a copy.
func TestErrEndpointTransportFailure_PreservesConcreteType(t *testing.T) {
	u := transportTestURL(t)
	orig := &url.Error{Op: "Post", URL: u.String(), Err: context.DeadlineExceeded}

	err := NewErrEndpointTransportFailure(u, orig)

	var ue *url.Error
	require.True(t, errors.As(err, &ue), "errors.As must reach *url.Error")
	require.Same(t, orig, ue, "errors.As must reach the original object, not a copy")
	require.True(t, ue.Timeout(), "the caller must still be able to ask the original whether it timed out")
	require.True(t, errors.Is(err, context.DeadlineExceeded))
}

// TestErrEndpointTransportFailure_RedactsURLOnEverySurface checks each place the
// cause message can surface. Every check asserts the placeholder is PRESENT, so
// an empty or dropped message cannot pass the test by accident.
func TestErrEndpointTransportFailure_RedactsURLOnEverySurface(t *testing.T) {
	SetErrorLabelMode(ErrorLabelModeVerbose)
	t.Cleanup(func() { SetErrorLabelMode(ErrorLabelModeVerbose) })

	u := transportTestURL(t)
	cause := fmt.Errorf("Post %q: %w", u.String(), context.DeadlineExceeded)
	err := NewErrEndpointTransportFailure(u, cause)
	se, ok := err.(StandardError)
	require.True(t, ok)

	check := func(surface, rendered string) {
		t.Helper()
		assert.Contains(t, rendered, "https://rpc.example.com/"+RedactedEndpointPlaceholder,
			"%s must show the host with the redaction placeholder", surface)
		assert.NotContains(t, rendered, transportTestSecret,
			"%s must not leak the endpoint credential", surface)
		assert.NotContains(t, rendered, u.String(),
			"%s must not leak the endpoint URL", surface)
	}

	// Surface 1: Error() — used by logs, by %v, and by every outer BaseError.
	check("Error()", err.Error())

	// Surface 2: DeepestMessage() — used by the JSON-RPC response body and by
	// the metrics error label.
	check("DeepestMessage()", se.DeepestMessage())

	// Surface 3: ErrorSummary() — the metrics label value.
	check("ErrorSummary()", ErrorSummary(err))

	// Surface 4: JSON marshalling — the HTTP error response body.
	body, merr := SonicCfg.Marshal(err)
	require.NoError(t, merr)
	check("MarshalJSON()", string(body))

	// Surface 5: zerolog object marshalling — the structured log line.
	var buf bytes.Buffer
	prevLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)
	t.Cleanup(func() { zerolog.SetGlobalLevel(prevLevel) })
	lg := zerolog.New(&buf).Level(zerolog.DebugLevel)
	lg.Error().Object("err", se).Msg("transport failure")
	check("MarshalZerologObject()", buf.String())

	// Surface 6: an outer error that wraps this one, as the retry and
	// upstream layers do.
	outer := NewErrUpstreamRequest(err, nil, "evm:123", "eth_getLogs", 0, 0, 0, 0)
	check("wrapped Error()", outer.Error())
	check("wrapped DeepestMessage()", outer.(StandardError).DeepestMessage())
	check("wrapped ErrorSummary()", ErrorSummary(outer))
	outerBody, merr := SonicCfg.Marshal(outer)
	require.NoError(t, merr)
	check("wrapped MarshalJSON()", string(outerBody))
}

// TestErrEndpointTransportFailure_KeepsStandardErrorCauseUntouched locks in the
// existing behaviour: a StandardError cause already redacts itself, so the
// constructor must pass it through unchanged.
func TestErrEndpointTransportFailure_KeepsStandardErrorCauseUntouched(t *testing.T) {
	u := transportTestURL(t)
	inner := NewErrEndpointServerSideException(errors.New("boom"), nil, 500)

	err := NewErrEndpointTransportFailure(u, inner)

	require.Same(t, inner, err.(StandardError).GetCause(),
		"a StandardError cause must reach the transport failure unwrapped")
}

// TestErrEndpointTransportFailure_EmptyURLDoesNotCorruptMessage guards the
// degenerate case. strings.ReplaceAll with an empty needle inserts the
// replacement between every character, which would shred the message.
func TestErrEndpointTransportFailure_EmptyURLDoesNotCorruptMessage(t *testing.T) {
	err := NewErrEndpointTransportFailure(&url.URL{}, errors.New("dial fail"))

	msg := err.(StandardError).DeepestMessage()
	require.Equal(t, "dial fail", msg)
	require.False(t, strings.Contains(msg, RedactedEndpointPlaceholder))
}

// TestErrEndpointTransportFailure_NilCause keeps the nil path safe.
func TestErrEndpointTransportFailure_NilCause(t *testing.T) {
	u := transportTestURL(t)
	err := NewErrEndpointTransportFailure(u, nil)
	require.Nil(t, err.(StandardError).GetCause())
	require.NotContains(t, err.Error(), transportTestSecret)
}

// TestEndpointErrors_RedactURLErrorCause covers the constructors that take no
// URL. net/http reports its own Client.Timeout as a *url.Error that wraps
// context.DeadlineExceeded, so the timeout error received the full URL as its
// cause and printed the key.
func TestEndpointErrors_RedactURLErrorCause(t *testing.T) {
	u := transportTestURL(t)
	ctors := map[string]func(error) error{
		"timeout":  func(c error) error { return NewErrEndpointRequestTimeout(0, c) },
		"canceled": NewErrEndpointRequestCanceled,
		"transport (redirect target differs from the endpoint)": func(c error) error {
			other, _ := url.Parse("https://other.example.com/")
			return NewErrEndpointTransportFailure(other, c)
		},
	}
	for name, ctor := range ctors {
		t.Run(name, func(t *testing.T) {
			orig := &url.Error{Op: "Post", URL: u.String(), Err: context.DeadlineExceeded}
			err := ctor(orig)

			var ue *url.Error
			require.True(t, errors.As(err, &ue))
			require.Same(t, orig, ue, "errors.As must reach the original *url.Error")
			require.True(t, errors.Is(err, context.DeadlineExceeded))

			body, merr := SonicCfg.Marshal(err)
			require.NoError(t, merr)
			for surface, s := range map[string]string{
				"Error()":        err.Error(),
				"DeepestMessage": err.(StandardError).DeepestMessage(),
				"ErrorSummary":   ErrorSummary(err),
				"MarshalJSON":    string(body),
			} {
				assert.NotContains(t, s, transportTestSecret, "%s leaks the key", surface)
			}
			assert.Contains(t, err.Error(), "https://rpc.example.com/"+RedactedEndpointPlaceholder)
		})
	}
}

// TestErrEndpointTransportFailure_RedactsUserinfoEndpoint covers an endpoint
// with a password. net/http prints the request URL with the password masked,
// so the masked string never matched the configured URL and the old
// replace-the-known-URL redaction let the path key and the user name through.
func TestErrEndpointTransportFailure_RedactsUserinfoEndpoint(t *testing.T) {
	u, err := url.Parse("https://alice:hunter2@rpc.example.com/v1/" + transportTestSecret)
	require.NoError(t, err)
	masked := "https://alice:***@rpc.example.com/v1/" + transportTestSecret
	cause := &url.Error{Op: "Post", URL: masked, Err: io.EOF}

	msg := NewErrEndpointTransportFailure(u, cause).Error()
	for _, leak := range []string{transportTestSecret, "alice", "hunter2"} {
		assert.NotContains(t, msg, leak)
	}
	assert.Contains(t, msg, "https://rpc.example.com/"+RedactedEndpointPlaceholder)
}
