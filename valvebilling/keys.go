package valvebilling

import "fmt"

// The credit-key names. They address state the api service also writes, so
// they are a shared contract, not an internal choice — changing one here
// without changing the monorepo's meter.ts silently splits the ledger in two.
//
// Every one of these is namespaced by accountId rather than by API key. The
// per-key rate buckets are built inline in Authorize, because they also carry
// a time bucket.

func ceilingKey(accountID string) string {
	return fmt.Sprintf("valve:credits:%s:ceiling", accountID)
}

func pendingKey(accountID string) string {
	return fmt.Sprintf("valve:credits:%s:pending", accountID)
}

func spendKey(accountID string) string {
	return fmt.Sprintf("valve:credits:%s:spend", accountID)
}

func closingKey(accountID string) string {
	return fmt.Sprintf("valve:credits:%s:closing", accountID)
}

func cpsBucketKey(accountID string) string {
	return fmt.Sprintf("valve:credits:%s:cps", accountID)
}

func perRequestLockKey(accountID string) string {
	return fmt.Sprintf("valve:credits:%s:per_request_lock", accountID)
}
