package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/h2non/gock"
	"github.com/stretchr/testify/require"
)

// A batch POSTed to /<project> — no architecture or chain in the path — carries
// its target network per entry, as "networkId":"evm:<chain>" in each request
// object. Entries are free to name different networks: that is the only way to
// ask one endpoint for two chains at once, and it is why the per-entry field
// exists at all.
//
// Every entry was answered from ONE network regardless of what it asked for.
// The batch fans out into a goroutine per entry, and all of them shared the
// request-scoped `architecture`/`chainId` pair: the first entry to parse its
// networkId wrote that pair, and every entry that read it afterwards took the
// "already resolved" branch and inherited the winner's network instead of
// resolving its own. A read/write race on shared strings, so which network won
// was down to scheduling — the visible result was usually the whole batch
// coming back from one entry's chain.
//
// Routing is asserted by result value: the two upstreams are mocked to return
// distinguishable balances, so an entry answered from the wrong chain returns
// the wrong number rather than merely being labelled wrong.
func TestBatchRoutesEachEntryToItsOwnNetwork(t *testing.T) {
	util.ResetGock()
	defer util.ResetGock()
	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		// Loopback is the server under test and anything it fronts; the
		// rpc*.localhost upstreams below are what gock is here to mock. This
		// filter is global and not load-bearing for the test's own request —
		// see postBatch, which sidesteps gock entirely.
		host := strings.Split(req.URL.Host, ":")[0]
		return host == "127.0.0.1" || host == "localhost"
	})

	// chain 123 lives on rpc1 and answers 0x1111; chain 456 lives on rpc2 and
	// answers 0x2222. eth_chainId has to agree with the configured chain or the
	// upstream is rejected before any of this is reachable.
	mockChain(t, "http://rpc1.localhost", "0x7b", "0x1111")
	mockChain(t, "http://rpc2.localhost", "0x1c8", "0x2222")

	cfg := &common.Config{
		Server: &common.ServerConfig{ListenV4: util.BoolPtr(true)},
		Projects: []*common.ProjectConfig{
			{
				Id: "test_project",
				Networks: []*common.NetworkConfig{
					{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 123}},
					{Architecture: common.ArchitectureEvm, Evm: &common.EvmNetworkConfig{ChainId: 456}},
				},
				Upstreams: []*common.UpstreamConfig{
					{
						Id:       "up-123",
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc1.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 123},
					},
					{
						Id:       "up-456",
						Type:     common.UpstreamTypeEvm,
						Endpoint: "http://rpc2.localhost",
						Evm:      &common.EvmUpstreamConfig{ChainId: 456},
					},
				},
			},
		},
		RateLimiters: &common.RateLimiterConfig{},
	}

	_, _, baseURL, cleanup, _ := createServerTestFixtures(cfg, t)
	defer cleanup()

	// Alternating networks over a batch wide enough that the entries cannot all
	// read the shared pair before the first one writes it, repeated because
	// losing the race is a matter of scheduling. Against the unfixed server
	// this reports the wrong balance within a couple of rounds; the race
	// detector (go test -race) catches the same defect on the first request,
	// and is the authoritative check.
	const entries = 12
	const rounds = 5

	expected := map[float64]string{}
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < entries; i++ {
		chain, balance := "123", "0x1111"
		if i%2 == 1 {
			chain, balance = "456", "0x2222"
		}
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"jsonrpc":"2.0","id":%d,"method":"eth_getBalance",`+
			`"params":["0x0000000000000000000000000000000000000000","latest"],`+
			`"networkId":"evm:%s"}`, i+1, chain)
		expected[float64(i+1)] = balance
	}
	sb.WriteString("]")
	batch := sb.String()

	for round := 0; round < rounds; round++ {
		body := postBatch(t, baseURL+"/test_project", batch)

		var responses []map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(body), &responses), "batch response: %s", body)
		require.Len(t, responses, entries, "batch response: %s", body)

		// id is the only reliable way to pair a batch response with its
		// request; the spec does not promise order.
		byId := map[float64]map[string]interface{}{}
		for _, resp := range responses {
			id, ok := resp["id"].(float64)
			require.True(t, ok, "response without a usable id: %v", resp)
			byId[id] = resp
		}

		for id, want := range expected {
			resp, ok := byId[id]
			require.True(t, ok, "no response for id %v in: %s", id, body)
			require.Nil(t, resp["error"], "id %v returned an error: %v", id, resp["error"])
			require.Equalf(t, want, resp["result"],
				"round %d: id %v was answered from the wrong network (full batch: %s)",
				round, id, body)
		}
	}
}

// mockChain stands up the state-poller methods an upstream must answer before
// it is usable, plus an eth_getBalance whose result identifies the chain.
func mockChain(t *testing.T, endpoint, chainIdHex, balance string) {
	t.Helper()

	reply := func(match string, result string) {
		gock.New(endpoint).
			Post("").
			Persist().
			Filter(func(request *http.Request) bool {
				return strings.Contains(util.SafeReadBody(request), match)
			}).
			Reply(200).
			JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":` + result + `}`))
	}

	reply("eth_chainId", `"`+chainIdHex+`"`)
	reply("eth_syncing", `false`)
	reply("eth_getBalance", `"`+balance+`"`)
	gock.New(endpoint).
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(util.SafeReadBody(request), "eth_getBlockByNumber")
		}).
		Reply(200).
		JSON([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"0x11118888","timestamp":"0x6702a8f0"}}`))
}

func postBatch(t *testing.T, url, batch string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(batch))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	// Its own transport, deliberately. gock swaps out http.DefaultTransport
	// process-wide, and its networking filter is global state that any earlier
	// test in the binary can reset — which intercepts this call to the server
	// under test and fails it with "cannot match any request" depending on what
	// ran before. Only the rpc*.localhost upstreams should be mocked; this hop
	// is real, so it goes through a transport gock never touched.
	client := &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{}}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}
