package clients

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// A pool with no clients must return an error naming itself, not a nil
// http.Client. Every caller of GetClient uses the returned client straight
// away, so a silent nil turns a misconfigured pool into a nil dereference on
// the first request instead of a startup error an operator can read.
func TestProxyPool_GetClientOnAnEmptyPoolErrorsAndNamesThePool(t *testing.T) {
	p := &ProxyPool{ID: "pool-with-no-urls"}

	client, err := p.GetClient()
	require.Error(t, err, "an empty pool handed out a client")
	require.Nil(t, client)
	require.Contains(t, err.Error(), "pool-with-no-urls",
		"the error must name the pool so an operator can find the bad config")
}

// A populated pool must hand out every client in turn. A pool that always
// returned the same one would send all traffic through a single proxy while
// the config says otherwise — the whole point of a pool.
func TestProxyPool_GetClientRotatesThroughEveryClient(t *testing.T) {
	a, b, c := &http.Client{}, &http.Client{}, &http.Client{}
	p := &ProxyPool{ID: "pool1", clients: []*http.Client{a, b, c}}

	seen := map[*http.Client]int{}
	for i := 0; i < 9; i++ {
		got, err := p.GetClient()
		require.NoError(t, err)
		seen[got]++
	}
	require.Len(t, seen, 3, "the pool did not rotate through every client")
	for cl, n := range seen {
		require.Equal(t, 3, n, "client %p was picked %d times, want 3", cl, n)
	}
}

// An empty pool at TRACE level must fall back to the direct client, not die.
//
// getHttpClient used to log before it checked the error. GetClient answers
// (nil, err) for an empty pool, so the trace line dereferenced a nil
// *http.Client and the request goroutine panicked — a fault that appeared only
// when someone raised the log level to look at it.
//
// The two sub-tests measure trace against a quieter level. Both must survive,
// or the log level still decides whether the process lives.
func TestGetHttpClient_AnEmptyPoolFallsBackAtEveryLogLevel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		trace bool
	}{
		{"AtTraceLevel", true},
		{"BelowTraceLevel", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger := zerolog.New(io.Discard)
			fallback := &http.Client{}
			c := &GenericHttpJsonRpcClient{
				logger:          &logger,
				httpClient:      fallback,
				proxyPool:       &ProxyPool{ID: "pool-with-no-urls"},
				isLogLevelTrace: tc.trace,
			}

			var got *http.Client
			require.NotPanics(t, func() { got = c.getHttpClient() },
				"the log level must not decide whether the request survives")
			require.Same(t, fallback, got,
				"an unusable pool must fall back to the direct client")
		})
	}
}

// proxyLabel answers "" for everything it cannot read. It feeds a trace line,
// and a label must never be able to kill the request it describes.
func TestProxyLabel_AnswersEmptyForWhatItCannotRead(t *testing.T) {
	proxied, err := url.Parse("http://proxy.example:8080")
	require.NoError(t, err)

	for _, tc := range []struct {
		name   string
		client *http.Client
		want   string
	}{
		{
			// The ordinary case, and the only one the pool itself builds.
			name:   "APooledClientNamesItsProxy",
			client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxied)}},
			want:   "http://proxy.example:8080",
		},
		{
			// Proxy returning (nil, nil) means "send this one direct". That is
			// an ordinary answer, and it used to be dereferenced.
			name:   "ADirectRouteIsNotAnError",
			client: &http.Client{Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }}},
			want:   "",
		},
		{
			name:   "ATransportWithNoProxySet",
			client: &http.Client{Transport: &http.Transport{}},
			want:   "",
		},
		{
			// The type assertion used to be bare, so any other RoundTripper
			// panicked.
			name:   "ARoundTripperThatIsNotAnHttpTransport",
			client: &http.Client{Transport: roundTripperFunc(nil)},
			want:   "",
		},
		{
			name:   "AProxyThatErrors",
			client: &http.Client{Transport: &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return nil, errors.New("no") }}},
			want:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			require.NotPanics(t, func() { got = proxyLabel(tc.client) })
			require.Equal(t, tc.want, got)
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
