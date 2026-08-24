package valverelay

import "context"

// Backend is where a JSON-RPC request goes after billing allows it.
//
// The contract is one sentence, and the whole billing path rests on it: a nil
// error means an ANSWER was produced for the customer, and a non-nil error
// means none was. Capture happens exactly when Forward returns an answer, so
// this interface is the line between "the upstream did work" and "the customer
// gets nothing and pays nothing".
//
// A JSON-RPC error inside the body is still an answer. A reverted eth_call is
// work the upstream performed, and it is billed. An implementation must not
// read the body to decide otherwise.
//
// Implementations must be safe for concurrent use.
type Backend interface {
	// Forward sends one JSON-RPC request body to the chain and returns the
	// answer bytes. The body is passed through unread — this seam does not
	// parse, rewrite or validate it.
	Forward(ctx context.Context, chainID int64, body []byte) ([]byte, error)

	// Close releases whatever the backend holds. It is safe to call once.
	Close() error
}
