package util

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

func EvmNetworkId(chainId interface{}) string {
	return fmt.Sprintf("evm:%d", chainId)
}

// SvmNetworkId derives the canonical "svm:..." network ID. When chain is
// empty or "solana", the format stays "svm:<cluster>" — preserving every
// pre-multi-chain config's network ID and cache key. For any other chain
// the format is "svm:<chain>:<cluster>" so forks (Fogo, Eclipse, custom)
// can coexist with Solana in a single eRPC instance.
func SvmNetworkId(chain, cluster string) string {
	if chain == "" || chain == "solana" {
		return "svm:" + cluster
	}
	return "svm:" + chain + ":" + cluster
}

var validIdentifierRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func IsValidIdentifier(s string) bool {
	return validIdentifierRegex.MatchString(s)
}

// IsValidNetworkId reports whether s is a well-formed "<family>:<body>"
// network ID.
//
// The family owns the body's shape, so this function only splits the two apart
// and asks: a chain family registers its own rule (network_id_shape.go) and
// evm/svm keep theirs as the builtin fallback. Adding a chain therefore needs
// no edit here.
func IsValidNetworkId(s string) bool {
	family, body, found := strings.Cut(s, ":")
	if !found {
		return false
	}
	if valid, known := networkIdShapeVerdict(family, body); known {
		return valid
	}
	return builtinNetworkIdShape(family, body)
}

// CanonicalNetworkId returns the one id of the network s names, and whether s
// names a valid network. An EVM chain id is a plain integer, so "evm:01" and
// "evm:0001" both name "evm:1": every consumer of the id (provider
// ignoreNetworks/onlyNetworks, lazy network creation, upstream ids) must see
// that one form. Every other family has one spelling per network, so its id
// is canonical exactly when IsValidNetworkId accepts it.
func CanonicalNetworkId(s string) (string, bool) {
	if strings.HasPrefix(s, "evm:") {
		chainId, err := strconv.ParseInt(s[4:], 10, 64)
		if err != nil {
			return "", false
		}
		return EvmNetworkId(chainId), true
	}
	if IsValidNetworkId(s) {
		return s, true
	}
	return "", false
}

var counters = make(map[string]int)
var countersMutex = sync.Mutex{}

func IncrementAndGetIndex(parts ...string) string {
	countersMutex.Lock()
	defer countersMutex.Unlock()
	counterKey := strings.Join(parts, "</@/>")
	if _, ok := counters[counterKey]; !ok {
		counters[counterKey] = 0
	}
	counters[counterKey]++
	return strconv.Itoa(counters[counterKey])
}
