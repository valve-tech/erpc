package common

import (
	"testing"
)

// FuzzWildcardMatch drives the selector parser with a hostile pattern and a
// hostile value. Both are untrusted: the value is a client method name, and
// the pattern reaches the matcher straight from the client's
// `X-ERPC-Use-Upstream` header (see headerDirectiveUseUpstream), so a client
// picks the expression the recursive-descent parser has to chew on.
//
// The seeds are REAL patterns from matcher_test.go and from the shipped
// configs.
func FuzzWildcardMatch(f *testing.F) {
	seeds := []struct {
		pattern string
		value   string
	}{
		{"*", "eth_getLogs"},
		{"eth_*", "eth_getLogs"},
		{"!eth_*", "eth_getLogs"},
		{"eth_getLogs | eth_call", "eth_call"},
		{"eth_* & !eth_send*", "eth_getLogs"},
		{"(eth_* | net_*) & !*_subscribe", "net_version"},
		{">=0x100", "0x200"},
		{"<1000", "999"},
		{"evm:1", "evm:1"},
		{"<empty>", ""},
		{"", "anything"},
		{"((((a))))", "a"},
		{"a & (b | c) & !d", "b"},
		{"(0x095ea7b3 | 0x23b872dd) & !(0xdeadbeef | 0xbeefdead)", "0x095ea7b3"},
		{"!(latest|safe|finalized) & >=0x1111", "0x2222"},
		{"!(>=0x100 & <=0x200)", "0x150"},
	}
	for _, s := range seeds {
		f.Add(s.pattern, s.value)
	}

	f.Fuzz(func(t *testing.T, pattern, value string) {
		_, _ = WildcardMatch(pattern, value)
		_, _ = MatchesSelector(pattern, value, []string{"tier:hot", "region:eu"})
		_, _ = SelectorAdmits(pattern, value, []string{"tier:hot", "region:eu"})
		_ = ValidatePattern(pattern)
	})
}
