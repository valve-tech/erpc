package erpc

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

// RFC 7239 defines one standard forwarding header, `Forwarded`, and writes the
// client as `for=192.0.2.60`. eRPC accepts any header name the operator
// configures, so it must read the value that name carries — on BOTH surfaces.
// Supporting it over HTTP and not over gRPC leaves the same defect reachable
// through a different door. See entries 30 and 133 in
// valve/upstream-bug-log.md.

// grpcServerFrom builds a GrpcServer with the same trusted-forwarder shape the
// HTTP tests use, so the two surfaces are compared on equal terms.
func grpcServerFrom(headers []string) *GrpcServer {
	return &GrpcServer{
		trustedForwarderIPs: map[string]struct{}{"10.0.0.1": {}},
		trustedIPHeaders:    headers,
	}
}

// grpcCtxFrom presents the peer address gRPC would report for the accepted
// connection.
func grpcCtxFrom(addr string) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP(addr), Port: 51000},
	})
}

func TestGrpcClientIP_ReadsAnRfc7239ForwardedHeader(t *testing.T) {
	gs := grpcServerFrom([]string{"forwarded"})
	md := metadata.New(map[string]string{"forwarded": `for=203.0.113.7;proto=https`})

	assert.Equal(t, "203.0.113.7", gs.grpcClientIP(grpcCtxFrom("10.0.0.1"), md),
		"an operator who configures the standard header must get the client, not the edge")
}

func TestResolveRealClientIP_ReadsAnRfc7239ForwardedHeader(t *testing.T) {
	srv := newClientIPServer(t, []string{"10.0.0.0/8"}, []string{"Forwarded"})

	got := srv.resolveRealClientIP(requestFrom("10.0.0.1:51000", map[string]string{
		"Forwarded": `for=203.0.113.7;proto=https`,
	}))

	require.Equal(t, "203.0.113.7", got)
}

// TestForwardedChain_TakesTheNearestHopNoForwarderVouchedFor is the property
// that matters for both syntaxes: a multi-hop chain must still resolve to the
// nearest address the configured forwarders did not vouch for.
func TestForwardedChain_TakesTheNearestHopNoForwarderVouchedFor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"XForwardedForSyntax", "203.0.113.7, 10.0.0.5, 10.0.0.1"},
		{"Rfc7239Syntax", `for=203.0.113.7, for=10.0.0.5;proto=https, for=10.0.0.1`},
		// A quoted IPv6 literal with a port is the shape RFC 7239 mandates for
		// IPv6, and the one an operator is most likely to hit.
		{"Rfc7239QuotedIpv6", `for="[2001:db8::7]:4711", for=10.0.0.1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newClientIPServer(t, []string{"10.0.0.0/8"}, []string{"Forwarded"})
			got := srv.resolveRealClientIP(requestFrom("10.0.0.1:51000", map[string]string{
				"Forwarded": tc.value,
			}))
			want := "203.0.113.7"
			if tc.name == "Rfc7239QuotedIpv6" {
				want = "2001:db8::7"
			}
			assert.Equal(t, want, got)
		})
	}
}

// TestForwardedChain_AnUnparseableValueStillFallsBackToThePeer is the safety
// counterweight. RFC 7239 lets a proxy write an obfuscated identifier instead
// of an address; eRPC must attribute the request to the peer rather than
// inventing a client.
func TestForwardedChain_AnUnparseableValueStillFallsBackToThePeer(t *testing.T) {
	srv := newClientIPServer(t, []string{"10.0.0.0/8"}, []string{"Forwarded"})

	got := srv.resolveRealClientIP(requestFrom("10.0.0.1:51000", map[string]string{
		"Forwarded": `for=_hidden;proto=https`,
	}))

	require.Equal(t, "10.0.0.1", got,
		"an obfuscated identifier names no address, so the peer stands")
}

// TestGrpcClientIP_IgnoresAnRfc7239HeaderFromAnUntrustedPeer keeps the spoofing
// guard in place for the new syntax: reading a standard header must not become
// a way around the trusted-forwarder check.
func TestGrpcClientIP_IgnoresAnRfc7239HeaderFromAnUntrustedPeer(t *testing.T) {
	gs := grpcServerFrom([]string{"forwarded"})
	md := metadata.New(map[string]string{"forwarded": `for=203.0.113.7`})

	assert.Equal(t, "198.51.100.25", gs.grpcClientIP(grpcCtxFrom("198.51.100.25"), md),
		"a direct caller claiming to be someone else must be billed as itself")
}
