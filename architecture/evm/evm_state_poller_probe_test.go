package evm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests cover the availability probes the state poller uses to discover
// how much history an upstream really retains. Every probe reads a real
// response body, so they need forwardingUpstream (see forwarding_upstream_test.go)
// rather than the (nil, nil) doubles used elsewhere in this package.
//
// The probes all collapse their failures into the same (false, false, nil)
// triple, so asserting the return value alone cannot tell one branch from
// another. Each test therefore also asserts the discriminating property: which
// requests the probe actually sent.

// headerScript answers eth_getBlockByNumber for every block that `has` accepts
// and returns null for the rest. That models one node's retained history.
func headerScript(t *testing.T, has func(block int64) bool) forwardHandler {
	t.Helper()
	return func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		block, ok := requestedBlockNumber(t, req)
		if !ok || !has(block) {
			return jsonResult(req, `null`)
		}
		return jsonResult(req, blockHeader(block))
	}
}

// --- checkProbe: routing ---

func TestCheckProbe_EmptyProbeDefaultsToBlockHeader(t *testing.T) {
	up := newForwardingUpstream(123)
	up.on("eth_getBlockByNumber", headerScript(t, func(int64) bool { return true }))
	p := newGateTestPoller(t, up)

	ok, unsupported, err := p.checkProbe(context.Background(), "", 100)

	require.NoError(t, err)
	assert.True(t, ok)
	assert.False(t, unsupported)
	// The discriminating property: an empty probe must resolve to the header
	// probe, i.e. eth_getBlockByNumber and nothing else.
	assert.Equal(t, []string{"eth_getBlockByNumber"}, up.allCalls())
}

func TestCheckProbe_UnknownProbeIsUnsupportedWithoutForwarding(t *testing.T) {
	up := newForwardingUpstream(123)
	p := newGateTestPoller(t, up)

	ok, unsupported, err := p.checkProbe(context.Background(), common.EvmAvailabilityProbeType("magic"), 100)

	require.NoError(t, err)
	assert.False(t, ok)
	assert.True(t, unsupported, "an unrecognised probe must report unsupported, not unavailable")
	// Discriminating: an unknown probe must not guess a method and burn a
	// request on the upstream.
	assert.Empty(t, up.allCalls(), "an unknown probe must not forward anything")
}

func TestCheckProbe_RoutesEachProbeToItsOwnMethod(t *testing.T) {
	cases := []struct {
		probe   common.EvmAvailabilityProbeType
		wantAll []string
	}{
		{common.EvmProbeBlockHeader, []string{"eth_getBlockByNumber"}},
		{common.EvmProbeEventLogs, []string{"eth_getBlockByNumber", "eth_getLogs"}},
		{common.EvmProbeCallState, []string{"eth_getBalance"}},
		{common.EvmProbeTraceData, []string{"eth_getBlockByNumber", "trace_block"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.probe), func(t *testing.T) {
			up := newForwardingUpstream(123)
			up.on("eth_getBlockByNumber", headerScript(t, func(int64) bool { return true }))
			up.on("eth_getLogs", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				return jsonResult(req, `[{"address":"0x1"}]`)
			})
			up.on("eth_getBalance", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				return jsonResult(req, `"0x0"`)
			})
			up.on("trace_block", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
				return jsonResult(req, `[{"type":"call"}]`)
			})
			p := newGateTestPoller(t, up)

			ok, unsupported, err := p.checkProbe(context.Background(), tc.probe, 100)

			require.NoError(t, err)
			assert.True(t, ok)
			assert.False(t, unsupported)
			assert.Equal(t, tc.wantAll, up.allCalls())
		})
	}
}

// --- checkBlockHeaderProbe ---

func TestCheckBlockHeaderProbe(t *testing.T) {
	t.Run("NegativeBlockShortCircuits", func(t *testing.T) {
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)

		ok, err := p.checkBlockHeaderProbe(context.Background(), -1)

		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, up.allCalls(), "a negative block must not reach the upstream")
	})

	t.Run("HeaderPresentMeansAvailable", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(b int64) bool { return b >= 1000 }))
		p := newGateTestPoller(t, up)

		ok, err := p.checkBlockHeaderProbe(context.Background(), 1000)
		require.NoError(t, err)
		assert.True(t, ok)

		// Discriminating: the probe must ask for the block it was given, in hex.
		require.Len(t, up.methodCalls("eth_getBlockByNumber"), 1)
		assert.Contains(t, up.methodCalls("eth_getBlockByNumber")[0], `"0x3e8"`)
	})

	t.Run("NullResultMeansUnavailable", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(b int64) bool { return b >= 1000 }))
		p := newGateTestPoller(t, up)

		ok, err := p.checkBlockHeaderProbe(context.Background(), 999)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("TransportErrorIsUnavailableNotAnError", func(t *testing.T) {
		// A pruned node that 500s on old blocks must not abort the binary
		// search; it reports "not available" and the search keeps going.
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, errors.New("connection reset")
		})
		p := newGateTestPoller(t, up)

		ok, err := p.checkBlockHeaderProbe(context.Background(), 500)
		require.NoError(t, err, "a transport error must be swallowed, not surfaced")
		assert.False(t, ok)
	})

	t.Run("JsonRpcErrorIsUnavailable", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonRpcError(req, -32000, "missing trie node")
		})
		p := newGateTestPoller(t, up)

		ok, err := p.checkBlockHeaderProbe(context.Background(), 500)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("ErrorMemberWinsOverAResultMember", func(t *testing.T) {
		// A 200-OK that carries BOTH members. The error member must decide;
		// trusting the result here would mark a failed lookup as retained
		// history. This is the only shape that separates the error check from
		// the empty-result check, since an error response normally has no
		// result at all.
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResultBesideError(req, blockHeader(500), -32000, "missing trie node")
		})
		p := newGateTestPoller(t, up)

		ok, err := p.checkBlockHeaderProbe(context.Background(), 500)
		require.NoError(t, err)
		assert.False(t, ok, "the error member must veto a populated result")
	})
}

// --- fetchBlockHashByNumber ---

func TestFetchBlockHashByNumber(t *testing.T) {
	t.Run("ReturnsHashFromHeader", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(int64) bool { return true }))
		p := newGateTestPoller(t, up)

		hash, ok, err := p.fetchBlockHashByNumber(context.Background(), 7)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, fmt.Sprintf("0x%064x", 7), hash)
	})

	t.Run("HeaderWithAnEmptyHashIsNotAvailable", func(t *testing.T) {
		// A header that parses and DOES carry a hash field, but an empty one.
		// The path lookup succeeds, so only the emptiness check can reject it.
		// Without that check the caller sends eth_getLogs{"blockHash":""} and
		// reads the resulting error as pruned history.
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `{"number":"0x7","hash":""}`)
		})
		p := newGateTestPoller(t, up)

		hash, ok, err := p.fetchBlockHashByNumber(context.Background(), 7)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, hash)
	})

	t.Run("HeaderWithoutAHashFieldIsNotAvailable", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `{"number":"0x7"}`)
		})
		p := newGateTestPoller(t, up)

		_, ok, err := p.fetchBlockHashByNumber(context.Background(), 7)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("NegativeBlockShortCircuits", func(t *testing.T) {
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)

		_, ok, err := p.fetchBlockHashByNumber(context.Background(), -5)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Empty(t, up.allCalls())
	})
}

// --- checkEventLogsProbe ---

func TestCheckEventLogsProbe(t *testing.T) {
	// hashOnly answers the hash lookup so each sub-test only has to script logs.
	hashOnly := func(up *forwardingUpstream) *forwardingUpstream {
		return up.on("eth_getBlockByNumber", headerScript(t, func(int64) bool { return true }))
	}

	t.Run("NegativeBlockShortCircuits", func(t *testing.T) {
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkEventLogsProbe(context.Background(), -1)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
		assert.Empty(t, up.allCalls())
	})

	t.Run("MissingBlockHashSkipsTheLogsCall", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(int64) bool { return false }))
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkEventLogsProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported, "a pruned block is unavailable, not an unsupported method")
		// Discriminating: without a hash there is nothing to ask eth_getLogs.
		assert.Equal(t, []string{"eth_getBlockByNumber"}, up.allCalls())
	})

	t.Run("OneLogMeansAvailable", func(t *testing.T) {
		up := hashOnly(newForwardingUpstream(123))
		up.on("eth_getLogs", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `[{"address":"0xdead"}]`)
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkEventLogsProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.False(t, unsupported)
		// Discriminating: the probe must query by the hash it just fetched.
		require.Len(t, up.methodCalls("eth_getLogs"), 1)
		assert.Contains(t, up.methodCalls("eth_getLogs")[0], fmt.Sprintf(`"blockHash":"0x%064x"`, 10))
	})

	t.Run("EmptyLogsArrayIsNotAvailable", func(t *testing.T) {
		// A block with no events looks the same as a pruned block through this
		// probe. That is the documented trade-off; pin it so a "fix" that flips
		// empty to available has to be deliberate.
		up := hashOnly(newForwardingUpstream(123))
		up.on("eth_getLogs", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `[]`)
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkEventLogsProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
	})

	t.Run("NonArrayResultIsNotAvailable", func(t *testing.T) {
		up := hashOnly(newForwardingUpstream(123))
		up.on("eth_getLogs", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `{"unexpected":"shape"}`)
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkEventLogsProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
	})

	t.Run("TypedUnsupportedErrorReportsUnsupported", func(t *testing.T) {
		up := hashOnly(newForwardingUpstream(123))
		up.on("eth_getLogs", func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, common.NewErrEndpointUnsupported(errors.New("no logs index"))
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkEventLogsProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.True(t, unsupported, "an unsupported endpoint must not read as pruned history")
	})

	t.Run("MethodIgnoredReportsUnsupported", func(t *testing.T) {
		up := hashOnly(newForwardingUpstream(123))
		up.on("eth_getLogs", func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, common.NewErrUpstreamMethodIgnored("eth_getLogs", "fwd-ups")
		})
		p := newGateTestPoller(t, up)

		_, unsupported, err := p.checkEventLogsProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.True(t, unsupported)
	})

	t.Run("OtherTransportErrorIsUnavailableNotUnsupported", func(t *testing.T) {
		up := hashOnly(newForwardingUpstream(123))
		up.on("eth_getLogs", func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, errors.New("i/o timeout")
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkEventLogsProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported, "a timeout is not evidence the method is missing")
	})

	t.Run("MethodNotFoundReportsUnsupported", func(t *testing.T) {
		up := hashOnly(newForwardingUpstream(123))
		up.on("eth_getLogs", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonRpcError(req, -32601, "the method eth_getLogs does not exist")
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkEventLogsProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.True(t, unsupported)
	})

	t.Run("OtherJsonRpcErrorIsUnavailable", func(t *testing.T) {
		up := hashOnly(newForwardingUpstream(123))
		up.on("eth_getLogs", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonRpcError(req, -32000, "query returned more than 10000 results")
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkEventLogsProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported, "a range error is not evidence the method is missing")
	})
}

// --- checkCallStateProbe ---

func TestCheckCallStateProbe(t *testing.T) {
	t.Run("NegativeBlockShortCircuits", func(t *testing.T) {
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkCallStateProbe(context.Background(), -1)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
		assert.Empty(t, up.allCalls())
	})

	t.Run("ZeroBalanceStillMeansStateIsHeld", func(t *testing.T) {
		// "0x0" is a real answer: the node held the trie and looked it up.
		// Treating it as absent would mark every archive node as pruned.
		up := newForwardingUpstream(123)
		up.on("eth_getBalance", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `"0x0"`)
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkCallStateProbe(context.Background(), 4096)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.False(t, unsupported)
		// Discriminating: the probe asks the zero address at the given block.
		require.Len(t, up.methodCalls("eth_getBalance"), 1)
		assert.Contains(t, up.methodCalls("eth_getBalance")[0], `"0x1000"`)
		assert.Contains(t, up.methodCalls("eth_getBalance")[0], `"0x0000000000000000000000000000000000000000"`)
	})

	t.Run("NullResultMeansStateIsGone", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBalance", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `null`)
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkCallStateProbe(context.Background(), 4096)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
	})

	t.Run("MissingTrieNodeIsUnavailableNotUnsupported", func(t *testing.T) {
		// The classic pruned-node answer. It must narrow the history bound, not
		// disable the probe.
		up := newForwardingUpstream(123)
		up.on("eth_getBalance", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonRpcError(req, -32000, "missing trie node")
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkCallStateProbe(context.Background(), 4096)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
	})

	t.Run("TypedUnsupportedErrorReportsUnsupported", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBalance", func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, common.NewErrEndpointUnsupported(errors.New("eth_getBalance disabled"))
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkCallStateProbe(context.Background(), 4096)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.True(t, unsupported)
	})

	t.Run("OtherTransportErrorIsUnavailable", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBalance", func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, errors.New("connection refused")
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkCallStateProbe(context.Background(), 4096)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
	})
}

// --- checkTraceDataProbe ---

func TestCheckTraceDataProbe(t *testing.T) {
	withHash := func() *forwardingUpstream {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(int64) bool { return true }))
		return up
	}
	emptyArray := func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonResult(req, `[]`)
	}
	notFound := func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
		return jsonRpcError(req, -32601, "method not found")
	}

	t.Run("MissingBlockHashSkipsEveryTraceCall", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(int64) bool { return false }))
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkTraceDataProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
		assert.Equal(t, []string{"eth_getBlockByNumber"}, up.allCalls())
	})

	t.Run("TraceBlockHitStopsAfterOneAttempt", func(t *testing.T) {
		up := withHash()
		up.on("trace_block", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `[{"type":"call"}]`)
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkTraceDataProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.False(t, unsupported)
		// Discriminating: the fallbacks must NOT run once one engine answered.
		assert.Equal(t, []string{"eth_getBlockByNumber", "trace_block"}, up.allCalls())
	})

	t.Run("FallsThroughToDebugTraceBlockByHash", func(t *testing.T) {
		up := withHash()
		up.on("trace_block", notFound)
		up.on("debug_traceBlockByHash", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `[{"gas":"0x1"}]`)
		})
		p := newGateTestPoller(t, up)

		ok, _, err := p.checkTraceDataProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t,
			[]string{"eth_getBlockByNumber", "trace_block", "debug_traceBlockByHash"},
			up.allCalls())
	})

	t.Run("FallsThroughToTraceReplayBlockTransactions", func(t *testing.T) {
		up := withHash()
		up.on("trace_block", notFound)
		up.on("debug_traceBlockByHash", notFound)
		up.on("trace_replayBlockTransactions", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonResult(req, `[{"trace":[]}]`)
		})
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkTraceDataProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.True(t, ok)
		assert.False(t, unsupported)
		assert.Equal(t, []string{
			"eth_getBlockByNumber", "trace_block",
			"debug_traceBlockByHash", "trace_replayBlockTransactions",
		}, up.allCalls())
	})

	t.Run("AllThreeMethodsMissingReportsUnsupported", func(t *testing.T) {
		up := withHash()
		up.on("trace_block", notFound)
		up.on("debug_traceBlockByHash", notFound)
		up.on("trace_replayBlockTransactions", notFound)
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkTraceDataProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.True(t, unsupported,
			"a node with no tracing engine must report unsupported, not empty history")
	})

	t.Run("MixedMissingAndEmptyReportsUnavailable", func(t *testing.T) {
		// One engine exists and answered empty, so tracing IS supported here —
		// this block simply has no trace data. Reporting unsupported would
		// disable the probe for the whole upstream.
		up := withHash()
		up.on("trace_block", notFound)
		up.on("debug_traceBlockByHash", emptyArray)
		up.on("trace_replayBlockTransactions", notFound)
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkTraceDataProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
	})

	t.Run("TypedUnsupportedTransportErrorsReportUnsupported", func(t *testing.T) {
		up := withHash()
		skip := func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, common.NewErrEndpointUnsupported(errors.New("tracing not enabled"))
		}
		up.on("trace_block", skip)
		up.on("debug_traceBlockByHash", skip)
		up.on("trace_replayBlockTransactions", skip)
		p := newGateTestPoller(t, up)

		_, unsupported, err := p.checkTraceDataProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.True(t, unsupported)
	})

	t.Run("PlainTransportErrorsReportUnavailable", func(t *testing.T) {
		up := withHash()
		boom := func(context.Context, *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return nil, errors.New("upstream 502")
		}
		up.on("trace_block", boom)
		up.on("debug_traceBlockByHash", boom)
		up.on("trace_replayBlockTransactions", boom)
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkTraceDataProbe(context.Background(), 10)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported, "502s are not evidence tracing is absent")
	})

	t.Run("NegativeBlockShortCircuits", func(t *testing.T) {
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)

		ok, unsupported, err := p.checkTraceDataProbe(context.Background(), -3)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.False(t, unsupported)
		assert.Empty(t, up.allCalls())
	})
}

// --- binarySearchEarliest ---

func TestBinarySearchEarliest(t *testing.T) {
	t.Run("GenesisAvailableReturnsZeroAfterOneProbe", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(int64) bool { return true }))
		p := newGateTestPoller(t, up)

		got, err := p.binarySearchEarliest(context.Background(), common.EvmProbeBlockHeader, 0, 1_000_000)
		require.NoError(t, err)
		assert.Equal(t, int64(0), got)
		// Discriminating: the block-0 fast path must skip the search entirely.
		assert.Len(t, up.allCalls(), 1)
	})

	t.Run("BlockOneFastPath", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(b int64) bool { return b >= 1 }))
		p := newGateTestPoller(t, up)

		got, err := p.binarySearchEarliest(context.Background(), common.EvmProbeBlockHeader, 0, 1_000_000)
		require.NoError(t, err)
		assert.Equal(t, int64(1), got)
		assert.Len(t, up.allCalls(), 2, "block 0 then block 1, then stop")
	})

	t.Run("FindsThePruneBoundary", func(t *testing.T) {
		const boundary = int64(4_000_037)
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(b int64) bool { return b >= boundary }))
		p := newGateTestPoller(t, up)

		got, err := p.binarySearchEarliest(context.Background(), common.EvmProbeBlockHeader, 0, 9_000_000)
		require.NoError(t, err)
		assert.Equal(t, boundary, got)
		// Discriminating: a linear scan would also land on the boundary, so
		// pin the cost — log2(9e6) ≈ 24, plus the two fast-path probes.
		assert.Less(t, len(up.allCalls()), 30, "the search must stay logarithmic")
	})

	t.Run("NegativeHighReturnsZeroWithoutProbing", func(t *testing.T) {
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)

		got, err := p.binarySearchEarliest(context.Background(), common.EvmProbeBlockHeader, 0, -1)
		require.NoError(t, err)
		assert.Equal(t, int64(0), got)
		assert.Empty(t, up.allCalls())
	})

	t.Run("EmptyProbeTypeDefaultsToBlockHeader", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(b int64) bool { return b >= 500 }))
		p := newGateTestPoller(t, up)

		got, err := p.binarySearchEarliest(context.Background(), "", 0, 100_000)
		require.NoError(t, err)
		assert.Equal(t, int64(500), got)
		assert.Equal(t, len(up.allCalls()), up.callCount("eth_getBlockByNumber"))
	})

	// An upstream that answers nothing must not be recorded as retaining the
	// tip. The search reports the failure so the caller fails open.
	t.Run("NothingAvailableIsAnError", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(int64) bool { return false }))
		p := newGateTestPoller(t, up)

		got, err := p.binarySearchEarliest(context.Background(), common.EvmProbeBlockHeader, 0, 1024)
		require.ErrorIs(t, err, errEarliestBlockUnavailable)
		assert.Equal(t, int64(0), got)
		// Discriminating: the loop converges on `high` without probing it, so
		// the conclusion "nothing is available" is only sound once `high`
		// itself has been asked.
		assert.Contains(t, strings.Join(up.methodCalls("eth_getBlockByNumber"), "\n"), `"0x400"`,
			"the search must probe the value it converges on (1024) before giving up")
	})

	// The one case the post-loop probe rescues: the node kept exactly the tip.
	// Every midpoint answers "no", so only probing `high` can find it.
	t.Run("EarliestEqualToHighIsStillFound", func(t *testing.T) {
		const tip = int64(1024)
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(b int64) bool { return b >= tip }))
		p := newGateTestPoller(t, up)

		got, err := p.binarySearchEarliest(context.Background(), common.EvmProbeBlockHeader, 0, tip)
		require.NoError(t, err)
		assert.Equal(t, tip, got)
	})

	// A node with no tracing engine answers -32601 at every height. That is a
	// missing capability, not a pruned history, and the two must not collapse
	// into the same answer.
	t.Run("UnsupportedProbeIsReportedAsUnsupported", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(int64) bool { return true }))
		notFound := func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			return jsonRpcError(req, -32601, "method not found")
		}
		up.on("trace_block", notFound)
		up.on("debug_traceBlockByHash", notFound)
		up.on("trace_replayBlockTransactions", notFound)
		p := newGateTestPoller(t, up)

		got, err := p.binarySearchEarliest(context.Background(), common.EvmProbeTraceData, 0, 2048)
		require.ErrorIs(t, err, errEarliestProbeUnsupported)
		assert.Equal(t, int64(0), got)
		// Discriminating: the first unsupported answer settles it, so the
		// search must stop instead of walking log2(2048) more heights.
		assert.Less(t, up.callCount("trace_block"), 3,
			"an unsupported probe must abort the search, not walk the range")
	})

	t.Run("AnUnknownProbeTypeIsUnsupported", func(t *testing.T) {
		up := newForwardingUpstream(123)
		p := newGateTestPoller(t, up)

		got, err := p.binarySearchEarliest(context.Background(), common.EvmAvailabilityProbeType("magic"), 0, 2048)
		require.ErrorIs(t, err, errEarliestProbeUnsupported)
		assert.Equal(t, int64(0), got)
		assert.Empty(t, up.allCalls(), "an unknown probe must not forward anything")
	})

	// NOTE: `binarySearchEarliest` is unexported and its single production
	// caller passes low = 0 literally, so a negative `low` cannot occur. The
	// old `if low < 0 { low = 0 }` clamp was therefore unreachable and is gone.
}

// --- PollEarliestBlockNumber ---

func TestPollEarliestBlockNumber(t *testing.T) {
	t.Run("EachProbeKeepsItsOwnBound", func(t *testing.T) {
		// One node, two probes, two different answers: headers go back to 100,
		// but the log index only covers 1234 upwards. Each bound must be stored
		// under its own key. If the probe name were dropped from that key, the
		// second poll would land on the first probe's counter and overwrite it,
		// and eth_getLogs routing would inherit the header bound.
		const headerFrom = int64(100)
		const logsFrom = int64(1_234)

		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", headerScript(t, func(b int64) bool { return b >= headerFrom }))
		up.on("eth_getLogs", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			// The probe queries by blockHash; headerScript renders the hash as
			// the zero-padded block number, so read it back out.
			jrq, err := req.JsonRpcRequest()
			require.NoError(t, err)
			params, ok := jrq.Params[0].(map[string]interface{})
			require.True(t, ok)
			var block int64
			_, err = fmt.Sscanf(params["blockHash"].(string), "0x%064x", &block)
			require.NoError(t, err)
			if block < logsFrom {
				return jsonResult(req, `[]`)
			}
			return jsonResult(req, `[{"address":"0xdead"}]`)
		})

		p := newGateTestPoller(t, up)
		// Seed latest so the poll does not have to fetch it. The seed and any
		// later poll agree, because "latest" resolves to this same tip.
		p.SuggestLatestBlock(9_000)

		gotHeader, err := p.PollEarliestBlockNumber(context.Background(), common.EvmProbeBlockHeader, time.Millisecond)
		require.NoError(t, err)
		assert.Equal(t, headerFrom, gotHeader)

		gotLogs, err := p.PollEarliestBlockNumber(context.Background(), common.EvmProbeEventLogs, time.Millisecond)
		require.NoError(t, err)
		assert.Equal(t, logsFrom, gotLogs)

		// Discriminating: both bounds survive side by side, and a probe that
		// was never polled stays at zero.
		assert.Equal(t, headerFrom, p.EarliestBlock(common.EvmProbeBlockHeader),
			"the eventLogs poll must not clobber the blockHeader bound")
		assert.Equal(t, logsFrom, p.EarliestBlock(common.EvmProbeEventLogs))
		assert.Equal(t, int64(0), p.EarliestBlock(common.EvmProbeTraceData))
	})

	t.Run("FetchesLatestWhenItIsNotKnownYet", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			block, ok := requestedBlockNumber(t, req)
			if !ok {
				// The "latest" tag: this is the extra poll the code makes when
				// no head is known yet.
				return jsonResult(req, blockHeader(600))
			}
			if block < 100 {
				return jsonResult(req, `null`)
			}
			return jsonResult(req, blockHeader(block))
		})
		p := newGateTestPoller(t, up)

		got, err := p.PollEarliestBlockNumber(context.Background(), common.EvmProbeBlockHeader, time.Millisecond)
		require.NoError(t, err)
		assert.Equal(t, int64(100), got)
		// Discriminating: the "latest" request proves the fallback poll ran.
		var sawLatest bool
		for _, body := range up.methodCalls("eth_getBlockByNumber") {
			if strings.Contains(body, `"latest"`) {
				sawLatest = true
			}
		}
		assert.True(t, sawLatest, "with no known head the poller must fetch latest first")
	})

	t.Run("LatestFetchFailureSurfacesTheError", func(t *testing.T) {
		up := newForwardingUpstream(123)
		up.on("eth_getBlockByNumber", func(_ context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
			if _, ok := requestedBlockNumber(t, req); !ok {
				return nil, errors.New("head unavailable")
			}
			return jsonResult(req, blockHeader(1))
		})
		p := newGateTestPoller(t, up)

		_, err := p.PollEarliestBlockNumber(context.Background(), common.EvmProbeBlockHeader, time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "head unavailable",
			"the cause must stay reachable, not be flattened into a generic poll error")
		// Discriminating: the binary search must not have started.
		assert.Equal(t, 1, up.callCount("eth_getBlockByNumber"),
			"a failed head fetch must abort before any probe")
	})
}
