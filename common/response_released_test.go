package common

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Release() frees a NormalizedResponse while other layers may still read it.
// Before ErrResponseReleased the three readers below answered a released
// response exactly like an absent one, so a caller could not tell "there is no
// response" from "the response was freed under me". These tests measure the
// two states against each other at every reader.

func releasedTestResponse(t *testing.T, body string) *NormalizedResponse {
	t.Helper()
	rc := &countingCloser{Reader: strings.NewReader(body)}
	return NewNormalizedResponse().WithBody(rc).WithExpectedSize(len(body))
}

const releasedTestBody = `{"jsonrpc":"2.0","id":1,"result":"0x1"}`

// A nil receiver and a released receiver ask different questions, so they must
// get different answers. The nil case keeps answering (nil, nil): the request
// path calls JsonRpcResponse on the result of a failed attempt, where a nil
// response means "no attempt produced one" and is not an error.
func TestJsonRpcResponse_NilAndReleasedAnswerDifferently(t *testing.T) {
	var nilResp *NormalizedResponse
	jrr, err := nilResp.JsonRpcResponse()
	require.Nil(t, jrr, "a nil response still carries no JSON-RPC response")
	require.NoError(t, err, "a nil response is not an error, and this must not change")

	released := releasedTestResponse(t, releasedTestBody)
	_, err = released.JsonRpcResponse()
	require.NoError(t, err, "the first parse must succeed before the release")
	released.Release()

	jrr, err = released.JsonRpcResponse()
	require.Nil(t, jrr)
	require.ErrorIs(t, err, ErrResponseReleased,
		"a released response must name the release, not read as an absent one")
}

// A live response answers its parsed body with no error. This is the case the
// released cases are measured against: if it ever reported ErrResponseReleased,
// the flag would be set too early and every normal read would fail.
func TestJsonRpcResponse_LiveResponseParsesWithoutError(t *testing.T) {
	r := releasedTestResponse(t, releasedTestBody)

	jrr, err := r.JsonRpcResponse()
	require.NoError(t, err)
	require.NotNil(t, jrr)
	require.NotErrorIs(t, err, ErrResponseReleased)

	// A second read hits the cached-pointer fast path and must answer the same.
	again, err := r.JsonRpcResponse()
	require.NoError(t, err)
	require.Same(t, jrr, again, "the fast path must return the cached pointer")
}

// Entry 76 recorded that the same object answered differently depending on WHEN
// the caller released it. A release AFTER the first parse produced a silent
// (nil, nil), because parseOnce had already run. A release BEFORE any parse left
// parseOnce armed, so the next read parsed a nil body and got a JSON-RPC
// "no body available to parse" error inside a non-nil JsonRpcResponse.
//
// The released flag now answers first, so both orders report ErrResponseReleased
// and neither one parses a body that Release() already closed.
func TestJsonRpcResponse_ReleaseBeforeAndAfterParseAgree(t *testing.T) {
	afterParse := releasedTestResponse(t, releasedTestBody)
	_, err := afterParse.JsonRpcResponse()
	require.NoError(t, err)
	afterParse.Release()

	beforeParse := releasedTestResponse(t, releasedTestBody)
	beforeParse.Release()

	jrrAfter, errAfter := afterParse.JsonRpcResponse()
	jrrBefore, errBefore := beforeParse.JsonRpcResponse()

	require.ErrorIs(t, errAfter, ErrResponseReleased)
	require.ErrorIs(t, errBefore, ErrResponseReleased)
	require.Nil(t, jrrAfter)
	require.Nil(t, jrrBefore,
		"a release before the first parse must not parse the closed body")
}

// MarshalJSON returned (nil, nil) for a released response, so a marshaller wrote
// an empty body and reported success. It must name the release instead. An empty
// but live response keeps its own answer.
func TestMarshalJSON_ReleasedAndEmptyAnswerDifferently(t *testing.T) {
	live := releasedTestResponse(t, releasedTestBody)
	empty, err := live.MarshalJSON()
	require.NoError(t, err, "an unparsed live response is empty, not an error")
	require.Nil(t, empty)

	_, err = live.JsonRpcResponse()
	require.NoError(t, err)
	_, err = live.MarshalJSON()
	// JsonRpcResponse refuses MarshalJSON on purpose: it would buffer the whole
	// body in memory. The point here is that a parsed response reports THAT
	// refusal, and never the released sentinel.
	require.ErrorContains(t, err, "MarshalJSON must not be used on JsonRpcResponse")
	require.NotErrorIs(t, err, ErrResponseReleased)

	live.Release()
	out, err := live.MarshalJSON()
	require.Nil(t, out)
	require.ErrorIs(t, err, ErrResponseReleased,
		"a released response must name the release, not marshal to nothing")
}

// WriteTo reported a generic "unexpected empty response" for a released
// response, which sent a reader hunting for a missing body instead of the
// release that freed it. The two states must report different errors.
func TestWriteTo_ReleasedAndEmptyReportDifferentErrors(t *testing.T) {
	empty := releasedTestResponse(t, releasedTestBody)
	var buf bytes.Buffer
	_, err := empty.WriteTo(&buf)
	require.Error(t, err, "an unparsed response has nothing to write")
	require.NotErrorIs(t, err, ErrResponseReleased,
		"an empty live response is not a released one")

	live := releasedTestResponse(t, releasedTestBody)
	_, err = live.JsonRpcResponse()
	require.NoError(t, err)
	buf.Reset()
	n, err := live.WriteTo(&buf)
	require.NoError(t, err)
	require.Greater(t, n, int64(0), "a parsed live response writes its body")

	live.Release()
	buf.Reset()
	n, err = live.WriteTo(&buf)
	require.Zero(t, n)
	require.ErrorIs(t, err, ErrResponseReleased)
}

// IsObjectNull drops the error on purpose (`jrr, _ :=`). The sentinel must not
// change what it answers: a released response still has no object, so it stays
// true, exactly as it was when the read returned (nil, nil).
func TestIsObjectNull_UnaffectedByTheReleasedSentinel(t *testing.T) {
	var nilResp *NormalizedResponse
	require.True(t, nilResp.IsObjectNull())

	live := releasedTestResponse(t, releasedTestBody)
	_, err := live.JsonRpcResponse()
	require.NoError(t, err)
	require.False(t, live.IsObjectNull(), "a parsed live response carries an object")

	live.Release()
	require.True(t, live.IsObjectNull(), "a released response carries no object")
}

// Release is idempotent, and so is the flag it sets. A second Release must not
// change the answer a reader gets.
func TestJsonRpcResponse_SecondReleaseKeepsTheSameAnswer(t *testing.T) {
	r := releasedTestResponse(t, releasedTestBody)
	_, err := r.JsonRpcResponse()
	require.NoError(t, err)

	r.Release()
	_, firstErr := r.JsonRpcResponse()
	r.Release()
	_, secondErr := r.JsonRpcResponse()

	require.ErrorIs(t, firstErr, ErrResponseReleased)
	require.ErrorIs(t, secondErr, ErrResponseReleased)
	require.True(t, errors.Is(firstErr, secondErr), "both reads report one sentinel")
}
