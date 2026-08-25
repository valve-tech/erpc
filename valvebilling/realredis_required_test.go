package valvebilling

import (
	"os"
	"os/exec"
	"testing"
)

// EnvAllowNoRedis lets a machine without redis-server run the rest of the
// suite on purpose.
//
// It is an OPT-OUT, not an opt-in, and that direction is the whole point.
const EnvAllowNoRedis = "VALVE_ALLOW_NO_REDIS"

// TestRealRedisIsAvailable fails when redis-server is missing, instead of
// letting the tests that need it disappear.
//
// # Why this test exists
//
// Three files in this package — settle_test.go, overdraft_test.go and
// canary_test.go — carry the evidence for claims nothing else can support.
// The settle crash matrix, the measured overdraft bound and the pepper canary
// all run against a real redis-server, because miniredis and real Redis are
// already known to disagree in this package: a cost above int64 makes real
// Redis answer no_credits and makes miniredis fail to compile the script.
//
// Every one of those files calls t.Skip when the binary is absent. That is
// the correct behaviour for one test. It is the wrong behaviour for the
// SUITE, because a machine without redis-server then runs none of the
// evidence and still prints ok. The run reads exactly like a pass.
//
// This repository has already been bitten by that shape once: a go test run
// that truncates at its timeout also prints a result that reads like a pass.
// A skipped test and a truncated run fail the same way — quietly, and in the
// direction of looking healthy.
//
// So the default is a FAILURE and the escape hatch is explicit. A developer
// without Redis sets VALVE_ALLOW_NO_REDIS=1 and accepts a weaker run. A CI
// machine that loses the binary in an image change gets a red build naming
// what it lost, rather than a green one that proves less than it did
// yesterday.
func TestRealRedisIsAvailable(t *testing.T) {
	if _, err := exec.LookPath("redis-server"); err == nil {
		return
	}
	if v, ok := os.LookupEnv(EnvAllowNoRedis); ok && v != "" && v != "0" {
		t.Skipf("redis-server is absent and %s is set; the settle, overdraft "+
			"and canary evidence did NOT run", EnvAllowNoRedis)
	}
	t.Fatalf(
		"redis-server is not on PATH, so every test that needs a real server skipped "+
			"and this run proves less than it appears to. The settle crash matrix, the "+
			"measured overdraft bound and the pepper canary all need it. Install it, or "+
			"set %s=1 to accept a weaker run on purpose.", EnvAllowNoRedis)
}
