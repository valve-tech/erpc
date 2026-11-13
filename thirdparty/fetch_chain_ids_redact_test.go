package thirdparty

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// An endpoint that net/http cannot parse comes back as a parse error that
// quotes the whole URL, key included. The chain-id probe logs that error.
const fetchRedactSentinel = "SENTINEL_KEY_123"

func assertFetchLogRedacted(t *testing.T, out, wantMsg string) {
	t.Helper()
	// Control: the probe logged the build failure.
	if !strings.Contains(out, wantMsg) || !strings.Contains(out, "failed to create request") {
		t.Fatalf("control: want the build-request warning, got %s", out)
	}
	if strings.Contains(out, fetchRedactSentinel) {
		t.Errorf("the chain-id probe log leaks the endpoint key: %s", out)
	}
}

func TestChainstackFetchChainIDs_BuildErrorRedactsAuthKey(t *testing.T) {
	var buf bytes.Buffer
	lg := zerolog.New(&buf)
	v := &ChainstackVendor{}
	nodes := []*ChainstackNode{{
		ID:     "nd-1",
		Status: "running",
		Details: ChainstackNodeDetails{
			HTTPSEndpoint: "https://nd-1.example.invalid",
			AuthKey:       fetchRedactSentinel + "\x7f",
		},
	}}
	v.fetchChainIDs(context.Background(), &lg, nodes)
	assertFetchLogRedacted(t, buf.String(), "Chainstack nodes")
}

func TestQuicknodeFetchChainIDs_BuildErrorRedactsEndpoint(t *testing.T) {
	var buf bytes.Buffer
	lg := zerolog.New(&buf)
	v := &QuicknodeVendor{}
	eps := []*QuicknodeEndpoint{{
		ID:      "ep-1",
		HttpUrl: "https://ep-1.example.invalid/" + fetchRedactSentinel + "\x7f",
	}}
	v.fetchChainIDs(context.Background(), &lg, eps)
	assertFetchLogRedacted(t, buf.String(), "QuickNode endpoints")
}
