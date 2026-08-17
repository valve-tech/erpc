package clients

import (
	"net/http"
	"testing"

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
