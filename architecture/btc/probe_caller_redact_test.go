package btc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A bitcoind endpoint carries basic-auth credentials, and a vendor endpoint a
// key in the path. A probe timeout must name neither.
func TestHttpProbeCaller_TimeoutRedactsEndpoint(t *testing.T) {
	const sentinel = "SENTINEL_KEY_123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}
	defer tr.CloseIdleConnections()
	// The real Client.Timeout: a *url.Error that wraps a deadline.
	client := &http.Client{Transport: tr, Timeout: 100 * time.Millisecond}

	f := &Family{}
	caller := f.NewProbeCaller("http://rpcuser:"+sentinel+"@example.invalid:8332/wallet/"+sentinel, client)

	_, err := caller.CallJsonRpc(context.Background(), "getblockchaininfo", nil)
	if err == nil {
		t.Fatal("the probe must fail on a timeout")
	}
	// Guard control: the raw error still carries the credential, so the
	// check below measures the redaction and not an error without a URL.
	var ue *url.Error
	if !errors.As(err, &ue) || !strings.Contains(ue.URL, "example.invalid") {
		t.Fatalf("the original *url.Error must stay reachable, got %T: %v", err, err)
	}
	if !ue.Timeout() {
		t.Fatalf("want a timeout, got %v", err)
	}

	// The same error as the family wraps it for the bootstrap log and the
	// startup error.
	probe := f.Probe(context.Background(), caller)
	for name, msg := range map[string]string{"CallJsonRpc": err.Error(), "Probe": probe.Err.Error()} {
		if strings.Contains(msg, sentinel) || strings.Contains(msg, "rpcuser") {
			t.Errorf("%s leaks the endpoint credential: %s", name, msg)
		}
		if !strings.Contains(msg, "http://example.invalid:8332/***") {
			t.Errorf("%s must still name the host: %s", name, msg)
		}
	}
}

// A URL net/http cannot use comes back as a parse error that quotes it whole.
func TestHttpProbeCaller_BuildErrorRedactsEndpoint(t *testing.T) {
	const sentinel = "SENTINEL_KEY_123"
	f := &Family{}
	caller := f.NewProbeCaller("http://example.invalid/"+sentinel+" \x7f", &http.Client{})
	_, err := caller.CallJsonRpc(context.Background(), "getblockchaininfo", nil)
	if err == nil {
		t.Fatal("the probe must fail on a bad URL")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("the build error leaks the endpoint credential: %s", err)
	}
	if !strings.Contains(err.Error(), "build request") {
		t.Errorf("control: want the build-request error, got %s", err)
	}
}
