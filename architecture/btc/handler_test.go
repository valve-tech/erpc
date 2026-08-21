package btc

import (
	"context"
	"errors"
	"testing"

	"github.com/erpc/erpc/common"
)

func TestArchitectureHandler_IsRegistered(t *testing.T) {
	// The networks registry refuses to prepare a network whose architecture
	// has no handler, so without this registration `architecture: btc` never
	// gets past network construction however well the family is wired.
	h, err := common.GetArchitectureHandler(Architecture)
	if err != nil {
		t.Fatalf("GetArchitectureHandler(btc): %v", err)
	}
	if h == nil {
		t.Fatal("registered btc handler is nil")
	}
}

func TestArchitectureHandler_HooksPassThrough(t *testing.T) {
	// Pass-through means the pipeline decides, not btc. The failure this
	// guards is a hook that quietly SWALLOWS an error or a response: the
	// request would then report success with nothing in it.
	h := &ArchitectureHandler{}
	ctx := context.Background()
	boom := errors.New("upstream said no")
	resp := &common.NormalizedResponse{}

	if handled, r, err := h.HandleProjectPreForward(ctx, nil, nil); handled || r != nil || err != nil {
		t.Errorf("HandleProjectPreForward short-circuited the request: handled=%v resp=%v err=%v", handled, r, err)
	}
	if handled, r, err := h.HandleNetworkPreForward(ctx, nil, nil, nil); handled || r != nil || err != nil {
		t.Errorf("HandleNetworkPreForward short-circuited the request: handled=%v resp=%v err=%v", handled, r, err)
	}
	if handled, r, err := h.HandleUpstreamPreForward(ctx, nil, nil, nil, false); handled || r != nil || err != nil {
		t.Errorf("HandleUpstreamPreForward short-circuited the request: handled=%v resp=%v err=%v", handled, r, err)
	}
	if gotResp, gotErr := h.HandleNetworkPostForward(ctx, nil, nil, resp, boom); gotResp != resp || !errors.Is(gotErr, boom) {
		t.Errorf("HandleNetworkPostForward altered the outcome: resp=%v err=%v", gotResp, gotErr)
	}
	if gotResp, gotErr := h.HandleUpstreamPostForward(ctx, nil, nil, nil, resp, boom, false); gotResp != resp || !errors.Is(gotErr, boom) {
		t.Errorf("HandleUpstreamPostForward altered the outcome: resp=%v err=%v", gotResp, gotErr)
	}
}

func TestArchitectureHandler_ErrorExtractorClaimsNothing(t *testing.T) {
	// The extractors are composed across every registered architecture and the
	// first non-nil answer wins, so a btc extractor that guessed would also be
	// offered every EVM and SVM error. Claiming nothing keeps it out of their
	// way until bitcoind's codes are mapped against real nodes.
	ex := (&ArchitectureHandler{}).NewJsonRpcErrorExtractor()
	if ex == nil {
		t.Fatal("NewJsonRpcErrorExtractor returned nil; the composite extractor would panic on it")
	}
	if err := ex.Extract(nil, nil, nil, nil); err != nil {
		t.Fatalf("btc extractor claimed an error it cannot have understood: %v", err)
	}
}
