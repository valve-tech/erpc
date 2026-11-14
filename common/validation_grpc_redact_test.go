package common

import (
	"strings"
	"testing"
)

// The wrapped url.Parse error quotes the whole endpoint, key included.
func TestValidateGrpcHealthCheckMount_ParseErrorRedactsEndpoint(t *testing.T) {
	const sentinel = "SENTINEL_KEY_123"
	err := validateGrpcHealthCheckMount("upstream.*.grpc", "grpc://bds.example.invalid/"+sentinel+"\x7f", "grpc.health.v1.Health")
	if err == nil {
		t.Fatal("control: an unparseable endpoint must fail validation")
	}
	if !strings.Contains(err.Error(), "invalid gRPC endpoint") {
		t.Fatalf("control: want the parse failure, got %s", err)
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("the validation error leaks the endpoint key: %s", err)
	}
}
