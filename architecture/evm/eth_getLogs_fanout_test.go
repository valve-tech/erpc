package evm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive executeGetLogsSubRequests — the request-splitting fan-out.
// A bug here silently returns PARTIAL logs, so every test below makes the two
// outcomes distinguishable: a sub-range that fails still carries a populated
// result, so "the merge honoured the failure" cannot be confused with "the
// merge happened to be empty".
//
// Concurrency is driven by channels, never by sleeps. Each sub-request is
// gated: the test decides which one returns first, which fails, and which is
// still in flight while another errors. Two of the assertions are sound by
// construction rather than by timing — the fan-out cannot return while a
// handler is blocked (wg.Wait), and a sub-request cannot start while the
// semaphore is full.

// subScript is the script for one gated sub-request.
type subScript struct {
	entered  chan struct{} // closed when the fan-out enters the handler
	release  chan struct{} // the test closes this to let the handler answer
	finished chan struct{} // closed once the handler has answered
	reply    forwardHandler
}

// gatedLogsNetwork answers each eth_getLogs sub-request from a per-fromBlock
// script and holds it open until the test releases it.
type gatedLogsNetwork struct {
	*forwardingNetwork

	mu          sync.Mutex
	scripts     map[string]*subScript
	inFlight    int
	maxInFlight int
	completed   []string
}

func newGatedLogsNetwork(t *testing.T, evmCfg *common.EvmNetworkConfig) *gatedLogsNetwork {
	t.Helper()
	n := &gatedLogsNetwork{
		forwardingNetwork: newForwardingNetwork(123),
		scripts:           map[string]*subScript{},
	}
	n.cfg = &common.NetworkConfig{Architecture: common.ArchitectureEvm, Evm: evmCfg}
	n.on("eth_getLogs", func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		from := subRequestFromBlock(t, req)

		n.mu.Lock()
		s := n.scripts[from]
		if s != nil {
			n.inFlight++
			if n.inFlight > n.maxInFlight {
				n.maxInFlight = n.inFlight
			}
		}
		n.mu.Unlock()
		require.NotNilf(t, s, "the fan-out sent an unscripted sub-request starting at %s", from)

		close(s.entered)
		<-s.release

		n.mu.Lock()
		n.inFlight--
		n.completed = append(n.completed, from)
		n.mu.Unlock()
		close(s.finished)

		return s.reply(ctx, req)
	})
	return n
}

// script registers the answer for the sub-request starting at fromBlock.
func (n *gatedLogsNetwork) script(fromBlock int64, reply forwardHandler) *subScript {
	s := &subScript{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
		reply:    reply,
	}
	n.mu.Lock()
	n.scripts[fmt.Sprintf("0x%x", fromBlock)] = s
	n.mu.Unlock()
	return s
}

func (n *gatedLogsNetwork) peakConcurrency() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.maxInFlight
}

func (n *gatedLogsNetwork) completionOrder() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.completed...)
}

// subRequestFromBlock reads the fromBlock of an eth_getLogs sub-request.
func subRequestFromBlock(t *testing.T, req *common.NormalizedRequest) string {
	t.Helper()
	jrq, err := req.JsonRpcRequest()
	require.NoError(t, err)
	require.NotEmpty(t, jrq.Params)
	filter, ok := jrq.Params[0].(map[string]interface{})
	require.True(t, ok)
	from, _ := filter["fromBlock"].(string)
	return from
}

// logsReply answers with one log naming the sub-range, so a merged body shows
// exactly which sub-ranges contributed.
func logsReply(fromBlock int64) forwardHandler {
	return func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResult(req, fmt.Sprintf(`[{"blockNumber":"0x%x"}]`, fromBlock))
	}
}

// cachedLogsReply is logsReply with the from-cache flag set.
func cachedLogsReply(fromBlock int64) forwardHandler {
	return func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		rs, err := logsReply(fromBlock)(ctx, req)
		if err != nil {
			return nil, err
		}
		return rs.SetFromCache(true), nil
	}
}

// splitRequest is the top-level eth_getLogs the fan-out was derived from.
func splitRequest() *common.NormalizedRequest {
	return common.NewNormalizedRequest([]byte(
		`{"jsonrpc":"2.0","method":"eth_getLogs","params":[{"fromBlock":"0x1","toBlock":"0x4"}],"id":1}`))
}

// contiguousSubs builds one sub-request per block in [from,to].
func contiguousSubs(from, to int64) []ethGetLogsSubRequest {
	subs := make([]ethGetLogsSubRequest, 0, to-from+1)
	for b := from; b <= to; b++ {
		subs = append(subs, ethGetLogsSubRequest{fromBlock: b, toBlock: b})
	}
	return subs
}

// mergedLogs renders the merged JSON-RPC response's result array.
func mergedLogs(t *testing.T, jrr *common.JsonRpcResponse) []interface{} {
	t.Helper()
	var buf bytes.Buffer
	_, err := jrr.WriteTo(&buf)
	require.NoError(t, err)
	var out map[string]interface{}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &out))
	arr, ok := out["result"].([]interface{})
	require.Truef(t, ok, "merged body has no result array: %s", buf.String())
	return arr
}

// --- a failed sub-range must never be merged away ---

func TestExecuteGetLogsSubRequests_AnErrorMemberOnOneSubRangeFailsTheWholeMerge(t *testing.T) {
	n := newGatedLogsNetwork(t, &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitConcurrency: 4})
	first := n.script(1, logsReply(1))
	// A 200-OK carrying BOTH a populated result and an error member. Only this
	// shape separates "the fan-out honoured the error member" from "the second
	// sub-range happened to return nothing".
	second := n.script(2, func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResultBesideError(req, `[{"blockNumber":"0x2"}]`, -32000, "log index is rebuilding")
	})
	close(first.release)
	close(second.release)

	merged, fromCache, err := executeGetLogsSubRequests(
		context.Background(), n, splitRequest(), contiguousSubs(1, 2), "")

	require.Error(t, err, "a sub-range that reported an error must fail the whole split")
	assert.Contains(t, err.Error(), "log index is rebuilding")
	assert.Nil(t, merged, "a partial merge must never reach the caller")
	assert.False(t, fromCache)
}

func TestExecuteGetLogsSubRequests_ASubRangeWithNoJsonRpcResponseIsAFailure(t *testing.T) {
	n := newGatedLogsNetwork(t, &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitConcurrency: 4})
	first := n.script(1, logsReply(1))
	// A nil response with a nil error: the shape Upstream.Forward itself logs
	// as "nil response and nil error". The fan-out must call it a failure, not
	// an empty block range.
	second := n.script(2, func(_ context.Context, _ *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return nil, nil
	})
	close(first.release)
	close(second.release)

	merged, _, err := executeGetLogsSubRequests(
		context.Background(), n, splitRequest(), contiguousSubs(1, 2), "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected empty json-rpc response")
	// Discriminating: sub-range 1 DID return a log. A fan-out that read the
	// missing response as "no logs in block 2" would merge that one log and
	// report success.
	assert.Nil(t, merged)
}

func TestExecuteGetLogsSubRequests_TheCallWaitsForEveryInFlightSubRangeAndJoinsEveryFailure(t *testing.T) {
	n := newGatedLogsNetwork(t, &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitConcurrency: 3})
	failFast := n.script(1, func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonRpcError(req, -32000, "first sub-range refused")
	})
	failSlow := n.script(2, func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonRpcError(req, -32000, "second sub-range refused")
	})
	stillRunning := n.script(3, logsReply(3))

	type outcome struct {
		merged *common.JsonRpcResponse
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		merged, _, err := executeGetLogsSubRequests(
			context.Background(), n, splitRequest(), contiguousSubs(1, 3), "")
		done <- outcome{merged: merged, err: err}
	}()

	// Let the two failures land while sub-range 3 stays inside its handler.
	<-failFast.entered
	<-failSlow.entered
	<-stillRunning.entered
	close(failFast.release)
	close(failSlow.release)

	// Sound by construction: sub-range 3's handler is blocked on its release
	// channel, so wg.Wait cannot have returned. A fan-out that abandoned peers
	// on the first error would already have answered here.
	select {
	case got := <-done:
		t.Fatalf("the fan-out returned while a sub-range was still in flight: %v", got.err)
	default:
	}

	close(stillRunning.release)
	got := <-done

	require.Error(t, got.err)
	assert.Nil(t, got.merged)
	// Discriminating: BOTH failures must survive. Keeping only the first would
	// hide which sub-ranges of the caller's window are missing.
	assert.Contains(t, got.err.Error(), "first sub-range refused")
	assert.Contains(t, got.err.Error(), "second sub-range refused")
}

// --- the configured concurrency really bounds the fan-out ---

func TestExecuteGetLogsSubRequests_TheConfiguredConcurrencyBoundsTheRequestsInFlight(t *testing.T) {
	n := newGatedLogsNetwork(t, &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitConcurrency: 2})
	subs := []*subScript{
		n.script(1, logsReply(1)),
		n.script(2, logsReply(2)),
		n.script(3, logsReply(3)),
		n.script(4, logsReply(4)),
	}

	type outcome struct {
		merged *common.JsonRpcResponse
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		merged, _, err := executeGetLogsSubRequests(
			context.Background(), n, splitRequest(), contiguousSubs(1, 4), "")
		done <- outcome{merged: merged, err: err}
	}()

	// Two tokens, so exactly two sub-requests may sit inside the handler.
	<-subs[0].entered
	<-subs[1].entered

	// Sound by construction: sub-request 3 acquires its token in the spawning
	// loop, and both tokens are held by the blocked handlers above.
	select {
	case <-subs[2].entered:
		t.Fatal("a third sub-request started while the concurrency limit was already full")
	default:
	}

	// Freeing one token must let exactly one more through.
	close(subs[0].release)
	<-subs[2].entered
	close(subs[1].release)
	<-subs[3].entered
	// Answer the LAST sub-range before the third, so completion order and
	// sub-request order disagree.
	close(subs[3].release)
	<-subs[3].finished
	close(subs[2].release)

	got := <-done
	require.NoError(t, got.err)
	assert.LessOrEqual(t, n.peakConcurrency(), 2,
		"the fan-out must never hold more sub-requests open than the configured concurrency")
	// Discriminating: completion order is 1,2,4,3 — scrambled on purpose — yet
	// the merged body must stay in sub-request order.
	assert.Equal(t, []string{"0x1", "0x2", "0x4", "0x3"}, n.completionOrder())
	assert.Equal(t,
		[]interface{}{
			map[string]interface{}{"blockNumber": "0x1"},
			map[string]interface{}{"blockNumber": "0x2"},
			map[string]interface{}{"blockNumber": "0x3"},
			map[string]interface{}{"blockNumber": "0x4"},
		},
		mergedLogs(t, got.merged))
}

// --- the from-cache flag ---

func TestExecuteGetLogsSubRequests_TheMergeIsFromCacheOnlyWhenEverySubRangeWas(t *testing.T) {
	for _, tc := range []struct {
		name        string
		secondReply forwardHandler
		want        bool
	}{
		{"EverySubRangeCached", cachedLogsReply(2), true},
		{"OneSubRangeWentToAnUpstream", logsReply(2), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newGatedLogsNetwork(t, &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitConcurrency: 2})
			first := n.script(1, cachedLogsReply(1))
			second := n.script(2, tc.secondReply)
			close(first.release)
			close(second.release)

			merged, fromCache, err := executeGetLogsSubRequests(
				context.Background(), n, splitRequest(), contiguousSubs(1, 2), "")

			require.NoError(t, err)
			// Discriminating: both cases merge the SAME two logs. Only the flag
			// differs, so an assertion on the body alone could not tell them
			// apart — and a wrongly-set flag makes the caller skip the cache
			// write for a half-fresh answer.
			assert.Len(t, mergedLogs(t, merged), 2)
			assert.Equal(t, tc.want, fromCache)
		})
	}
}

// --- the skip-cache-read directive reaches every sub-request ---

func TestExecuteGetLogsSubRequests_TheSkipCacheReadDirectiveOverridesTheParents(t *testing.T) {
	n := newGatedLogsNetwork(t, &common.EvmNetworkConfig{ChainId: 123, GetLogsSplitConcurrency: 2})
	var seen []string
	var mu sync.Mutex
	record := func(fromBlock int64) forwardHandler {
		return func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			mu.Lock()
			if d := req.Directives(); d != nil {
				seen = append(seen, d.SkipCacheRead)
			}
			mu.Unlock()
			return logsReply(fromBlock)(ctx, req)
		}
	}
	first := n.script(1, record(1))
	second := n.script(2, record(2))
	close(first.release)
	close(second.release)

	parent := splitRequest()
	parent.SetDirectives(&common.RequestDirectives{SkipCacheRead: "parent-value"})

	_, _, err := executeGetLogsSubRequests(
		context.Background(), n, parent, contiguousSubs(1, 2), "memory-*")

	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	// Discriminating: the fan-out is given "memory-*" explicitly, and it must
	// win over the parent's own directive on every sub-request. Passing the
	// parent's value through would read a cache the caller asked to bypass.
	assert.Equal(t, []string{"memory-*", "memory-*"}, seen)
}
