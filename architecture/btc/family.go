// Package btc implements the Bitcoin chain family: bitcoind's JSON-RPC health
// probe, its tip, and its rotation rule.
//
// Deliberately small. Everything that ranks, rotates, hedges or trips a
// breaker already runs on chain-agnostic inputs — health.Tracker stores a plain
// int64 height and the selection policy reads only tracker metrics. So a new
// chain owes eRPC three answers (probe, classify, transport) and nothing else.
package btc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/erpc/erpc/common"
)

// Architecture is the `architecture:` value and URL segment for this family.
const Architecture = common.NetworkArchitecture("btc")

// headerLagTolerance is how far `blocks` may trail `headers` before the node
// counts as syncing.
//
// Not zero. bitcoind publishes a header before the block behind it is
// connected, so a healthy node sits one block behind its own header chain for
// a moment on every block. Gating at zero would flap every upstream out of
// rotation on every block and empty the pool — the failure mode the EVM side
// hit as erpc#934.
const headerLagTolerance = 2

// minVerificationProgress is the floor below which a node counts as syncing
// even when it claims not to be in initial block download.
//
// bitcoind clears `initialblockdownload` once it is near the tip, but a node
// replaying a long reorg or catching up after downtime can be out of IBD and
// still be materially behind. verificationprogress is the honest signal there.
const minVerificationProgress = 0.9999

// chainInfo is the subset of getblockchaininfo this family reads.
type chainInfo struct {
	Chain                string  `json:"chain"`
	Blocks               int64   `json:"blocks"`
	Headers              int64   `json:"headers"`
	VerificationProgress float64 `json:"verificationprogress"`
	InitialBlockDownload bool    `json:"initialblockdownload"`
}

// Family implements common.ChainFamily for Bitcoin and bitcoind-compatible
// chains (Dogecoin, Litecoin — same RPC surface).
type Family struct{}

// Family also builds its own probe transport. Without it eRPC cannot tell a
// synced node from a stalled one, and upstream bootstrap refuses every btc
// upstream — so the assertion is stated here rather than discovered at runtime.
var (
	_ common.ChainFamily           = (*Family)(nil)
	_ common.ProbeTransportFactory = (*Family)(nil)
)

// New returns the Bitcoin family.
func New() *Family { return &Family{} }

func (f *Family) Family() common.NetworkArchitecture { return Architecture }

func (f *Family) Transport() common.ChainTransport { return common.TransportJsonRpc }

// ValidateNetworkId accepts a bitcoind network name — the body of "btc:mainnet",
// "btc:testnet", "btc:signet", "btc:regtest".
//
// The list is NOT enumerated. bitcoind's own `chain` field is free text, the
// same RPC surface serves Dogecoin and Litecoin, and operators run private
// signets with their own names — an enumeration would reject a working node
// for not being on a list eRPC happens to know. The real constraint is that
// the name has to survive being half of a network ID, a metric label and a
// cache key, so it must be a single identifier segment: no colon (which would
// re-split the id), no empty value, nothing exotic.
func (f *Family) ValidateNetworkId(body string) bool {
	if body == "" {
		return false
	}
	for _, r := range body {
		if !(r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// Probe issues one getblockchaininfo and derives liveness and tip from it.
//
// One call answers everything: `blocks` is the tip, and
// `initialblockdownload` + `verificationprogress` + the blocks-vs-headers gap
// together decide whether the node is caught up. Splitting this into a
// separate health and tip call would double probe traffic and let the two
// answers disagree across the gap between them.
func (f *Family) Probe(ctx context.Context, c common.ProbeCaller) common.ChainProbe {
	raw, err := c.CallJsonRpc(ctx, "getblockchaininfo", nil)
	if err != nil {
		// Fail closed, and keep the cause: an operator needs to tell a refused
		// dial from a node answering badly.
		return common.ChainProbe{
			Liveness: common.ChainDown,
			Detail:   "getblockchaininfo failed",
			Err:      fmt.Errorf("btc probe: %w", err),
		}
	}

	var info chainInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return common.ChainProbe{
			Liveness: common.ChainDown,
			Detail:   "getblockchaininfo returned an undecodable body",
			Err:      fmt.Errorf("btc probe decode: %w", err),
		}
	}

	// Tip is reported even when syncing — an operator wants to watch it climb,
	// and the tracker wants the real number rather than a zero that would read
	// as "furthest behind".
	probe := common.ChainProbe{Tip: info.Blocks}

	switch {
	case info.Blocks <= 0:
		probe.Liveness = common.ChainSyncing
		probe.Detail = "height 0: node has no blocks yet"
	case info.InitialBlockDownload:
		probe.Liveness = common.ChainSyncing
		probe.Detail = fmt.Sprintf("initial block download, %d/%d blocks", info.Blocks, info.Headers)
	case info.Headers-info.Blocks > headerLagTolerance:
		probe.Liveness = common.ChainSyncing
		probe.Detail = fmt.Sprintf("%d blocks behind its own headers (%d/%d)",
			info.Headers-info.Blocks, info.Blocks, info.Headers)
	case info.VerificationProgress < minVerificationProgress:
		probe.Liveness = common.ChainSyncing
		probe.Detail = fmt.Sprintf("verificationprogress %.6f", info.VerificationProgress)
	default:
		probe.Liveness = common.ChainHealthy
		probe.Detail = fmt.Sprintf("height %d on %s", info.Blocks, info.Chain)
	}
	return probe
}

// rotateOnEmpty lists the methods where an empty result means "this node
// cannot answer" rather than "the answer is empty".
//
// The distinction is the whole rotation rule, and Bitcoin's version of it is
// narrower than EVM's. A node without `txindex` genuinely cannot return an
// arbitrary transaction while a peer with txindex can, so rotating is right.
// Chain-state reads (getblockcount, getblockchaininfo) answer from the node's
// own view — rotating on those re-asks every peer for a value only this node
// can give, which is the amplification the fork already measured on evm:369.
var rotateOnEmpty = map[string]bool{
	"getrawtransaction":    true,
	"gettransaction":       true,
	"getblock":             true,
	"getblockheader":       true,
	"gettxout":             true,
	"getblockhash":         true,
	"getrawmempool":        false,
	"getblockchaininfo":    false,
	"getblockcount":        false,
	"getbestblockhash":     false,
	"getnetworkinfo":       false,
	"estimatesmartfee":     false,
	"sendrawtransaction":   false,
	"getmempoolinfo":       false,
	"getdifficulty":        false,
	"getconnectioncount":   false,
	"getchaintips":         false,
	"getblockstats":        false,
	"getmempoolentry":      true,
	"getdescriptorinfo":    false,
	"validateaddress":      false,
	"getindexinfo":         false,
	"getnettotals":         false,
	"uptime":               false,
	"getmininginfo":        false,
	"getblocktemplate":     false,
	"getrawtransactions":   true,
	"scantxoutset":         false,
	"getreceivedbyaddress": false,
}

// Classify decides whether a response should be served or retried elsewhere.
func (f *Family) Classify(in common.ClassifyInput) common.RotateVerdict {
	// A malformed request fails identically on every peer. Rotating it
	// multiplies one bad client call by the size of the pool.
	if in.ErrCode == common.ErrCodeEndpointClientSideException {
		return common.VerdictClientError
	}
	if !in.IsEmpty {
		return common.VerdictServe
	}
	// Method names are lowercase by convention but callers vary; a
	// case-sensitive miss would fall through to serve and silently drop the
	// missing-transaction rotation.
	if rotateOnEmpty[strings.ToLower(in.Method)] {
		return common.VerdictRotate
	}
	return common.VerdictServe
}

func init() {
	// Registration failure is a programming error (duplicate name or a bad
	// transport), not a runtime condition — surface it immediately rather than
	// leaving a family that silently never resolves.
	if err := common.RegisterChainFamily(New()); err != nil {
		panic(fmt.Sprintf("btc: %v", err))
	}
}
