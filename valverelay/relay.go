package valverelay

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/erpc/erpc/valvebilling"
)

// Request describes one JSON-RPC call to the billing path. It is what billing
// needs to know about the call, not the call itself — the request body belongs
// to the forward, which billing wraps and does not read.
//
// AccountID, KeyID, Limits and CUCost are all parameters because the things
// that produce them — auth, API-key resolution, tiering policy — are not in
// this fork. See the package doc.
type Request struct {
	// ChainID selects the network. This seam is EVM-only.
	ChainID int64
	// Method is the JSON-RPC method name, used to price the call. It is NOT
	// re-parsed out of the request body: the caller has already parsed the
	// request, and parsing it twice is how two answers to "what method is
	// this?" appear.
	Method string

	// AccountID is the paying account. It names the credit keys.
	AccountID string
	// KeyID is the HASHED api key — valvebilling.Module.HashKey's output,
	// never the key itself.
	KeyID string

	// TokenAddress prices token-specific methods. The zero address is the
	// any-token row; empty means the same lookup with no token.
	TokenAddress string
	// CUCost feeds the compute-unit gates. Zero leaves them unexercised.
	CUCost int64
	// Limits are the policy numbers for this request. A zero Limits disables
	// every rate gate in the script; the credit-balance gate still applies.
	Limits valvebilling.Limits
	// Methods are the per-second per-method buckets. Empty skips that gate.
	Methods []valvebilling.MethodBucket

	// Now is the instant the request is billed at. Zero means time.Now().
	Now time.Time
}

// Result is one answered request.
type Result struct {
	// Body is the answer. It is what the customer gets.
	Body []byte

	// Verdict is what Redis decided. It is nil when billing did not run,
	// which is the only honest value: a synthesised "allowed" verdict would
	// put a decision in an audit row that nothing made.
	Verdict *valvebilling.Verdict

	// Billed is what the ledger actually took. It is zero when billing did
	// not run and zero when the capture failed, so an audit row built from
	// this can never claim revenue Redis did not record.
	Billed *big.Int

	// CaptureErr is a capture that did not land. The answer in Body is still
	// valid and must still be delivered — the customer has already been
	// served and there is no refund path in the other direction. Log it,
	// count it, and leave Billed at zero.
	CaptureErr error
}

// RejectedError is billing refusing the request. It is the ONLY error this
// package returns that means "do not forward, and tell the customer why".
//
// Every other error — Redis unreachable, a malformed verdict, a failed
// forward — is a plain error. That split is what stops an unreachable Redis
// from being reported to a customer as an out-of-credit account: a caller that
// maps *RejectedError to 402 and everything else to 5xx cannot make that
// mistake, because a Redis fault never arrives in this shape.
type RejectedError struct {
	Verdict valvebilling.Verdict
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("valverelay: billing refused the request: %s (tier %s)", e.Verdict.Code, e.Verdict.Tier)
}

// Forward is the operation billing wraps: it produces the answer, or an error
// if it produced none. Backend.Forward is the obvious implementation, and
// valverelay.Bill deliberately does not take a Backend — forwarding stands on
// its own, and billing is a wrapper around it.
//
// A caller that must stop blocking on billing deletes the wrapper and calls
// the backend directly. That is a call-site change, not a rewrite.
type Forward func(ctx context.Context) ([]byte, error)

// Bill prices, authorizes, forwards and captures one request, in that order.
//
// With a nil (disabled) module it calls forward and does nothing else.
// valvebilling.New returns a nil *Module when VALVE_BILLING_ENABLED is clear,
// so "off" is the absence of the object rather than a branch somebody can
// reach with the wrong value.
//
// The order is not negotiable:
//
//   - Capture runs only AFTER the forward answered, and only when it did. A
//     failed upstream costs the customer nothing, and there is no refund path
//     to lean on if the two steps were merged.
//   - A JSON-RPC error in the answer is still captured. A reverted call is
//     work the upstream performed.
//   - A capture that fails does not withhold the answer. It comes back in
//     Result.CaptureErr with Billed at zero.
//   - A Redis failure in Authorize is not a rejection. It is returned as a
//     plain error, and the caller decides whether to fail open or closed.
func Bill(ctx context.Context, m *valvebilling.Module, req Request, forward Forward) (Result, error) {
	if forward == nil {
		return Result{}, fmt.Errorf("valverelay: nothing to forward to")
	}
	if !m.Enabled() {
		body, err := forward(ctx)
		if err != nil {
			return Result{}, err
		}
		return Result{Body: body, Billed: big.NewInt(0)}, nil
	}

	cost, err := m.ResolveCost(req.ChainID, req.Method, req.TokenAddress)
	if err != nil {
		return Result{}, fmt.Errorf("valverelay: pricing %s on chain %d: %w", req.Method, req.ChainID, err)
	}

	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	verdict, err := m.Authorize(ctx, valvebilling.AuthorizeInput{
		AccountID: req.AccountID,
		KeyID:     req.KeyID,
		Now:       now,
		Cost:      cost.AmountWei,
		CUCost:    req.CUCost,
		Limits:    req.Limits,
		Methods:   req.Methods,
	})
	if err != nil {
		// Deliberately NOT a *RejectedError. Redis being unreachable is not
		// an out-of-credit customer.
		return Result{}, fmt.Errorf("valverelay: authorizing %s for account %q: %w", req.Method, req.AccountID, err)
	}
	if !verdict.OK() {
		return Result{Verdict: &verdict, Billed: big.NewInt(0)}, &RejectedError{Verdict: verdict}
	}

	body, err := forward(ctx)
	if err != nil {
		// No answer, no capture. Authorize moved no spend, so there is
		// nothing to unwind.
		return Result{Verdict: &verdict, Billed: big.NewInt(0)}, err
	}

	res := Result{Body: body, Verdict: &verdict, Billed: big.NewInt(0)}
	if err := m.Capture(ctx, req.AccountID, cost.AmountWei); err != nil {
		// The customer already has the answer. Report the fault and zero
		// billed weight; do not turn this into a failed request.
		res.CaptureErr = err
		return res, nil
	}
	res.Billed = new(big.Int).Set(cost.AmountWei)
	return res, nil
}
