package valverelay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpBackend POSTs to an eRPC over HTTP, which is what makes the relay a
// separate process from the node it bills for.
type httpBackend struct {
	base      string
	projectID string
	client    *http.Client
}

// NewHTTPBackend forwards to {baseURL}/{projectID}/evm/{chainID}.
//
// timeout bounds the whole round trip and is required. A relay whose upstream
// hangs must fail the request, not hold a customer's connection and a
// goroutine until the process is restarted.
func NewHTTPBackend(baseURL, projectID string, timeout time.Duration) (Backend, error) {
	if projectID == "" {
		return nil, fmt.Errorf("valverelay: http backend needs a project id")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("valverelay: http backend needs a positive timeout")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("valverelay: %q is not a valid base URL: %w", baseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("valverelay: base URL %q needs a scheme and a host", baseURL)
	}
	return &httpBackend{
		base:      strings.TrimSuffix(baseURL, "/"),
		projectID: projectID,
		client:    &http.Client{Timeout: timeout},
	}, nil
}

func (b *httpBackend) Forward(ctx context.Context, chainID int64, body []byte) ([]byte, error) {
	endpoint := fmt.Sprintf("%s/%s/evm/%d", b.base, b.projectID, chainID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("valverelay: building the request for chain %d: %w", chainID, err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Ask for an uncompressed answer on this hop.
	//
	// Go's transport adds "Accept-Encoding: gzip" on its own and transparently
	// inflates the result, so without this header eRPC gzips a body the relay
	// decompresses a microsecond later on the same box. The monorepo measured
	// that: identity on this hop saved 0.78 cores. Setting the header
	// explicitly also switches off the transparent inflate, so what arrives is
	// what eRPC wrote.
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("valverelay: forwarding chain %d: %w", chainID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("valverelay: reading the answer for chain %d: %w", chainID, err)
	}

	// eRPC answers 200 for JSON-RPC application errors and reserves non-2xx
	// for transport-level faults — auth, rate limits, an unknown network, a
	// missing project (determineResponseStatusCode in erpc/http_server.go).
	// So 2xx is the closest observable stand-in for "an answer was produced",
	// and a non-2xx status is charged to nobody.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("valverelay: chain %d answered HTTP %d: %s",
			chainID, resp.StatusCode, snippet(out))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("valverelay: chain %d answered HTTP %d with an empty body", chainID, resp.StatusCode)
	}
	return out, nil
}

// Close drops idle connections. The client itself needs no shutdown.
func (b *httpBackend) Close() error {
	b.client.CloseIdleConnections()
	return nil
}

// snippet bounds an error message that would otherwise carry a whole response
// body into the logs.
func snippet(b []byte) string {
	const max = 256
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
