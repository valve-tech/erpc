package common

import (
	"context"
	"net/http"
	"sort"
	"sync"

	"github.com/erpc/erpc/util"
)

// ChainFamily is the fork's pluggable chain pattern: everything eRPC needs to
// know about a chain that is NOT already chain-agnostic.
//
// # WHY THIS SITS BESIDE ArchitectureHandler RATHER THAN INSIDE IT
//
// Upstream owns ArchitectureHandler (common/architecture.go) and is actively
// changing it. Adding the fork's methods to that interface would conflict on
// every upstream release, and every architecture package would have to
// implement fork-only methods to keep compiling. A sibling registry keyed by
// the same NetworkArchitecture costs one extra lookup and keeps the fork's
// surface in fork-owned files, so `git merge upstream` stays boring.
//
// # WHAT A FAMILY MUST ANSWER
//
// Only three questions are genuinely per-chain. Everything downstream —
// ranking, rotation, hedging, breaker state — already runs on chain-agnostic
// inputs (health.Tracker stores a plain int64 height; the selection policy
// reads only tracker metrics).
//
//  1. Probe  — is this upstream alive, is it synced, and what tip does it
//     report?
//  2. Classify — does this response mean "serve it" or "try another upstream"?
//  3. Transport — how is a call shaped (JSON-RPC body vs REST path)?
//  4. ValidateNetworkId — what does one of this chain's network IDs look like?
type ChainFamily interface {
	// Family is the architecture name this pattern serves ("evm", "btc").
	// Must match the `architecture:` value used in config and URLs.
	Family() NetworkArchitecture

	// Transport reports how requests reach an upstream of this family.
	Transport() ChainTransport

	// Probe performs ONE liveness+tip check and returns both.
	//
	// Deliberately one call, not the brief's separate Health() and Tip():
	// every chain we surveyed answers both from a single response
	// (`getblockchaininfo` carries `blocks`, `verificationprogress` AND
	// `initialblockdownload`; `eth_syncing` + `eth_blockNumber` is the only
	// two-call case, and that one is EVM's existing poller, not this path).
	// Splitting them would double probe traffic and let the two answers
	// disagree across the gap.
	//
	// The result is BOTH the rotation gate and the operator-facing signal —
	// see ChainProbe. Never return a bare bool: an operator asking "is this
	// chain serving?" needs the tip and the reason, not a yes/no.
	Probe(ctx context.Context, c ProbeCaller) ChainProbe

	// Classify decides what a response means for upstream rotation. It is the
	// generalization of the fork's emptyResultAccept rule (common/empty_result.go):
	// for some methods an empty answer IS the answer, and rotating on it
	// re-asks every other upstream for the same empty result.
	Classify(in ClassifyInput) RotateVerdict

	// ValidateNetworkId reports whether `body` is a well-formed network ID for
	// this family. `body` is everything AFTER the "<family>:" prefix — "1" in
	// "evm:1", "mainnet-beta" in "svm:mainnet-beta", "mainnet" in
	// "btc:mainnet".
	//
	// The shape is genuinely per-chain, so the family owns it: an integer
	// chain id means nothing to Bitcoin and a cluster name means nothing to
	// EVM. Registration forwards this method to util (see
	// util/network_id_shape.go), which is what util.IsValidNetworkId — the
	// gate the networks registry runs before it reads any config — consults.
	//
	// Be strict. Every id this accepts is one the request path will try to
	// build a network for.
	ValidateNetworkId(body string) bool
}

// ChainTransport names how a request is carried to an upstream.
type ChainTransport int

const (
	// TransportJsonRpc is a JSON-RPC 2.0 body over HTTP POST — EVM, Bitcoin,
	// Solana. The existing ingress, NormalizedRequest and client all apply.
	TransportJsonRpc ChainTransport = iota
	// TransportREST is a path+verb request — the Ethereum beacon API.
	//
	// NOT SERVED TODAY. NormalizedRequest carries a body and nothing else: no
	// verb, no path, no query (common/request.go). Ingress decides batching by
	// reading the first byte of the body. A REST family therefore needs a
	// request shape eRPC does not have. The constant exists so a family can
	// declare its transport honestly and be rejected at registration with a
	// clear error, rather than half-working.
	TransportREST
)

func (t ChainTransport) String() string {
	switch t {
	case TransportJsonRpc:
		return "jsonrpc"
	case TransportREST:
		return "rest"
	default:
		return "unknown"
	}
}

// ChainLiveness is a probe's verdict about an upstream.
type ChainLiveness int

const (
	// ChainUnknown is the zero value: no probe has completed yet. Distinct
	// from ChainDown so a cold start is never reported as an outage.
	ChainUnknown ChainLiveness = iota
	// ChainDown — unreachable, or answered in a way that means "not serving".
	ChainDown
	// ChainSyncing — reachable and honest, but behind. Serving from it risks
	// stale reads.
	ChainSyncing
	// ChainHealthy — reachable and caught up.
	ChainHealthy
)

func (l ChainLiveness) String() string {
	switch l {
	case ChainDown:
		return "down"
	case ChainSyncing:
		return "syncing"
	case ChainHealthy:
		return "healthy"
	default:
		return "unknown"
	}
}

// Serving reports whether an upstream in this state should take traffic.
func (l ChainLiveness) Serving() bool { return l == ChainHealthy }

// ChainProbe is one probe result. It is deliberately the SAME value that gates
// rotation and answers the operator's "is this chain enabled and working?" —
// a silent balancer is the thing this design is meant to avoid.
type ChainProbe struct {
	// Liveness is the verdict.
	Liveness ChainLiveness
	// Tip is the height/slot the upstream reports, or 0 if unknown. Feeds
	// health.Tracker.SetLatestBlockNumber, which is already chain-agnostic —
	// that one call is what buys head-lag exclusion and "prefer the most
	// caught-up upstream" for free.
	Tip int64
	// Detail is a short operator-facing reason ("verificationprogress 0.97").
	// Shown in health output; never parsed.
	Detail string
	// Err is the transport/decode failure, if any. A non-nil Err always
	// accompanies ChainDown.
	Err error
}

// ProbeCaller is the narrow transport a family needs to run a probe. Narrow on
// purpose: a family must not reach into Upstream, the tracker, or config, or
// it stops being unit-testable against a fake server.
type ProbeCaller interface {
	// CallJsonRpc issues one JSON-RPC call and returns the raw `result`.
	CallJsonRpc(ctx context.Context, method string, params []interface{}) ([]byte, error)
	// CallREST issues one REST request and returns its status and body.
	CallREST(ctx context.Context, verb, path string) (status int, body []byte, err error)
}

// RotateVerdict is what Classify decides about a response.
type RotateVerdict int

const (
	// VerdictServe — return this to the client. An empty result that is the
	// real answer lands here.
	VerdictServe RotateVerdict = iota
	// VerdictRotate — this upstream cannot answer; try another.
	VerdictRotate
	// VerdictClientError — the request itself is wrong. Rotating cannot help,
	// so do not burn other upstreams on it.
	VerdictClientError
)

func (v RotateVerdict) String() string {
	switch v {
	case VerdictServe:
		return "serve"
	case VerdictRotate:
		return "rotate"
	case VerdictClientError:
		return "clientError"
	default:
		return "unknown"
	}
}

// ClassifyInput is the response shape Classify judges. Plain data so a family
// stays testable without building a NormalizedResponse.
type ClassifyInput struct {
	// Method is the JSON-RPC method (or REST path) that was called.
	Method string
	// IsEmpty reports whether the result was empty/null/zero-ish.
	IsEmpty bool
	// HTTPStatus is the transport status, or 0 if not applicable.
	HTTPStatus int
	// ErrCode is the normalized eRPC error code, or "" when the call
	// succeeded at the transport level.
	ErrCode ErrorCode
}

var (
	chainFamiliesMu sync.RWMutex
	chainFamilies   = map[NetworkArchitecture]ChainFamily{}
)

// RegisterChainFamily records a family under its Family() name. Intended for
// package init(), mirroring how upstream's RegisterArchitecture is called.
//
// Returns an error rather than panicking so a caller can surface a bad
// registration as config output instead of killing the process, and so the
// TransportREST rejection below is testable.
func RegisterChainFamily(f ChainFamily) error {
	if f == nil {
		return NewErrInvalidConfig("cannot register a nil chain family")
	}
	name := f.Family()
	if name == "" {
		return NewErrInvalidConfig("chain family has an empty Family() name")
	}
	if f.Transport() == TransportREST {
		// Fail loudly at registration. A REST family cannot be served until
		// NormalizedRequest carries a verb and a path; letting it register
		// would produce a family that resolves but drops every request.
		return NewErrInvalidConfig(
			"chain family " + string(name) + " declares REST transport, which eRPC cannot serve yet " +
				"(NormalizedRequest carries no verb/path); route it at the edge instead")
	}
	chainFamiliesMu.Lock()
	defer chainFamiliesMu.Unlock()
	if _, dup := chainFamilies[name]; dup {
		return NewErrInvalidConfig("chain family " + string(name) + " is already registered")
	}
	// Teach util the family's network-ID shape in the same call. util sits
	// BELOW common in the import graph and cannot hold a ChainFamily, so this
	// bridge is how util.IsValidNetworkId learns about a new chain. Doing it
	// here rather than leaving it to the family author is deliberate: a family
	// that registered here but not there would be probeable and unroutable at
	// once, and the request would 404 with nothing to explain it.
	if err := util.RegisterNetworkIdShape(string(name), f.ValidateNetworkId); err != nil {
		return NewErrInvalidConfig("chain family " + string(name) +
			" could not register its network id shape: " + err.Error())
	}
	chainFamilies[name] = f
	return nil
}

// LookupChainFamily returns the family registered for an architecture.
func LookupChainFamily(a NetworkArchitecture) (ChainFamily, bool) {
	chainFamiliesMu.RLock()
	defer chainFamiliesMu.RUnlock()
	f, ok := chainFamilies[a]
	return f, ok
}

// LookupChainFamilyForUpstreamType resolves an upstream's `type:` to its chain
// family.
//
// UpstreamType and NetworkArchitecture are separate types upstream, but they
// are not independent values: `type: evm` means the EVM architecture and
// `type: svm` means SVM. Every call site that compares them already relies on
// that — the extractors, the state pollers and the client factory all read
// cfg.Type to decide architecture-specific behaviour. The mapping is therefore
// the identity on the string, and this helper is where the fact is stated once
// instead of being re-derived. If an upstream type ever names something that is
// NOT an architecture, this function is the single place to teach it.
func LookupChainFamilyForUpstreamType(t UpstreamType) (ChainFamily, bool) {
	return LookupChainFamily(NetworkArchitecture(t))
}

// ProbeTransportFactory is the OPTIONAL surface a family implements to build
// the ProbeCaller its own Probe expects.
//
// Optional and asserted narrowly, in the EndpointSchemeGate pattern. It exists
// because the probe wire shape is per-chain even when the transport is "HTTP
// POST with a JSON body": bitcoind wants a JSON-RPC 1.0 envelope and answers an
// RPC failure with HTTP 500 carrying the error, which is not what an EVM node
// does. Letting the upstream layer build one generic caller would bake one
// chain's dialect into chain-agnostic code — so the family that knows the
// dialect builds the caller instead.
//
// A family that does not implement it cannot be probed, and upstream bootstrap
// refuses such an upstream with a message naming the family. That is the honest
// outcome: without a probe there is no tip, no lag, and no way to tell a synced
// node from a stalled one, so the upstream would join the pool and route traffic
// on no evidence at all.
type ProbeTransportFactory interface {
	// NewProbeCaller returns a caller bound to `endpoint`, using `client` for
	// every request. The caller owns the client's timeout — a probe without one
	// can hang a poll loop forever.
	NewProbeCaller(endpoint string, client *http.Client) ProbeCaller
}

// NewFamilyProbeCaller asks `f` for a probe transport. `ok` is false when the
// family provides none.
func NewFamilyProbeCaller(f ChainFamily, endpoint string, client *http.Client) (ProbeCaller, bool) {
	factory, implements := f.(ProbeTransportFactory)
	if !implements {
		return nil, false
	}
	return factory.NewProbeCaller(endpoint, client), true
}

// EndpointSchemeGate is the OPTIONAL surface a family implements when it must
// refuse a URL scheme eRPC could otherwise carry.
//
// Optional and asserted narrowly, in the EvmStateProvenReader pattern: a family
// that can use every client eRPC has implements nothing. SVM is the only
// implementor today — it is http/https-only because nothing in the SVM path has
// been run against the WebSocket or gRPC clients.
type EndpointSchemeGate interface {
	// SupportsEndpointScheme reports whether this family's upstreams may use
	// `scheme`, and when they may not, a short operator-facing reason.
	SupportsEndpointScheme(scheme string) (ok bool, reason string)
}

// EndpointSchemeSupported asks f about a URL scheme. A family that does not
// implement EndpointSchemeGate allows every scheme the client factory knows —
// the factory still rejects the ones it has no client for, so an unhandled
// scheme cannot slip through as a working upstream.
func EndpointSchemeSupported(f ChainFamily, scheme string) (bool, string) {
	if gate, ok := f.(EndpointSchemeGate); ok {
		return gate.SupportsEndpointScheme(scheme)
	}
	return true, ""
}

// RegisteredChainFamilies lists registered architecture names, sorted so
// callers (health output, validation messages) are deterministic.
func RegisteredChainFamilies() []NetworkArchitecture {
	chainFamiliesMu.RLock()
	defer chainFamiliesMu.RUnlock()
	out := make([]NetworkArchitecture, 0, len(chainFamilies))
	for name := range chainFamilies {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// UnregisterChainFamilyForTest removes a family and its network-id shape.
//
// Test-only, and exported because the tests that need it live in OTHER
// packages: the client factory and the pipeline gates are registry-driven now,
// so a test for them registers a fake family and must put both registries back.
// The registries are process-global; a leaked fake decides the next test's
// answers.
func UnregisterChainFamilyForTest(name NetworkArchitecture) {
	chainFamiliesMu.Lock()
	defer chainFamiliesMu.Unlock()
	delete(chainFamilies, name)
	// Both registries are written by RegisterChainFamily, so both are cleared
	// here. A leftover shape would keep validating ids for a family that no
	// longer resolves.
	util.UnregisterNetworkIdShape(string(name))
}
