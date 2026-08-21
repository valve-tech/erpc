package util

import (
	"fmt"
	"math"
	"math/rand"
	"strings"

	"github.com/blockchain-data-standards/manifesto/evm"
)

// RandomID returns a value appropriate for a JSON-RPC ID field (i.e int64 type but with 32 bit range) to avoid overflow during conversions and reading/sending to upstreams
func RandomID() int64 {
	return int64(rand.Intn(math.MaxInt32)) // #nosec G404
}

// NormalizeBlockHashHexString normalizes a hex string intended to represent a block hash.
// Rules:
// - Accepts with/without 0x prefix
// - Trims spaces
// - Lowercases hex
// - Pads odd-length by adding a leading '0'
// - If more than 64 hex digits, trims leading zeros; if still >64, returns error
// - Left-pads with zeros to exactly 64 hex digits
// Returns a string with 0x prefix and exactly 64 hex digits.
func NormalizeBlockHashHexString(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("empty block hash")
	}
	trimmed := strings.TrimSpace(s)
	// Strip 0x/0X if present
	if strings.HasPrefix(trimmed, "0x") || strings.HasPrefix(trimmed, "0X") {
		trimmed = trimmed[2:]
	}
	trimmed = strings.ToLower(trimmed)
	// Ensure only hex chars
	for i := 0; i < len(trimmed); i++ {
		ch := trimmed[i]
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			continue
		}
		return "", fmt.Errorf("invalid hex character in block hash")
	}
	// Pad odd length
	if len(trimmed)%2 == 1 {
		trimmed = "0" + trimmed
	}
	// If too long, trim left zeros
	if len(trimmed) > 64 {
		trimmed = strings.TrimLeft(trimmed, "0")
		// Re-check odd length after trimming
		if len(trimmed)%2 == 1 {
			trimmed = "0" + trimmed
		}
	}
	if len(trimmed) > 64 {
		return "", fmt.Errorf("block hash too long")
	}
	// Left-pad to 64
	if len(trimmed) < 64 {
		trimmed = strings.Repeat("0", 64-len(trimmed)) + trimmed
	}
	return "0x" + trimmed, nil
}

// ParseBlockHashHexToBytes normalizes a block hash hex string and converts it to 32 bytes.
func ParseBlockHashHexToBytes(s string) ([]byte, error) {
	norm, err := NormalizeBlockHashHexString(s)
	if err != nil {
		return nil, err
	}
	b, err := evm.HexToBytes(norm)
	if err != nil {
		return nil, fmt.Errorf("invalid block hash hex: %w", err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("invalid block hash length: %d", len(b))
	}
	return b, nil
}

// twoToTheSixtyFour is the first float64 above the uint64 range. uint64 stops
// one below it, and the constant is exact in float64, so `v >= twoToTheSixtyFour`
// is the precise bound. Comparing against math.MaxUint64 would not be: that
// constant rounds UP to this same value in float64, so 2^64 itself would slip
// through and overflow the cast.
const twoToTheSixtyFour = float64(1 << 64)

// checkBlockNumberFloat reports why a JSON number names no block, or nil when
// a uint64 holds it exactly.
func checkBlockNumberFloat(v float64) error {
	switch {
	case math.IsNaN(v):
		return fmt.Errorf("invalid block number: NaN")
	case math.IsInf(v, 0):
		return fmt.Errorf("invalid block number: %v", v)
	case v < 0:
		return fmt.Errorf("invalid block number: %v is negative", v)
	case v >= twoToTheSixtyFour:
		return fmt.Errorf("invalid block number: %v exceeds the largest 64-bit block number", v)
	case v != math.Trunc(v):
		return fmt.Errorf("invalid block number: %v is not a whole number", v)
	}
	return nil
}

// ParseBlockParameter handles complex block parameters including objects with blockHash
// Returns blockNumber string, blockHash bytes, and error
func ParseBlockParameter(param interface{}) (blockNumber string, blockHash []byte, err error) {
	switch v := param.(type) {
	case string:
		if strings.HasPrefix(v, "0x") && len(v) == 66 {
			// This is a block hash
			blockHash, err = parseHexBytes(v)
			if err != nil {
				return "", nil, fmt.Errorf("failed to parse block hash: %w", err)
			}
			return "", blockHash, nil
		}
		// This is a block number or tag
		blockNumber = v
	case float64:
		// JSON numbers are parsed as float64. Reject every value a uint64
		// cannot hold instead of casting it. Go leaves a float-to-integer
		// conversion whose value the target cannot represent
		// "implementation-dependent", so the bare cast made -1, NaN and 1e30
		// name a real block — genesis on this machine, something else on a
		// machine whose conversion traps to the minimum. A negative,
		// fractional, NaN, infinite or oversized number names no block, so the
		// caller gets an error and decides what to do.
		if err := checkBlockNumberFloat(v); err != nil {
			return "", nil, err
		}
		blockNumber = fmt.Sprintf("0x%x", uint64(v))
	case int64:
		if v < 0 {
			return "", nil, fmt.Errorf("invalid block number: %d is negative", v)
		}
		blockNumber = fmt.Sprintf("0x%x", uint64(v))
	case uint64:
		blockNumber = fmt.Sprintf("0x%x", v)
	case map[string]interface{}:
		// Handle complex block parameter object
		if blockHashValue, exists := v["blockHash"]; exists {
			if bh, ok := blockHashValue.(string); ok {
				blockHash, err = ParseBlockHashHexToBytes(bh)
				if err != nil {
					return "", nil, fmt.Errorf("failed to parse blockHash from object: %w", err)
				}
				return "", blockHash, nil
			}
		}
		// If no blockHash, check for blockNumber
		if blockNumberValue, exists := v["blockNumber"]; exists {
			if bn, ok := blockNumberValue.(string); ok {
				blockNumber = bn
			}
		}
		// Check for blockTag
		if blockTagValue, exists := v["blockTag"]; exists {
			if bt, ok := blockTagValue.(string); ok {
				blockNumber = bt
			}
		}
		if blockNumber == "" {
			return "", nil, fmt.Errorf("block parameter object must contain blockHash, blockNumber, or blockTag")
		}
	default:
		return "", nil, fmt.Errorf("invalid block parameter type: %T value: %v", param, param)
	}

	return blockNumber, blockHash, nil
}

// parseHexBytes helper function to parse hex strings to bytes
func parseHexBytes(hexStr string) ([]byte, error) {
	return evm.HexToBytes(hexStr)
}
