package util

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Network-ID shapes, per chain family.
//
// A network ID is "<family>:<body>" — "evm:1", "svm:mainnet-beta",
// "svm:fogo:mainnet", "btc:mainnet". The FAMILY owns the body: an integer
// chain id means nothing to Bitcoin and a cluster name means nothing to EVM.
// This file is where a family teaches util its shape, so IsValidNetworkId
// (ids.go) can stay a split-and-ask.
//
// # WHY THE REGISTRY LIVES IN util RATHER THAN BESIDE ChainFamily
//
// common imports util, so util can never import common: a family cannot hand
// util a common.ChainFamily. common.RegisterChainFamily bridges instead — it
// registers the family's ValidateNetworkId here under the same name. A family
// author still makes ONE registration call, and the import graph is unchanged.
//
// # WHY THE evm/svm BUILTINS STAY
//
// util is below every architecture package, so a binary — or util's own test
// binary — can run with no family registered at all. The builtins keep evm and
// svm validating identically in that case. They are not a second opinion: the
// registered evm and svm families call these very functions, so the registered
// path and the fallback cannot drift apart.

// NetworkIdShape reports whether `body` — everything AFTER the "<family>:"
// prefix — is a well-formed network ID body for one family.
type NetworkIdShape func(body string) bool

var (
	networkIdShapesMu sync.RWMutex
	networkIdShapes   = map[string]NetworkIdShape{}
)

// RegisterNetworkIdShape records a family's ID shape. Call it through
// common.RegisterChainFamily rather than directly, so a family cannot end up
// probeable but unroutable.
//
// Returns an error instead of panicking: a bad registration should surface as
// config output, and the duplicate rejection has to be testable.
func RegisterNetworkIdShape(family string, shape NetworkIdShape) error {
	if family == "" {
		return fmt.Errorf("cannot register a network id shape without a family name")
	}
	if shape == nil {
		return fmt.Errorf("network id shape for %q is nil", family)
	}
	networkIdShapesMu.Lock()
	defer networkIdShapesMu.Unlock()
	if _, dup := networkIdShapes[family]; dup {
		return fmt.Errorf("network id shape for %q is already registered", family)
	}
	networkIdShapes[family] = shape
	return nil
}

// UnregisterNetworkIdShape removes a family's shape. It exists for the test
// cleanup that mirrors common's own registry teardown — the maps are
// process-global, so a test that registers a fake family must put them back.
func UnregisterNetworkIdShape(family string) {
	networkIdShapesMu.Lock()
	defer networkIdShapesMu.Unlock()
	delete(networkIdShapes, family)
}

// networkIdShapeVerdict asks the registered family about `body`. `known`
// reports whether any family answered at all — the caller falls back to the
// builtins only when nobody did.
func networkIdShapeVerdict(family, body string) (valid bool, known bool) {
	networkIdShapesMu.RLock()
	shape, ok := networkIdShapes[family]
	networkIdShapesMu.RUnlock()
	if !ok {
		return false, false
	}
	return shape(body), true
}

// builtinNetworkIdShape is the answer for evm and svm when their families are
// not linked into this binary. Any other family without a registration is
// unknown, and an unknown family's ids must not validate.
func builtinNetworkIdShape(family, body string) bool {
	switch family {
	case "evm":
		return IsEvmNetworkIdBody(body)
	case "svm":
		return IsSvmNetworkIdBody(body)
	}
	return false
}

// IsEvmNetworkIdBody reports whether `body` is the tail of a valid EVM network
// ID — a plain integer chain id ("1" in "evm:1").
//
// Exported so architecture/evm's ChainFamily can delegate to it instead of
// restating the rule. Deliberately unchanged from the pre-registry code,
// including the fact that it accepts a negative integer: config validation
// (common.IsValidNetwork) is what rejects chain id <= 0, and moving that check
// here would reject ids this function has always accepted.
func IsEvmNetworkIdBody(body string) bool {
	_, err := strconv.Atoi(body)
	return err == nil
}

// IsSvmNetworkIdBody reports whether `body` is the tail of a valid SVM network
// ID: "<cluster>" (implicit solana, the back-compat form) or "<chain>:<cluster>".
//
// Exported so architecture/svm's ChainFamily can delegate to it. Every segment
// must be an identifier, so "svm::" and trailing-colon nonsense is rejected,
// and more than two segments is rejected — there is no use for svm:a:b:c today.
func IsSvmNetworkIdBody(body string) bool {
	if body == "" {
		return false
	}
	for _, segment := range strings.Split(body, ":") {
		if segment == "" {
			return false
		}
		for _, r := range segment {
			if !(r == '-' || r == '_' || r == '.' ||
				(r >= 'a' && r <= 'z') ||
				(r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9')) {
				return false
			}
		}
	}
	return strings.Count(body, ":") <= 1
}
