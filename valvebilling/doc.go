// Package valvebilling performs the valve relay's per-request billing work
// inside the eRPC fork: resolve a cost, authorize it against Redis, let the
// caller forward, then capture what the answer actually cost.
//
// # Why this package exists as a package
//
// The fork tracks upstream erpc/erpc by rebasing, so every line the fork
// changes in an upstream-owned file is replayed forever and can conflict.
// This package imports nothing from eRPC. It depends on the standard library
// and the Redis client eRPC already vendors, which means it can be linked
// into eRPC's process, built into a separate binary from this same repo, or
// dropped entirely — and none of those choices requires touching a file
// upstream also edits. The hosting decision is deliberately left open; see
// valve/billing-module.md.
//
// The fork has done this twice before: indexer/ is a whole added subsystem,
// and common/chain_family.go is a sibling registry whose header explains the
// same reasoning. This follows both.
//
// # Why the metering decision is not here
//
// The decision tree — the order of the gates, the credits-per-second
// bucket's self-heal, the tier boundary — lives in a
// Lua script that runs inside Redis (authorize.lua, verbatim from the
// monorepo's AUTHORIZE_LUA as of a08e9b9). This package CALLS it. It does
// not reimplement it, and a future maintainer must not "simplify" it into
// Go. Two implementations of that decision would drift, and the monorepo has
// already been bitten: four semantic mutations to the TypeScript authorize()
// survived a 740-test suite, including one that would have given every
// freshly topped-up account an instant no_credits with nothing going red.
//
// Because the script body here is byte-identical to the TypeScript one, both
// languages hash to the same SHA1 and share a single cached script inside
// Redis. TestAuthorizeScript_MatchesTheMonorepoDigest pins that.
//
// # What can still break quietly
//
// Cost. It is computed here, not in Lua, and a wrong cost bills the wrong
// amount with nothing going red. It is a pure function of (chainId, method,
// tokenAddress) and it is pinned by a golden corpus generated from the live
// pricing table — see cost.go and cost_test.go. Read the three hazards
// documented on ResolveCost before changing anything there.
package valvebilling
