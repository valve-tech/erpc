package erpc

import (
	"context"
	"net/http"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// The client IP eRPC settles on is the identity every per-IP rate limit, every
// auth rule and every usage metric is keyed by. Get it wrong in one direction
// and one shared proxy address absorbs the whole fleet's budget; get it wrong in
// the other and any caller can spoof a header to reset its own limit. The rules
// below are the whole defence: a forwarding header is read ONLY when the
// immediate peer is a configured forwarder, and only the nearest hop the
// forwarders did not vouch for is taken as the client.

// newClientIPServer builds a real HttpServer through the production constructor
// so the trusted-forwarder parsing under test is the same code that runs at
// startup, not a hand-filled struct.
func newClientIPServer(t *testing.T, forwarders, headers []string) *HttpServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	logger := log.Logger
	srv, err := NewHttpServer(ctx, &logger, &common.ServerConfig{
		TrustedIPForwarders: forwarders,
		TrustedIPHeaders:    headers,
	}, nil, nil, nil, nil)
	require.NoError(t, err)
	return srv
}

// requestFrom builds a request as the Go server would present it: RemoteAddr is
// the peer eRPC actually accepted the TCP connection from, headers are whatever
// that peer chose to send.
func requestFrom(peer string, headers map[string]string) *http.Request {
	r := &http.Request{RemoteAddr: peer, Header: http.Header{}}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// TestResolveRealClientIP_IgnoresForwardingHeadersFromAnUntrustedPeer is the
// spoofing guard. Any caller can put any address in X-Forwarded-For, so eRPC
// must read it only from a peer the operator named as a forwarder.
func TestResolveRealClientIP_IgnoresForwardingHeadersFromAnUntrustedPeer(t *testing.T) {
	srv := newClientIPServer(t, []string{"10.0.0.0/8"}, []string{"X-Forwarded-For"})

	got := srv.resolveRealClientIP(requestFrom("203.0.113.9:51000", map[string]string{
		"X-Forwarded-For": "198.51.100.1",
	}))

	require.Equal(t, "203.0.113.9", got,
		"a direct caller claiming to be someone else must be billed as itself")
}

// TestResolveRealClientIP_TakesTheNearestHopTheForwardersDidNotVouchFor is the
// core selection rule. The header holds the whole chain; only the entry closest
// to eRPC that is NOT one of our own proxies is a client address we can trust
// the proxies to have observed. Taking the leftmost entry instead would take
// whatever the caller wrote; taking the rightmost would take our own proxy.
func TestResolveRealClientIP_TakesTheNearestHopTheForwardersDidNotVouchFor(t *testing.T) {
	srv := newClientIPServer(t, []string{"10.0.0.0/8"}, []string{"X-Forwarded-For"})

	got := srv.resolveRealClientIP(requestFrom("10.0.0.1:51000", map[string]string{
		// 198.51.100.9 is caller-supplied and unverified, 203.0.113.7 is what
		// our first proxy actually saw, 10.0.0.5 is that proxy itself.
		"X-Forwarded-For": "198.51.100.9, 203.0.113.7, 10.0.0.5",
	}))

	require.Equal(t, "203.0.113.7", got)
}

// TestResolveRealClientIP_FallsBackToThePeerWhenEveryHopIsOurOwnProxy covers a
// header that names only trusted addresses. There is no client address in it, so
// inventing one from the list would attribute the request to a proxy.
func TestResolveRealClientIP_FallsBackToThePeerWhenEveryHopIsOurOwnProxy(t *testing.T) {
	srv := newClientIPServer(t, []string{"10.0.0.0/8"}, []string{"X-Forwarded-For"})

	got := srv.resolveRealClientIP(requestFrom("10.0.0.1:51000", map[string]string{
		"X-Forwarded-For": "10.0.0.4, 10.0.0.5",
	}))

	require.Equal(t, "10.0.0.1", got)
}

// TestResolveRealClientIP_ReadsConfiguredHeadersInOrder pins precedence. An
// operator lists headers in the order their edge sets them; a server that
// scanned them in any other order would silently prefer the wrong edge.
func TestResolveRealClientIP_ReadsConfiguredHeadersInOrder(t *testing.T) {
	srv := newClientIPServer(t, []string{"10.0.0.0/8"},
		[]string{"X-Real-IP", "X-Forwarded-For"})

	got := srv.resolveRealClientIP(requestFrom("10.0.0.1:51000", map[string]string{
		"X-Real-IP":       "203.0.113.1",
		"X-Forwarded-For": "203.0.113.2",
	}))

	require.Equal(t, "203.0.113.1", got, "the first configured header wins")
}

// TestResolveRealClientIP_SkipsHeadersThatHoldNoUsableAddress covers the
// fallthrough between configured headers. An edge that sets a placeholder
// ("unknown" is common) must not stop the search and strand the request on the
// proxy address.
func TestResolveRealClientIP_SkipsHeadersThatHoldNoUsableAddress(t *testing.T) {
	srv := newClientIPServer(t, []string{"10.0.0.0/8"},
		[]string{"X-Real-IP", "X-Forwarded-For"})

	got := srv.resolveRealClientIP(requestFrom("10.0.0.1:51000", map[string]string{
		"X-Real-IP":       "unknown",
		"X-Forwarded-For": "203.0.113.2",
	}))

	require.Equal(t, "203.0.113.2", got)
}

// TestResolveRealClientIP_ReportsAnUnreadablePeerAsNotAvailable covers the one
// case with no address at all. "n/a" is a fixed sentinel the metric labels and
// rate-limit keys already expect; an empty string there would silently merge
// every such request into one bucket under a blank label.
func TestResolveRealClientIP_ReportsAnUnreadablePeerAsNotAvailable(t *testing.T) {
	srv := newClientIPServer(t, []string{"10.0.0.0/8"}, []string{"X-Forwarded-For"})

	require.Equal(t, "n/a", srv.resolveRealClientIP(requestFrom("", nil)))
	require.Equal(t, "n/a", srv.resolveRealClientIP(requestFrom("not-an-address", nil)))
}

// TestResolveRealClientIP_HandlesBracketedIPv6HopsAndPorts covers the address
// decorations real proxies emit. IPv6 hops arrive bracketed, and some proxies
// append the source port. Either one left in place fails net.ParseIP, and the
// hop is dropped from the chain — which quietly changes which hop is "nearest".
func TestResolveRealClientIP_HandlesBracketedIPv6HopsAndPorts(t *testing.T) {
	srv := newClientIPServer(t, []string{"2001:db8:aaaa::/48"}, []string{"X-Forwarded-For"})

	// Bare bracketed hops: only the bracket strip makes these parse at all, and
	// a dropped trusted hop changes which one counts as nearest.
	bare := srv.resolveRealClientIP(requestFrom("[2001:db8:aaaa::1]:51000", map[string]string{
		"X-Forwarded-For": "[2001:db8:cccc::9], [2001:db8:aaaa::5]",
	}))
	require.Equal(t, "2001:db8:cccc::9", bare)

	// The same chain with a port on the client hop, which some proxies append.
	withPort := srv.resolveRealClientIP(requestFrom("[2001:db8:aaaa::1]:51000", map[string]string{
		"X-Forwarded-For": "[2001:db8:cccc::9]:443, [2001:db8:aaaa::5]",
	}))
	require.Equal(t, "2001:db8:cccc::9", withPort)
}

// TestNewHttpServer_SortsTrustedForwardersAndDropsUnusableEntries covers the
// startup parse. A typo'd entry must be dropped rather than widened into
// something that matches: an operator who mistypes a CIDR should lose that one
// forwarder, not gain a trust rule they never wrote.
func TestNewHttpServer_SortsTrustedForwardersAndDropsUnusableEntries(t *testing.T) {
	srv := newClientIPServer(t, []string{
		"10.0.0.0/8",  // valid CIDR
		"192.0.2.1",   // valid single IP
		"  ",          // blank, ignored
		"not-an-ip",   // junk
		"300.0.0.0/8", // out-of-range CIDR
		"192.0.2.999", // out-of-range IP
	}, nil)

	require.Len(t, srv.trustedForwarderNets, 1, "only the one parseable CIDR is kept")
	require.Len(t, srv.trustedForwarderIPs, 1, "only the one parseable IP is kept")

	require.True(t, srv.isTrustedForwarder(parseRemoteIP("10.1.2.3:80")))
	require.True(t, srv.isTrustedForwarder(parseRemoteIP("192.0.2.1:80")))
	require.False(t, srv.isTrustedForwarder(parseRemoteIP("203.0.113.1:80")))
	require.False(t, srv.isTrustedForwarder(nil))
}

// TestNewHttpServer_TrimsAndDropsBlankTrustedIPHeaders covers the header list
// parse. A blank entry left in the list would make eRPC call Header.Get("") on
// every request, which always returns "" — harmless but it is the kind of silent
// no-op that hides a bad config from the operator who wrote it.
func TestNewHttpServer_TrimsAndDropsBlankTrustedIPHeaders(t *testing.T) {
	srv := newClientIPServer(t, []string{"10.0.0.0/8"},
		[]string{"  X-Real-IP  ", "", "   "})

	require.Equal(t, []string{"X-Real-IP"}, srv.trustedIPHeaders)

	// The trimmed name must be the one actually read off the wire.
	got := srv.resolveRealClientIP(requestFrom("10.0.0.1:51000", map[string]string{
		"X-Real-IP": "203.0.113.4",
	}))
	require.Equal(t, "203.0.113.4", got)
}

// TestResolveRealClientIP_DoesNotUnderstandTheRfc7239ForwardedHeader pins
// today's behaviour for an operator who configures `Forwarded` (RFC 7239)
// instead of `X-Forwarded-For`. resolveRealClientIP parses every configured
// header as a bare comma-separated IP list, so `for=203.0.113.7` never parses
// as an address and the request is attributed to the proxy instead of the
// client — every caller behind that edge shares one rate-limit bucket.
//
// http_server.go carries a parseForwardedFor that would handle this, but
// nothing calls it. This test fails the moment that changes, which is the point:
// the fix is to wire it up, and this expectation is what must then be updated.
func TestResolveRealClientIP_DoesNotUnderstandTheRfc7239ForwardedHeader(t *testing.T) {
	srv := newClientIPServer(t, []string{"10.0.0.0/8"}, []string{"Forwarded"})

	got := srv.resolveRealClientIP(requestFrom("10.0.0.1:51000", map[string]string{
		"Forwarded": `for=203.0.113.7;proto=https`,
	}))

	require.Equal(t, "10.0.0.1", got,
		"RFC 7239 syntax is not parsed today; see parseForwardedFor, which is unreachable")
}
