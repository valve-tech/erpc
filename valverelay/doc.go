// Package valverelay is the hosting seam for the valve billing path.
//
// It answers one question that valvebilling deliberately left open: where does
// a request go after billing allows it? The answer is a Backend, and there are
// two of them — one that forwards inside eRPC's own process and one that POSTs
// to an eRPC over HTTP. "In-process" and "separate process" are therefore the
// same billed code path with a different backend, which is what lets the
// hosting decision be made at deploy time rather than at design time.
//
// # Billing wraps a forward; it is not baked into one
//
// Backend.Forward is the whole hot path on its own. Bill takes a forward
// function and wraps the billing around it, and it never mentions a Backend.
// So "forward, and bill it" and "just forward" differ by one call site rather
// than by a rewrite — which is what a host needs if the billing path ever
// stops being a blocking gate in front of eRPC.
//
// # Zero upstream-owned files change
//
// The fork tracks erpc/erpc by rebasing, so every line it changes in an
// upstream-owned file is replayed forever and can conflict. This package
// imports eRPC; eRPC does not import it. Nothing in erpc/, common/, the
// Makefile or the release config knows this exists, exactly as
// valve/billing-module.md requires. The seam is a new directory and a new
// cmd/ binary, which is the cheapest wiring the fork can carry.
//
// # What is NOT here, and must be supplied by the caller
//
// This is the billing path, not the relay. The TypeScript relay is about
// 11,000 lines across 61 modules; almost none of it is ported. Absent:
//
//   - Auth and API-key resolution. AccountID and KeyID are PARAMETERS on
//     Request. This package does not know where they come from and must not
//     invent a source.
//   - Tiering policy. Limits and CUCost are parameters. A zero Limits leaves
//     every rate gate in the Lua script disabled; the credit-balance gate
//     still applies.
//   - Analytics and Postgres writes. Result carries what an audit row would
//     need; writing it is the caller's job.
//   - Pricing refresh. LoadPriceTable reads files. What refreshes them, and
//     how often, stays the host's decision — see valvebilling's Prices().
//   - WebSockets, x402 per-request payments, the gas oracle, beacon REST and
//     the hash ring. None of it is here, and none of it is stubbed.
//
// # EVM only, because chain identity is an int64 here
//
// Backend.Forward takes an int64 chain id, so this seam addresses evm:<id>
// networks and nothing else. Widening it to a network id string is a change
// to make when a non-EVM chain actually needs billing, not before.
//
// # One known asymmetry between the two backends
//
// The billed path captures when, and only when, the forward produced an
// answer. Each backend decides that from what it can actually observe:
// the embedded one from the error eRPC's Network.Forward returns, the HTTP
// one from the response status.
//
// Those two are not identical. eRPC answers 200 for JSON-RPC application
// errors AND for several of its own failures — see determineResponseStatusCode
// in erpc/http_server.go, where everything that is not a transport-level fault
// falls through to 200. So an exhausted upstream bundle reaches the embedded
// backend as an error (not billed) and the HTTP backend as a 200 (billed).
//
// This is recorded rather than fixed. Closing it means matching eRPC's error
// payloads by string or by code, which is the kind of commitment the razor in
// CLAUDE.md forbids, and the two shapes would drift the first time upstream
// renamed an error. If it ever must close, eRPC needs a header that states
// "this body is an eRPC failure, not an upstream answer", and both backends
// read that one fact.
package valverelay
