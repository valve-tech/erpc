package valvebilling

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// This file prices the BLOCK SPAN a request asks for.
//
// # Why a second dimension exists at all
//
// The method table in cost.go prices a call by its name. eth_getLogs is 18
// credits whether it asks for one block or five million, and every other
// range method is flat the same way. That is not a rounding error; it is the
// same price for six orders of magnitude more work, and it is the largest
// mispricing in the table.
//
// The design razor argues FOR this rather than against it. An 83-entry method
// table is an enumerated case list, and the razor's primary path is the
// unknown case. A charge proportional to the work a request names demotes that
// table from the main axis to a per-request floor: a method nobody has priced
// yet still bills something proportionate. Nothing here reads a method name.
//
// # No method list, by construction
//
// ExtractSpan looks for a params object carrying fromBlock and toBlock. It
// does not ask which method is being called. eth_getLogs, eth_newFilter,
// trace_filter and eth_query all carry that object today, and the next method
// that does is priced correctly with no edit here.

// Range block tags. These are wire constants, not policy, so they are not
// configurable.
const (
	tagEarliest  = "earliest"
	tagLatest    = "latest"
	tagFinalized = "finalized"
	tagSafe      = "safe"
	tagPending   = "pending"
)

// Heads carries the chain state a tag needs to become a number.
//
// A zero field means "not known". This deliberately mirrors what eRPC itself
// will translate: json_rpc.go resolves "latest" and "finalized" from polled
// state and passes "safe" and "pending" through untouched, because it does not
// track them. This module does not guess what upstream declines to guess.
type Heads struct {
	Latest    int64
	Finalized int64
}

// Span is the block range one request named.
type Span struct {
	// Found is false when the params carry no range at all. Most methods.
	Found bool
	// Resolved is false when a range is present but a tag in it could not be
	// turned into a number with the heads supplied. The caller decides the
	// policy; this package will not invent a span.
	Resolved bool
	// From and To are inclusive and ordered low to high. Meaningful only when
	// Resolved is true.
	From, To int64
}

// Blocks is the inclusive block count, matching eRPC's own blockSpan.
//
// Exactly one valid span does not fit the return type: 0 to MaxInt64 is 2^63
// blocks, so the addition wraps negative. Credits does not read this — it
// measures the gap instead, which cannot overflow — but a caller that logs or
// sums this must know. See TestSpanBlocks_WrapsOnTheOneSpanAnInt64CannotCount.
func (s Span) Blocks() int64 {
	if !s.Resolved {
		return 0
	}
	return s.To - s.From + 1
}

// ExtractSpan finds the block range a request names.
//
// params is the raw JSON-RPC params value, by position (an array) or by name
// (a single object). It walks the top-level elements looking for the first one
// that carries fromBlock or toBlock. A missing end defaults the way the
// JSON-RPC filter object does: fromBlock absent is "latest", toBlock absent is
// "latest".
//
// It does NOT walk nested objects. A filter buried under a key, which some
// client frameworks generate, is not found and pays the flat price. That is a
// recorded limit, not a rule — closing it means walking the whole document,
// which is a larger commitment than the by-name fallback.
//
// It never returns an error. An unparseable params array simply carries no
// range, which is indistinguishable from the overwhelming majority of calls
// that legitimately carry none.
func ExtractSpan(params []byte, heads Heads) Span {
	// JSON-RPC 2.0 allows params as an array OR as a single by-name object.
	// The array is what every EVM client sends, but reading only the array
	// would make the by-name form a free query once Credits refuses an
	// unresolved range: no range found at all, so no charge and no error.
	var elems []json.RawMessage
	if err := json.Unmarshal(params, &elems); err != nil {
		elems = []json.RawMessage{json.RawMessage(params)}
	}
	for _, el := range elems {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(el, &obj); err != nil {
			continue
		}
		rawFrom, hasFrom := obj["fromBlock"]
		rawTo, hasTo := obj["toBlock"]
		if !hasFrom && !hasTo {
			continue
		}
		// The filter object's own defaults. Both ends default to "latest".
		from, okFrom := resolveEnd(rawFrom, hasFrom, heads)
		to, okTo := resolveEnd(rawTo, hasTo, heads)
		if !okFrom || !okTo {
			return Span{Found: true}
		}
		if from > to {
			from, to = to, from
		}
		return Span{Found: true, Resolved: true, From: from, To: to}
	}
	return Span{}
}

// resolveEnd turns one end of the range into a block number.
func resolveEnd(raw json.RawMessage, present bool, heads Heads) (int64, bool) {
	if !present {
		// Absent means "latest" in the filter object.
		if heads.Latest > 0 {
			return heads.Latest, true
		}
		return 0, false
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		if heads.Latest > 0 {
			return heads.Latest, true
		}
		return 0, false
	}
	// Some clients send a JSON number rather than a hex string. Both are seen
	// on the wire, so both are read.
	if len(trimmed) > 0 && trimmed[0] != '"' {
		n, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case tagEarliest:
		return 0, true
	case tagLatest:
		if heads.Latest > 0 {
			return heads.Latest, true
		}
		return 0, false
	case tagFinalized:
		if heads.Finalized > 0 {
			return heads.Finalized, true
		}
		return 0, false
	case tagSafe, tagPending:
		// eRPC does not translate these and neither does this. "safe" sits at
		// an unknown point between finalized and latest, and "pending" is a
		// mempool view nothing here can see. Charging either as "latest" would
		// overbill by an amount nobody can check.
		return 0, false
	}
	n, err := parseHexBlock(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseHexBlock reads a 0x-prefixed block number.
//
// It refuses anything that does not fit a signed 64-bit integer rather than
// saturating. A block number above 2^63 is not a real chain height; it is a
// client sending nonsense or probing, and a saturated value would silently
// become a multi-quintillion-block charge.
func parseHexBlock(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "0x") && !strings.HasPrefix(t, "0X") {
		return 0, fmt.Errorf("valvebilling: block %q is neither a tag nor a 0x number", s)
	}
	n, err := strconv.ParseUint(t[2:], 16, 64)
	if err != nil {
		return 0, fmt.Errorf("valvebilling: block %q: %w", s, err)
	}
	if n > math.MaxInt64 {
		return 0, fmt.Errorf("valvebilling: block %q exceeds a signed 64-bit block height", s)
	}
	return int64(n), nil
}

// RangePrice is the deployment's block-span tariff.
//
// The zero value prices nothing, which is how the feature stays off. That is
// safe here in a way a zero credits-per-second limit is not: a zero range
// tariff is today's behaviour exactly, and today's behaviour undercharges
// rather than removing a bound.
type RangePrice struct {
	// BlocksPerUnit is how many blocks buy one unit. Zero switches the whole
	// range charge off.
	//
	// The number is arbitrary and that is the point — 1,000 and 1,000,000 are
	// equally correct as long as CreditsPerUnit moves with it. What is NOT
	// arbitrary is that a partial unit rounds UP: see Credits.
	BlocksPerUnit int64

	// CreditsPerUnit is the charge for one unit, on top of the method's own
	// flat price. The flat price stays because it pays for the per-request
	// overhead that exists even at a span of one block.
	CreditsPerUnit int64
}

// Enabled reports whether a range charge applies.
func (p RangePrice) Enabled() bool { return p.BlocksPerUnit > 0 && p.CreditsPerUnit > 0 }

// Credits prices a span.
//
// # Partial units round UP
//
// This is the one choice in this file that is not free. With 1,000 blocks to
// the unit, truncation makes every request under 1,000 blocks cost nothing,
// and that is the overwhelming majority of real traffic — every single-block
// eth_getLogs on every chain. A tariff that bills zero for the common case and
// something for the rare one is worse than no tariff at all, because it looks
// like it is working.
//
// # The span is measured as a gap, not as a count
//
// A span can be dishonest. toBlock 0x7fffffffffffffff against fromBlock 0 is
// 2^63 blocks, which is one more than an int64 holds, so counting the blocks
// first wraps to a large NEGATIVE number and the widest request a client can
// name bills nothing. The gap between the ends is always representable — both
// ends are non-negative — so this divides the gap and adds the last unit back.
//
// The identity: the count is gap+1 = (gap/B)*B + r + 1 with 0 <= r < B, so
// 1 <= r+1 <= B and exactly one more unit is always due. That is the round-up
// rule and the overflow fix in the same expression.
//
// # Overflow is refused, not saturated
//
// Only a charge that does not FIT is refused. A span of 2^63 blocks at 5
// credits per 1,000 is 46 quadrillion credits, which is a real number the
// credit gate then refuses on its own merits; erroring there would refuse a
// request this can price. Saturating would post a charge nobody can pay and
// wedge the account, so the case that truly does not fit returns an error.
func (p RangePrice) Credits(span Span) (int64, error) {
	if !p.Enabled() {
		return 0, nil
	}
	// Resolved is tested before Found on purpose. The two fields can express a
	// combination ExtractSpan never builds (resolved but not found), and
	// reading Found first would price such a Span as free. Resolved is the
	// stronger statement, so it decides first.
	if !span.Resolved {
		if !span.Found {
			// The overwhelming majority of calls: no block range at all. The
			// method's flat price is the whole charge.
			return 0, nil
		}
		// A range is present and this cannot price it. Answering zero here
		// would decide the fee, and the client picks the input that lands on
		// this path: "toBlock":"safe" returns whole-chain data, and eRPC
		// forwards the tag untouched. One word would buy the tariff off.
		//
		// So this refuses instead, and the caller makes the policy choice in
		// one visible place: reject the request, or fall back to the flat
		// price and COUNT it. Either is defensible. Silence is not.
		return 0, fmt.Errorf(
			"valvebilling: the request names a block range this cannot resolve to numbers; " +
				"it is not free, and the caller must decide whether to refuse it or bill it flat")
	}

	gap := span.To - span.From
	if gap < 0 {
		// Backwards, or built by hand from values ExtractSpan never produces.
		// ExtractSpan orders the ends, so this invents no charge.
		return 0, nil
	}
	units := gap/p.BlocksPerUnit + 1
	// units <= 0 means the +1 wrapped, which needs one block to the unit and
	// the widest span. The second test is the product.
	if units <= 0 || units > math.MaxInt64/p.CreditsPerUnit {
		return 0, fmt.Errorf(
			"valvebilling: blocks %d to %d at %d credits per %d blocks overflows a 64-bit charge; "+
				"the request is refused rather than billed a saturated amount",
			span.From, span.To, p.CreditsPerUnit, p.BlocksPerUnit)
	}
	return units * p.CreditsPerUnit, nil
}

// The range tariff's environment variables. Both are optional together and
// required together: naming one without the other is a half-configured tariff,
// and the half that is missing would silently be zero.
const (
	EnvBlocksPerUnit  = "VALVE_BLOCKS_PER_UNIT"
	EnvCreditsPerUnit = "VALVE_CREDITS_PER_BLOCK_UNIT"
)

// LoadRangePriceFromEnv reads the block-span tariff.
//
// Neither variable set returns the zero RangePrice, which prices nothing. That
// absence is a real deployment choice — it is stock behaviour — so unlike the
// tier limits it is not an error.
func LoadRangePriceFromEnv() (RangePrice, error) {
	rawBlocks, hasBlocks := os.LookupEnv(EnvBlocksPerUnit)
	rawCredits, hasCredits := os.LookupEnv(EnvCreditsPerUnit)
	if !hasBlocks && !hasCredits {
		return RangePrice{}, nil
	}
	if !hasBlocks || !hasCredits {
		return RangePrice{}, fmt.Errorf(
			"valvebilling: %s and %s must be set together; one alone leaves the other at zero "+
				"and the range charge silently off", EnvBlocksPerUnit, EnvCreditsPerUnit)
	}
	blocks, err := positiveInt(EnvBlocksPerUnit, rawBlocks)
	if err != nil {
		return RangePrice{}, err
	}
	credits, err := positiveInt(EnvCreditsPerUnit, rawCredits)
	if err != nil {
		return RangePrice{}, err
	}
	return RangePrice{BlocksPerUnit: blocks, CreditsPerUnit: credits}, nil
}

func positiveInt(name, raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("valvebilling: %s is empty; an empty value is not a zero and not a default", name)
	}
	v, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("valvebilling: %s=%q is not a whole number: %w", name, trimmed, err)
	}
	if v <= 0 {
		return 0, fmt.Errorf(
			"valvebilling: %s=%d must be greater than zero; to switch the range charge off, "+
				"unset both %s and %s", name, v, EnvBlocksPerUnit, EnvCreditsPerUnit)
	}
	return v, nil
}
