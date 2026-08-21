package common

import "bytes"

// IsJsonNull reports whether `raw` is the JSON literal `null`.
//
// # WHY THIS EXISTS
//
// JSON-RPC 1.0 REQUIRES both members on every response: a success carries
// `"error": null`, a failure carries `"result": null`. bitcoind therefore sends
// `"error": null` on every successful call, and many JSON-RPC 2.0 servers do the
// same even though 2.0 lets them omit the member.
//
// eRPC's parser only asked whether the member was PRESENT. Four bytes of `null`
// went into ParseError, matched none of its shapes — a JSON null unmarshals into
// a struct as a silent no-op, so even the `raw == "null"` guard there is
// unreachable — and fell through to "treat the raw data as the message". Every
// successful bitcoind response arrived as ErrEndpointServerSideException with the
// message "null", and every btc request exhausted its whole upstream pool.
//
// The check lives here, on the parse sites, rather than inside ParseError. The
// two parse sites know they are looking at a response's `error` MEMBER, where
// null unambiguously means "no error". ParseError is also called with a whole
// response body that failed to parse (clients/http_json_rpc_client.go), and a
// bare `null` body there is a malformed response, not a success — widening the
// rule to cover it would turn that into a silent empty answer.
func IsJsonNull(raw []byte) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
