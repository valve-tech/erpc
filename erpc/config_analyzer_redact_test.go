package erpc

import (
	"context"
	"strings"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
)

// The wrapped url.Parse error quotes the whole endpoint, key included.
func TestValidateUpstreamEndpoints_ParseErrorRedactsEndpoint(t *testing.T) {
	const sentinel = "SENTINEL_KEY_123"
	t.Setenv("ERPC_IGNORE_LOCAL_ENDPOINT_VALIDATION", "true")
	cfg := &common.Config{
		Metrics: &common.MetricsConfig{},
		Projects: []*common.ProjectConfig{{
			Id: "prj1",
			Upstreams: []*common.UpstreamConfig{{
				Id:       "ups1",
				Endpoint: "https://rpc.example.invalid/v3/" + sentinel + "\x7f",
			}},
		}},
	}
	err := validateUpstreamEndpoints(context.Background(), cfg, zerolog.Nop())
	if err == nil {
		t.Fatal("control: an unparseable endpoint must fail validation")
	}
	if !strings.Contains(err.Error(), "invalid endpoint URL") {
		t.Fatalf("control: want the parse failure, got %s", err)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("the validation error leaks the endpoint key: %s", err)
	}
}
