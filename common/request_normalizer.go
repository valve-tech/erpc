package common

import "context"

// RequestNormalizer is the OPTIONAL surface an ArchitectureHandler implements
// when its requests need rewriting before the pipeline sees them — EVM's hex
// padding and block-tag expansion are the only case today.
//
// Optional and asserted narrowly, in the pattern EvmStateProvenReader
// established: ArchitectureHandler is upstream's interface, implemented outside
// this repo as well as inside it, and widening it breaks every implementor at
// once. A family that has nothing to rewrite implements nothing and is left
// alone.
//
// This is what lets Network.prepareRequest be a registry lookup instead of a
// switch over architecture names: the pipeline parses the JSON-RPC body (every
// architecture eRPC can serve speaks JSON-RPC — registration refuses the rest)
// and then asks the handler whether it wants to touch it.
type RequestNormalizer interface {
	// NormalizeRequest rewrites req in place. It runs AFTER the body has been
	// parsed, so the parse result is memoized and re-reading it is free.
	//
	// Returning an error rejects the request, so return one only when the
	// request genuinely cannot be served — a normalizer that fails on an
	// unfamiliar shape turns a servable request into a 500.
	NormalizeRequest(ctx context.Context, req *NormalizedRequest) error
}
