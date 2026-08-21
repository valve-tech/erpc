package clients

import (
	"context"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// The client factory used to switch on `type: evm` / `type: svm`, so a new
// chain got "unsupported upstream type" no matter how completely it was
// registered elsewhere. These tests pin the registry-driven replacement.
//
// A FAKE family is used on purpose: architecture/evm and architecture/svm
// cannot be imported here (they import this package's siblings and would make
// the test assert on their rules instead of on the factory's wiring). The real
// evm and svm paths are covered where they are linked — see the erpc package
// tests and architecture/svm/chain_family_test.go.

type fakeClientFamily struct {
	name common.NetworkArchitecture
	// refuse names a scheme this family rejects, exercising the optional
	// EndpointSchemeGate. Empty means the family implements no gate behaviour.
	refuse string
}

func (f *fakeClientFamily) Family() common.NetworkArchitecture { return f.name }
func (f *fakeClientFamily) Transport() common.ChainTransport   { return common.TransportJsonRpc }
func (f *fakeClientFamily) Probe(context.Context, common.ProbeCaller) common.ChainProbe {
	return common.ChainProbe{}
}
func (f *fakeClientFamily) Classify(common.ClassifyInput) common.RotateVerdict {
	return common.VerdictServe
}
func (f *fakeClientFamily) ValidateNetworkId(body string) bool { return body != "" }
func (f *fakeClientFamily) MatchesConfiguredChain(configured, observed string) bool {
	return configured == observed
}
func (f *fakeClientFamily) SupportsEndpointScheme(scheme string) (bool, string) {
	if f.refuse != "" && scheme == f.refuse {
		return false, "this family cannot speak " + f.refuse
	}
	return true, ""
}

func registerClientFamily(t *testing.T, f common.ChainFamily) {
	t.Helper()
	if err := common.RegisterChainFamily(f); err != nil {
		t.Fatalf("RegisterChainFamily(%s): %v", f.Family(), err)
	}
	t.Cleanup(func() { common.UnregisterChainFamilyForTest(f.Family()) })
}

func newTestRegistry(t *testing.T) *ClientRegistry {
	t.Helper()
	lg := zerolog.Nop()
	pools, err := NewProxyPoolRegistry(nil, &lg)
	if err != nil {
		t.Fatalf("NewProxyPoolRegistry: %v", err)
	}
	return NewClientRegistry(&lg, "test-project", pools, nil)
}

func TestCreateClient_RegisteredFamilyGetsAnHttpJsonRpcClient(t *testing.T) {
	// The whole point: a chain the factory has never heard of gets a client
	// because it registered a family, not because someone added a case.
	registerClientFamily(t, &fakeClientFamily{name: "testchain"})

	ups := common.NewFakeUpstream("testchain-1")
	ups.Config().Type = common.UpstreamType("testchain")
	ups.Config().Endpoint = "http://testchain-1.localhost:8332"

	c, err := newTestRegistry(t).CreateClient(context.Background(), ups)
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	if c == nil {
		t.Fatal("CreateClient returned no client and no error")
	}
	if c.GetType() != ClientTypeHttpJsonRpc {
		t.Fatalf("client type = %v, want %v", c.GetType(), ClientTypeHttpJsonRpc)
	}
}

func TestCreateClient_UnregisteredTypeIsStillRefused(t *testing.T) {
	// Negative control for the test above. Without it, a factory that built an
	// HTTP client for anything would pass — and a typo'd `type:` would produce
	// an upstream that registers, ranks and takes traffic while speaking a
	// protocol nobody implemented.
	ups := common.NewFakeUpstream("nosuchchain-1")
	ups.Config().Type = common.UpstreamType("nosuchchain")
	ups.Config().Endpoint = "http://nosuchchain-1.localhost"

	_, err := newTestRegistry(t).CreateClient(context.Background(), ups)
	if err == nil {
		t.Fatal("an unregistered upstream type produced a client")
	}
	if !strings.Contains(err.Error(), "unsupported upstream type") {
		t.Fatalf("error = %v, want it to name the unsupported upstream type", err)
	}
}

func TestCreateClient_FamilyCanRefuseAScheme(t *testing.T) {
	// SVM's http-only restriction, expressed through the seam. The factory
	// knows how to build a WebSocket client, so only the family can say that
	// this chain must not get one.
	registerClientFamily(t, &fakeClientFamily{name: "httponly", refuse: "ws"})

	ups := common.NewFakeUpstream("httponly-1")
	ups.Config().Type = common.UpstreamType("httponly")
	ups.Config().Endpoint = "ws://httponly-1.localhost"

	_, err := newTestRegistry(t).CreateClient(context.Background(), ups)
	if err == nil {
		t.Fatal("the family refused ws and a WebSocket client was built anyway")
	}
	if !strings.Contains(err.Error(), "cannot speak ws") {
		t.Fatalf("error = %v, want the family's own reason so an operator can act on it", err)
	}
}

func TestCreateClient_UnknownSchemeIsRefusedEvenWhenTheFamilyAllowsIt(t *testing.T) {
	// A family that implements no scheme gate allows everything, so the
	// factory's own "do I have a client for this?" check is the last line of
	// defence. Without it, a typo'd endpoint would return a nil client.
	registerClientFamily(t, &fakeClientFamily{name: "anyscheme"})

	ups := common.NewFakeUpstream("anyscheme-1")
	ups.Config().Type = common.UpstreamType("anyscheme")
	ups.Config().Endpoint = "ftp://anyscheme-1.localhost"

	c, err := newTestRegistry(t).CreateClient(context.Background(), ups)
	if err == nil {
		t.Fatal("an endpoint scheme with no client implementation was accepted")
	}
	if c != nil {
		t.Fatal("a client was returned alongside the error")
	}
	if !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("error = %v, want it to name the scheme", err)
	}
}
