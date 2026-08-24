package util

import (
	"crypto/sha256"
	"encoding/hex"
)

// RedactSecret renders an opaque secret as a stand-in that is safe to log.
//
// A log is not a secret store. It is shipped, aggregated, retained and read
// by people who are not entitled to the credential, so no caller-supplied
// secret may ever reach it verbatim. What an operator actually needs from a
// log line is the ability to say "this is the same value as the one on the
// line above", and that survives hashing.
//
// The output follows RedactEndpoint in this package: the same SHA-256 hash,
// the same hex encoding, the same "redacted=" prefix and the same five-
// character prefix of the digest. One vocabulary, so an operator reading a
// mixed log does not have to learn two. Five characters correlate two lines
// within one incident; they do not identify the secret to anyone who steals
// the log.
//
// The slice is taken from the hex digest, which is always 64 characters, so
// no input length can produce a short slice. The empty string gets its own
// marker rather than the well-known SHA-256 of "" — that constant would read
// like a real digest and send someone hunting for a key that was never sent.
//
// This makes no claim about the secret's shape: no prefix is preserved, no
// length is reported, no format is assumed. Anything a caller can put in a
// string is handled the same way.
func RedactSecret(secret string) string {
	if secret == "" {
		return "redacted=empty"
	}
	hasher := sha256.New()
	hasher.Write([]byte(secret))
	hash := hex.EncodeToString(hasher.Sum(nil))
	return "redacted=" + hash[:5]
}
