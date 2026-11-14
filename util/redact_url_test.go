package util

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const urlTestSecret = "SENTINEL_KEY_123"

func TestRedactURLString(t *testing.T) {
	cases := []struct{ in, want string }{
		// The credential in the path, the query, the userinfo, the fragment.
		{"https://mainnet.infura.io/v3/" + urlTestSecret, "https://mainnet.infura.io/***"},
		{"https://lb.drpc.org/ogrpc?network=ethereum&dkey=" + urlTestSecret, "https://lb.drpc.org/***"},
		{"https://user:" + urlTestSecret + "@rpc.example.com", "https://rpc.example.com/***"},
		{"wss://rpc.example.com/ws#" + urlTestSecret, "wss://rpc.example.com/***"},
		{"https://rpc.example.com?" + urlTestSecret, "https://rpc.example.com/***"},
		// The port is part of the host and stays.
		{"http://10.0.0.1:8545/" + urlTestSecret, "http://10.0.0.1:8545/***"},
		{"grpc+bds://bds.example.com:443/" + urlTestSecret, "grpc+bds://bds.example.com:443/***"},
		// Nothing to hide: no placeholder.
		{"https://rpc.example.com", "https://rpc.example.com"},
		{"https://rpc.example.com/", "https://rpc.example.com"},
		// Vendor shorthands carry the key in the host. An unknown scheme is
		// the safe fallthrough: hide the host too.
		{"infura://" + urlTestSecret, "infura://***"},
		{"evm+alchemy://" + urlTestSecret, "evm+alchemy://***"},
		{"somefuturevendor://" + urlTestSecret + "/x", "somefuturevendor://***"},
		// Unparseable input gives no host to trust.
		{"https://rpc.example.com/v3/" + urlTestSecret + "\x7f", "https://***"},
		{"not a url " + urlTestSecret, "***"},
	}
	for _, c := range cases {
		got := RedactURLString(c.in)
		assert.Equal(t, c.want, got, "input %q", c.in)
		assert.NotContains(t, got, urlTestSecret)
	}
}

func TestRedactURL_Nil(t *testing.T) {
	assert.Equal(t, "", RedactURL(nil))
}

func TestRedactURLsInText(t *testing.T) {
	in := `Post "https://mainnet.infura.io/v3/` + urlTestSecret + `": dial tcp: connect: connection refused` +
		` (via proxy http://u:` + urlTestSecret + `@proxy.example.com:3128)`
	got := RedactURLsInText(in)
	assert.NotContains(t, got, urlTestSecret)
	assert.Contains(t, got, `Post "https://mainnet.infura.io/***": dial tcp: connect: connection refused`)
	assert.Contains(t, got, "http://proxy.example.com:3128/***")

	// Text with no URL comes back as the same string.
	plain := "dial tcp 127.0.0.1:1: connect: connection refused"
	assert.Equal(t, plain, RedactURLsInText(plain))
}

func TestRedactURLError(t *testing.T) {
	assert.Nil(t, RedactURLError(nil))

	// A message with no URL comes back as the same error value.
	plain := errors.New("connection reset by peer")
	assert.Same(t, plain, RedactURLError(plain))

	// The *url.Error shape net/http returns, wrapped once more as callers do.
	raw := "https://mainnet.infura.io/v3/" + urlTestSecret
	ue := &url.Error{Op: "Post", URL: raw, Err: context.DeadlineExceeded}
	wrapped := fmt.Errorf("send: %w", ue)
	red := RedactURLError(wrapped)
	assert.NotContains(t, red.Error(), urlTestSecret)
	assert.Contains(t, red.Error(), `"https://mainnet.infura.io/***"`)
	require.True(t, errors.Is(red, context.DeadlineExceeded), "errors.Is must still reach the sentinel")
	var got *url.Error
	require.True(t, errors.As(red, &got))
	assert.Same(t, ue, got, "errors.As must reach the original *url.Error")
	// Redacting twice changes nothing.
	assert.Same(t, red, RedactURLError(red))

	// url.Parse quotes the unparsed input, which may hold characters that end
	// a text match early. The URL field is replaced whole.
	perr := parseErr("https://mainnet.infura.io/v3/" + urlTestSecret + " tail\x7f")
	require.Error(t, perr)
	assert.NotContains(t, RedactURLError(perr).Error(), urlTestSecret)
	assert.NotContains(t, RedactURLError(perr).Error(), "tail")

	// A known URL is replaced even where the text scan has no scheme to see.
	known, _ := url.Parse(raw)
	odd := errors.New("upstream at <" + raw + "> failed")
	assert.NotContains(t, RedactURLError(odd, known).Error(), urlTestSecret)
}

// parseErr returns url.Parse's error. The input is invalid on purpose; the
// helper keeps staticcheck from flagging a constant bad URL (SA1007).
func parseErr(raw string) error {
	_, err := url.Parse(raw)
	return err
}

func TestRedactHeaders(t *testing.T) {
	in := map[string][]string{
		"Authorization":       {"Bearer " + urlTestSecret},
		"Proxy-Authorization": {"Basic " + urlTestSecret},
		"Cookie":              {"a=" + urlTestSecret},
		"X-Api-Key":           {urlTestSecret},
		"X-Goog-Api-Key":      {urlTestSecret},
		"X-Session-Token":     {urlTestSecret, urlTestSecret},
		"X-Client-Secret":     {urlTestSecret},
		"X-Auth":              {urlTestSecret},
		"Content-Type":        {"application/json"},
		"User-Agent":          {"erpc"},
	}
	out := RedactHeaders(in)
	for k, vs := range out {
		for _, v := range vs {
			assert.NotContains(t, v, urlTestSecret, "header %s", k)
		}
	}
	assert.Equal(t, []string{RedactedURLPlaceholder, RedactedURLPlaceholder}, out["X-Session-Token"])
	// Controls: harmless headers pass, every name survives, the input is untouched.
	assert.Equal(t, []string{"application/json"}, out["Content-Type"])
	assert.Equal(t, []string{"erpc"}, out["User-Agent"])
	assert.Len(t, out, len(in))
	assert.Equal(t, "Bearer "+urlTestSecret, in["Authorization"][0])
	assert.Nil(t, RedactHeaders(nil))
}
