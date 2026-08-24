package valvebilling

import (
	"context"
	"fmt"
	"math/big"

	"github.com/redis/go-redis/v9"
)

// Capture debits what the request actually cost, AFTER the upstream answered.
//
// This is deliberately not folded into Authorize. Authorize decides; Capture
// charges. A request whose upstream failed costs the customer nothing, and
// there is no refund path to fall back on if the two were merged — the money
// would already be gone.
//
// A JSON-RPC error from the upstream IS still captured. A reverted call is
// work the upstream performed. Do not "fix" that.
//
// Callers must not treat a Capture error as a reason to withhold the answer.
// The customer already has it, and by this point the only question is whether
// the ledger saw the debit. Log it, count it, and record zero weight in
// analytics so the audit row never claims revenue the ledger did not take.
func Capture(ctx context.Context, rdb redis.Cmdable, accountID string, cost *big.Int) error {
	if cost == nil {
		return fmt.Errorf("valvebilling: capture called with nil cost for account %q", accountID)
	}
	if accountID == "" {
		return fmt.Errorf("valvebilling: capture called with an empty accountId")
	}
	// Nothing to move, and INCRBY 0 would still cost a round trip.
	if cost.Sign() == 0 {
		return nil
	}
	if cost.Sign() < 0 {
		// A negative capture would be a credit, which this path must never
		// issue. Refunds are the api's business, not the relay's.
		return fmt.Errorf("valvebilling: refusing a negative capture of %s for account %q", cost, accountID)
	}
	if !cost.IsInt64() {
		// INCRBY takes a 64-bit signed integer. A cost that does not fit is a
		// pricing-table error, not something to silently truncate.
		return fmt.Errorf("valvebilling: capture of %s for account %q exceeds INCRBY's range", cost, accountID)
	}
	if err := rdb.IncrBy(ctx, spendKey(accountID), cost.Int64()).Err(); err != nil {
		return fmt.Errorf("valvebilling: capture failed for account %q: %w", accountID, err)
	}
	return nil
}
