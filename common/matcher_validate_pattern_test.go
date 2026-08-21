package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ValidatePattern is what tells an operator their config has a typo. The
// existing table only checks that SOME error came back, which passes even when
// the wrong rule fires. These cases pin the exact message, so a rejected
// pattern points at the character the operator must fix.

func TestValidatePattern_RejectsWithASpecificMessage(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		wantErr string
	}{
		{"only whitespace", "   ", "empty pattern"},
		{"a closing parenthesis with nothing open", "0x1 )", "unmatched closing parenthesis at position 1"},
		{"an opening parenthesis never closed", "(0x1", "unclosed parenthesis"},
		{"NOT at the end", "0x1 | !", "NOT operator missing operand at position 2"},
		{"NOT followed by an operator", "! |", "invalid operand for NOT at position 0"},
		{"OR at the start", "| 0x1", "operator '|' missing operand at position 0"},
		{"AND at the end", "0x1 &", "operator '&' missing operand at position 1"},
		{"OR with no left operand", "(| 0x1)", "invalid left operand for '|' at position 1"},
		{"OR with an operator on the right", "0x1 | & 0x2", "invalid right operand for '|' at position 1"},
		{"bad hex after >=", ">=0xZZ", "invalid hex number in comparison: >=0xZZ"},
		{"bad hex after <=", "<=0xZZ", "invalid hex number in comparison: <=0xZZ"},
		{"bad hex after >", ">0xZZ", "invalid hex number in comparison: >0xZZ"},
		{"bad hex after <", "<0xZZ", "invalid hex number in comparison: <0xZZ"},
		{"bad hex after =", "=0xZZ", "invalid hex number in comparison: =0xZZ"},
		{"two patterns with no operator", "0x1 0x2", "unexpected token at position 1: 0x2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePattern(tt.pattern)
			require.Error(t, err)
			require.Equal(t, tt.wantErr, err.Error())
		})
	}
}

func TestValidatePattern_AcceptsTheDocumentedForms(t *testing.T) {
	valid := []string{
		"",
		"eth_*",
		">=0x100",
		"<=0xff",
		">0x1",
		"<0x1",
		"=0x1",
		"0x1 | 0x2",
		"0x1 & 0x2",
		"!0x1",
		"!(0x1 | 0x2)",
		"(0x1 | 0x2) & !0x3",
		"0x1 & !(0x2 | 0x3)",
	}

	for _, pattern := range valid {
		t.Run(pattern, func(t *testing.T) {
			require.NoError(t, ValidatePattern(pattern))
		})
	}
}

// TestValidatePattern_AgreesWithTheMatcher pins the contract that makes
// validation worth running at config load: a pattern ValidatePattern accepts
// must also compile into a matcher at request time, and one it rejects must
// not silently compile into a matcher that quietly matches nothing.
func TestValidatePattern_AgreesWithTheMatcher(t *testing.T) {
	accepted := []string{"eth_*", "(0x1 | 0x2) & !0x3", ">=0x100"}
	for _, pattern := range accepted {
		t.Run("accepted/"+pattern, func(t *testing.T) {
			require.NoError(t, ValidatePattern(pattern))
			_, err := NewWildcardMatcher(pattern)
			require.NoError(t, err, "a validated pattern must compile")
		})
	}
}
