package valvebilling

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// APIKeyHashLength is how many hex characters of the digest name a key.
// It matches API_KEY_HASH_LENGTH in the monorepo's hash-api-key.ts.
const APIKeyHashLength = 32

// MinPepperLength mirrors the monorepo's MIN_PEPPER_LENGTH. A short pepper is
// refused rather than accepted, because the whole point of the pepper is that
// a stolen Redis dump is useless without it.
const MinPepperLength = 32

// HashAPIKey derives the Redis-safe identifier for an API key.
//
// Redis key names are not secret — SCAN, MONITOR, an RDB backup and a memory
// dump all expose them — so the raw key must never appear in one. A real key
// was exposed exactly that way on 2026-08-02.
//
// This must agree byte for byte with the TypeScript hashApiKey, because the
// api service writes the buckets this reads. The construction is
// HMAC-SHA256 with the PEPPER AS THE KEY and the API KEY AS THE MESSAGE —
// that order is not interchangeable — hex encoded lowercase, then truncated
// to the first 32 hex characters.
//
// HMAC rather than a bare SHA-256: a bare digest of a guessable key can be
// confirmed offline from a dump alone.
//
// The pepper is taken as an argument rather than read from the environment
// here, so this stays a pure function that a conformance test can drive with
// published vectors. Read it once at startup with PepperFromEnv.
func HashAPIKey(pepper, apiKey string) (string, error) {
	if len(pepper) < MinPepperLength {
		// Fail loud. The monorepo throws here for the same reason: silently
		// accepting a weak or empty pepper would address a different Redis
		// namespace than the api writes, so every lookup would miss and the
		// customer would look unmetered rather than misconfigured.
		return "", fmt.Errorf(
			"valvebilling: pepper must be at least %d characters, got %d",
			MinPepperLength, len(pepper))
	}
	mac := hmac.New(sha256.New, []byte(pepper))
	// Write cannot fail for hash.Hash; the error is part of io.Writer only.
	_, _ = mac.Write([]byte(apiKey))
	return hex.EncodeToString(mac.Sum(nil))[:APIKeyHashLength], nil
}
