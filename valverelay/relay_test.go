package valverelay

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/valvebilling"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The credit keys are a contract shared with the api service, so the tests
// name them literally rather than borrowing valvebilling's unexported
// helpers. A rename on either side has to show up as a failure here.
const (
	testAccount    = "acct_1"
	testCeilingKey = "valve:credits:acct_1:ceiling"
	testSpendKey   = "valve:credits:acct_1:spend"
	// 32 characters: valvebilling.MinPepperLength. Synthetic, never a real
	// pepper.
	testPepper = "SYNTHETIC-pepper-for-tests-00000"
)

// The cost of eth_blockNumber in these tests. It comes from the method
// constant table, so every billed test bills exactly this many credits.
const testCost = 3

type fakeCall struct {
	chainID int64
	body    []byte
}

// fakeBackend stands in for eRPC. It records what it was asked and answers
// with whatever the test set.
type fakeBackend struct {
	mu     sync.Mutex
	calls  []fakeCall
	answer []byte
	err    error
	// onForward runs inside Forward, before it returns. It is how a test
	// breaks Redis at the exact moment between the answer and the capture.
	onForward func()
	closed    bool
}

func (f *fakeBackend) Forward(ctx context.Context, chainID int64, body []byte) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{chainID: chainID, body: body})
	f.mu.Unlock()
	if f.onForward != nil {
		f.onForward()
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.answer, nil
}

func (f *fakeBackend) Close() error {
	f.closed = true
	return nil
}

func (f *fakeBackend) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// testBody is the JSON-RPC request the fake backend is asked to forward. It
// lives beside the Request rather than inside it: billing describes the call,
// the forward carries it.
var testBody = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)

// billed is the wrap every call site does: the billing path around one
// backend forward. Bill itself never sees the Backend.
func billed(ctx context.Context, m *valvebilling.Module, b Backend, req Request) (Result, error) {
	return Bill(ctx, m, req, func(ctx context.Context) ([]byte, error) {
		return b.Forward(ctx, req.ChainID, testBody)
	})
}

func okBackend() *fakeBackend {
	return &fakeBackend{answer: []byte(`{"jsonrpc":"2.0","id":1,"result":"0x10"}`)}
}

// newBilling starts a miniredis — which runs the real Lua, so the metering
// decision under test is the same script production runs — and builds an
// ENABLED module against it.
func newBilling(t *testing.T) (*miniredis.Miniredis, *valvebilling.Module) {
	t.Helper()
	mr := miniredis.RunT(t)
	table := valvebilling.NewPriceTable(map[string]int64{"eth_blockNumber": testCost}, 6)
	m, err := valvebilling.New(context.Background(), valvebilling.Config{
		Enabled:  true,
		RedisURL: "redis://" + mr.Addr(),
		Pepper:   testPepper,
	}, table)
	require.NoError(t, err)
	require.True(t, m.Enabled())
	t.Cleanup(func() { _ = m.Close() })
	return mr, m
}

func fundedRequest() Request {
	return Request{
		ChainID:   1,
		Method:    "eth_blockNumber",
		AccountID: testAccount,
		KeyID:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		// A fixed instant, so the script's second and day buckets are
		// deterministic.
		Now: time.Unix(1_700_000_000, 0).UTC(),
	}
}

// Rule D: with the flag clear the path is a pure passthrough.
//
// The module is nil because that is what valvebilling.New returns when
// VALVE_BILLING_ENABLED is clear — asserted here rather than assumed, because
// the whole passthrough guarantee rests on it. A nil module holds no Redis
// connection, so this test proves the absence of billing by construction:
// there is no Redis in it to reach.
func TestBill_PassesThroughWhenBillingIsOff(t *testing.T) {
	m, err := valvebilling.New(context.Background(), valvebilling.Config{Enabled: false}, nil)
	require.NoError(t, err, "a disabled module is the success case, not an error")
	require.Nil(t, m, "off must be the absence of the module")
	require.False(t, m.Enabled())

	b := okBackend()
	req := fundedRequest()
	res, err := billed(context.Background(), m, b, req)
	require.NoError(t, err)

	assert.Equal(t, b.answer, res.Body)
	assert.Nil(t, res.Verdict, "nothing decided anything; a synthesised verdict would be a lie")
	assert.Equal(t, int64(0), res.Billed.Int64())
	assert.NoError(t, res.CaptureErr)

	require.Equal(t, 1, b.callCount())
	assert.Equal(t, req.ChainID, b.calls[0].chainID)
	assert.Equal(t, testBody, b.calls[0].body)
}

// The passthrough must not depend on the caller filling anything in. An empty
// Request with no account, no key and no method still forwards.
func TestBill_PassthroughNeedsNoBillingFields(t *testing.T) {
	b := okBackend()
	res, err := billed(context.Background(), nil, b, Request{ChainID: 42})
	require.NoError(t, err)
	assert.Equal(t, b.answer, res.Body)
	assert.Equal(t, 1, b.callCount())
}

// The billed case end to end: the script allows it, the backend answers, and
// the ledger records exactly the resolved cost.
func TestBill_BillsAnAnsweredRequest(t *testing.T) {
	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1000"))

	b := okBackend()
	res, err := billed(context.Background(), m, b, fundedRequest())
	require.NoError(t, err)

	assert.Equal(t, b.answer, res.Body)
	require.NotNil(t, res.Verdict)
	assert.True(t, res.Verdict.OK(), "got %q", res.Verdict.Code)
	assert.Equal(t, valvebilling.TierFull, res.Verdict.Tier)
	assert.NoError(t, res.CaptureErr)
	assert.Equal(t, int64(testCost), res.Billed.Int64())

	got, err := mr.Get(testSpendKey)
	require.NoError(t, err, "the capture never reached the ledger")
	assert.Equal(t, fmt.Sprint(testCost), got)
}

// Rule 1: capture happens only after the upstream answers, and only when it
// answered. A failed forward costs the customer nothing, and there is no
// refund path if that ever stops being true.
func TestBill_DoesNotCaptureWhenTheForwardFails(t *testing.T) {
	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1000"))

	b := okBackend()
	b.err = errors.New("every upstream refused")

	res, err := billed(context.Background(), m, b, fundedRequest())
	require.Error(t, err, "a failed forward must be reported as a failure")
	assert.Nil(t, res.Body)
	assert.Equal(t, int64(0), res.Billed.Int64())

	assert.False(t, mr.Exists(testSpendKey),
		"the ledger moved for a request that was never answered")
}

// Rule 2: a JSON-RPC error IS an answer. A reverted eth_call is work the
// upstream performed and the customer pays for it. Do not "fix" this.
func TestBill_CapturesAJsonRpcErrorAnswer(t *testing.T) {
	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1000"))

	b := okBackend()
	b.answer = []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":3,"message":"execution reverted"}}`)

	res, err := billed(context.Background(), m, b, fundedRequest())
	require.NoError(t, err)
	assert.Equal(t, b.answer, res.Body)
	assert.Equal(t, int64(testCost), res.Billed.Int64())

	got, err := mr.Get(testSpendKey)
	require.NoError(t, err, "a reverted call was not billed")
	assert.Equal(t, fmt.Sprint(testCost), got)
}

// Rule 3: a capture that fails must not withhold the answer. The customer
// already has it. The failure is reported, and the billed weight is zero so
// an audit row never claims revenue the ledger did not take.
func TestBill_ReturnsTheAnswerWhenCaptureFails(t *testing.T) {
	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1000"))

	b := okBackend()
	// Kill Redis after the authorize and after the answer, in the exact
	// window where only the capture is left.
	b.onForward = func() { mr.Close() }

	res, err := billed(context.Background(), m, b, fundedRequest())
	require.NoError(t, err, "the customer must still get the answer they were served")
	assert.Equal(t, b.answer, res.Body)
	require.Error(t, res.CaptureErr, "a capture against a dead Redis reported success")
	assert.Equal(t, int64(0), res.Billed.Int64(),
		"billed weight must be zero when the ledger did not take the debit")
}

// Rule 4: a Redis failure is not a rejection. It comes back as a plain error,
// never as a *RejectedError, so a caller mapping rejections to 402 cannot
// report an unreachable Redis to a customer as an empty account.
func TestBill_RedisFailureIsNotARejection(t *testing.T) {
	mr, m := newBilling(t)
	mr.Close()

	b := okBackend()
	res, err := billed(context.Background(), m, b, fundedRequest())
	require.Error(t, err)

	var rejected *RejectedError
	assert.False(t, errors.As(err, &rejected),
		"an unreachable Redis was reported as a billing rejection: %v", err)
	assert.Nil(t, res.Body)
	assert.Equal(t, 0, b.callCount(), "the request was forwarded despite an undecided authorization")
}

// A real rejection: the account cannot cover the cost. Nothing is forwarded
// and nothing is billed.
func TestBill_RejectsAnAccountWithoutCredits(t *testing.T) {
	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1"))

	b := okBackend()
	res, err := billed(context.Background(), m, b, fundedRequest())
	require.Error(t, err)

	var rejected *RejectedError
	require.True(t, errors.As(err, &rejected), "want a rejection, got %v", err)
	assert.Equal(t, "no_credits", rejected.Verdict.Code)
	require.NotNil(t, res.Verdict)
	assert.False(t, res.Verdict.OK())
	assert.Equal(t, 0, b.callCount(), "a refused request reached the upstream")
	assert.False(t, mr.Exists(testSpendKey))
	assert.Equal(t, int64(0), res.Billed.Int64())
}

// Authorize must not move spend; only capture does. If it ever did, the
// failed-forward case above would silently start charging.
func TestBill_AuthorizeAloneMovesNoSpend(t *testing.T) {
	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1000"))

	b := okBackend()
	b.err = errors.New("upstream down")
	_, _ = billed(context.Background(), m, b, fundedRequest())

	assert.False(t, mr.Exists(testSpendKey))
}

func TestBill_RefusesWithoutAForward(t *testing.T) {
	_, err := Bill(context.Background(), nil, Request{}, nil)
	require.Error(t, err)
}

// Billing wraps a forward; it does not know what a Backend is. This test is
// the structural claim, written out: the billed path runs against a bare
// closure with no backend anywhere in it.
func TestBill_WrapsAnyForward(t *testing.T) {
	mr, m := newBilling(t)
	require.NoError(t, mr.Set(testCeilingKey, "1000"))

	answer := []byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	res, err := Bill(context.Background(), m, fundedRequest(), func(ctx context.Context) ([]byte, error) {
		return answer, nil
	})
	require.NoError(t, err)
	assert.Equal(t, answer, res.Body)
	assert.Equal(t, int64(testCost), res.Billed.Int64())
}

func TestLoadPriceTable_ReadsTheExportedCorpus(t *testing.T) {
	table, err := LoadPriceTable("../valvebilling/testdata/cost-corpus.json")
	require.NoError(t, err)

	// A row the corpus carries. It was written against the zero address, so
	// asking for any other token still resolves it — through tier 2.
	got := table.Resolve(1, "beacon.eth.v1.beacon.blocks.attestations", "0x1111111111111111111111111111111111111111")
	assert.Equal(t, valvebilling.SourceZeroAddressRow, got.Source)
	assert.Equal(t, big.NewInt(10), got.AmountWei)

	// An unlisted method falls to the default, which came from the file.
	got = table.Resolve(1, "valve_no_such_method", "")
	assert.Equal(t, valvebilling.SourceDefaultConstant, got.Source)
	assert.Equal(t, big.NewInt(6), got.AmountWei)
}

// The export must carry methodCu. It used to live in a second file, and this
// test used to assert that the two files' defaultCu agreed — a real hazard
// while there were two. The monorepo folded methodCu into the corpus, so the
// remaining failure is an export that lacks it, which would otherwise price
// every listed method at the tier-3 default in silence.
func TestLoadPriceTable_RefusesAnExportWithoutTheComputeUnitTable(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/export.json"
	require.NoError(t, writeFile(path, `{"defaultCu":6,"rows":[]}`))

	_, err := LoadPriceTable(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "methodCu")
}

// defaultCu is never defaulted here — see the comment on LoadPriceTable.
func TestLoadPriceTable_RefusesAnExportWithoutADefault(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/export.json"
	require.NoError(t, writeFile(path, `{"methodCu":{"eth_call":12},"rows":[]}`))

	_, err := LoadPriceTable(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "defaultCu")
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
