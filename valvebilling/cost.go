package valvebilling

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync"
)

// ZeroAddress is the all-zero 20-byte address. A (chain, method) row written
// against it prices that method for ANY token — it is a distinct fallback
// tier, not a default row.
const ZeroAddress = "0x0000000000000000000000000000000000000000"

// CostSource records which tier answered a lookup.
//
// It exists so a test can assert the PATH as well as the number. Without it a
// case that was meant to exercise the zero-address fallback can silently fall
// through to the method constant, land on the same value by coincidence, and
// pass — which is exactly how a pricing bug ships.
type CostSource string

const (
	// SourceExactRow is tier 1: a row for this exact (chain, method, token).
	SourceExactRow CostSource = "exact"
	// SourceZeroAddressRow is tier 2: a (chain, method) row written against
	// the zero address, resolving for any token.
	SourceZeroAddressRow CostSource = "zero_address"
	// SourceMethodConstant is tier 3 where the method is in the compute-unit
	// table.
	SourceMethodConstant CostSource = "method_constant"
	// SourceDefaultConstant is tier 3 where it is not.
	SourceDefaultConstant CostSource = "default_constant"
)

// Cost is the answer to one pricing lookup.
type Cost struct {
	// AmountWei is a big integer because the pricing table's amount_wei is
	// Postgres numeric and exceeds 2^53. Decoding it through a float would
	// destroy it silently.
	AmountWei *big.Int
	// HoldLockUntilSettle controls where the per-request lock is released.
	// Tier 3 never sets it.
	HoldLockUntilSettle bool
	// Source is which tier answered.
	Source CostSource
}

// PriceRow is one row of shared.method_pricing.
// The JSON names are the corpus's, which is camelCase. They are NOT the
// Postgres column names (chain_id, amount_wei, ...) — the generator renames
// them on the way out, and this struct reads the fixture, not the database.
//
// "AmountWei" keeps the corpus's spelling, but the name is a misnomer inherited
// from the column: every live row is credits-per-request, not token wei. The
// largest value in the live table is 50. Token-wei amounts belong to the x402
// path, which never reaches this code.
type PriceRow struct {
	ChainID             int64     `json:"chainId"`
	Method              string    `json:"method"`
	TokenAddress        string    `json:"tokenAddress"`
	AmountWei           WeiString `json:"amountWei"`
	HoldLockUntilSettle bool      `json:"holdLockUntilSettle"`
}

// WeiString is the amount as it must travel: a JSON STRING.
//
// Unmarshalling REFUSES a JSON number rather than accepting a rounded one.
//
// The justification is not the one originally given. No live pricing row comes
// anywhere near 2^53 — the whole table maxes out at 50 — so nothing is being
// rounded today. The reason to keep this strict is that the column is Postgres
// numeric, nothing constrains a future row, and a rounded read would be
// silent: the wrong price bills the wrong amount and nothing goes red.
//
// The monorepo has one live instance of the pattern this guards against, at
// beacon-handler.ts:276, which does Number(amountWei) on the credits path.
// Harmless at 50. Do not mirror it here.
type WeiString string

func (w *WeiString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || b[0] != '"' {
		return fmt.Errorf(
			"valvebilling: amount_wei must be a JSON string, got %s — a JSON number "+
				"has already lost precision above 2^53 by the time it reaches here; "+
				"fix the generator, not the reader", string(b))
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*w = WeiString(s)
	return nil
}

// Big parses the decimal string into a big integer.
func (w WeiString) Big() (*big.Int, error) {
	n, ok := new(big.Int).SetString(string(w), 10)
	if !ok {
		return nil, fmt.Errorf("valvebilling: amount_wei %q is not a base-10 integer", string(w))
	}
	return n, nil
}

// PriceTable is the in-memory pricing cache. Lookups are O(1) and safe for
// concurrent use; Load replaces the whole table atomically so a refresh never
// exposes a half-updated map.
type PriceTable struct {
	mu        sync.RWMutex
	rows      map[string]Cost
	methodCU  map[string]int64
	defaultCU int64
}

// NewPriceTable builds an empty table with the tier-3 constants.
//
// defaultCU is a parameter, not a constant in this file, because it is data
// the monorepo owns. Hard-coding it here is how the two implementations drift:
// the monorepo's own method-pricing-cache.ts header currently states the wrong
// value, and two separate readers copied that wrong value before the source
// was checked.
func NewPriceTable(methodCU map[string]int64, defaultCU int64) *PriceTable {
	cu := make(map[string]int64, len(methodCU))
	for k, v := range methodCU {
		cu[k] = v
	}
	return &PriceTable{rows: map[string]Cost{}, methodCU: cu, defaultCU: defaultCU}
}

// cacheKey composes the lookup key.
//
// The token address folds to lowercase. The method and the chain id DO NOT.
// That asymmetry is the contract, not an oversight: an EIP-55 mixed-case
// address must hit the same row as the all-lowercase one that was written,
// while JSON-RPC method names are case-sensitive. Folding the method too
// would change pricing with no error anywhere.
func cacheKey(chainID int64, method, token string) string {
	return fmt.Sprintf("%d|%s|%s", chainID, method, strings.ToLower(token))
}

// Load replaces the table's rows.
func (t *PriceTable) Load(rows []PriceRow) error {
	next := make(map[string]Cost, len(rows))
	for i, r := range rows {
		amount, err := r.AmountWei.Big()
		if err != nil {
			return fmt.Errorf("valvebilling: row %d (%d, %s, %s): %w", i, r.ChainID, r.Method, r.TokenAddress, err)
		}
		next[cacheKey(r.ChainID, r.Method, r.TokenAddress)] = Cost{
			AmountWei:           amount,
			HoldLockUntilSettle: r.HoldLockUntilSettle,
		}
	}
	t.mu.Lock()
	t.rows = next
	t.mu.Unlock()
	return nil
}

// Resolve prices one call. It never fails: tier 3 always answers.
func (t *PriceTable) Resolve(chainID int64, method, tokenAddress string) Cost {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if hit, ok := t.rows[cacheKey(chainID, method, tokenAddress)]; ok {
		hit.Source = SourceExactRow
		return hit
	}
	if hit, ok := t.rows[cacheKey(chainID, method, ZeroAddress)]; ok {
		hit.Source = SourceZeroAddressRow
		return hit
	}
	// Tier 3. holdLockUntilSettle is false here by construction — only a real
	// pricing row can opt a method into settle-mode.
	if cu, ok := t.methodCU[method]; ok {
		return Cost{AmountWei: big.NewInt(cu), Source: SourceMethodConstant}
	}
	return Cost{AmountWei: big.NewInt(t.defaultCU), Source: SourceDefaultConstant}
}
