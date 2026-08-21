package util

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// ExtractUsefulHeaders decides which upstream response headers eRPC copies
// into logs and error details. It is an allow-list by shape. Two failures
// matter to an operator: dropping the trace/rate-limit headers they need to
// diagnose an upstream, and copying a credential into a log line.

func TestExtractUsefulHeaders_KeepsTheDiagnosticHeaders(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	want := map[string]string{
		"X-Ratelimit-Remaining": "42",
		"Traceparent":           "00-abc-def-01",
		"X-Debug-Info":          "slow-path",
		"X-Correlation-Id":      "corr-1",
		"X-Request-Id":          "req-1",
		"X-Amzn-Trace-Id":       "root=1",
		"Content-Type":          "application/json",
		"Content-Length":        "17",
		"Server":                "nginx",
		"Date":                  "Mon, 01 Jan 2035 00:00:00 GMT",
		"Etag":                  "W/\"1\"",
		"Retry-After":           "5",
		"Cache-Control":         "no-store",
	}
	for k, v := range want {
		resp.Header.Set(k, v)
	}

	got := ExtractUsefulHeaders(resp)
	for k, v := range want {
		require.Equal(t, v, got[canonLower(k)],
			"header %q must survive extraction — operators diagnose upstreams with it", k)
	}
}

func TestExtractUsefulHeaders_DropsCredentialsAndCookies(t *testing.T) {
	// These carry secrets. Copying them into an error detail publishes the
	// operator's upstream credential to every log sink.
	resp := &http.Response{Header: http.Header{}}
	for _, k := range []string{"Authorization", "Proxy-Authorization", "Set-Cookie", "Cookie", "Www-Authenticate"} {
		resp.Header.Set(k, "SECRET-VALUE")
	}
	got := ExtractUsefulHeaders(resp)
	require.Empty(t, got, "no credential header may be copied, got %v", got)
}

func TestExtractUsefulHeaders_KeysAreLowercased(t *testing.T) {
	// Callers index the map by lowercase name. If extraction kept Go's
	// canonical casing, every lookup would miss and the diagnostics would
	// read as "upstream sent no headers".
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("X-Ratelimit-Reset", "9")
	got := ExtractUsefulHeaders(resp)
	require.Equal(t, "9", got["x-ratelimit-reset"])
	require.NotContains(t, got, "X-Ratelimit-Reset")
}

func TestExtractUsefulHeaders_EmptyResponseYieldsEmptyMap(t *testing.T) {
	// A nil map here would panic the caller that ranges over it.
	got := ExtractUsefulHeaders(&http.Response{Header: http.Header{}})
	require.NotNil(t, got)
	require.Empty(t, got)
}

func canonLower(k string) string {
	b := []byte(k)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
