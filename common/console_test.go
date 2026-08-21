package common

import (
	"bytes"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// An operator debugging a selection policy has exactly one tool inside the
// script: console.log. These tests pin that each console method reaches the
// eRPC log at the level it names, that several arguments arrive space-joined,
// and that a call below the configured level costs nothing.

// runPolicyScript evaluates src in a real eRPC JS runtime and returns whatever
// the script wrote to the global logger.
func runPolicyScript(t *testing.T, level zerolog.Level, src string) string {
	t.Helper()

	buf := useTestLogger(t, level)

	rt, err := NewRuntime()
	require.NoError(t, err)

	_, err = rt.Evaluate(src)
	require.NoError(t, err)

	return buf.String()
}

// useTestLogger points the global logger at a buffer at the given level and
// restores both the logger and the global level afterwards.
func useTestLogger(t *testing.T, level zerolog.Level) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	prevLogger := log.Logger
	prevLevel := zerolog.GlobalLevel()
	log.Logger = zerolog.New(buf).Level(level)
	zerolog.SetGlobalLevel(level)
	t.Cleanup(func() {
		log.Logger = prevLogger
		zerolog.SetGlobalLevel(prevLevel)
	})
	return buf
}

func TestConsole_EachMethodLogsAtItsOwnLevel(t *testing.T) {
	tests := []struct {
		method    string
		wantLevel string
	}{
		{"debug", "debug"},
		{"info", "info"},
		{"log", "info"},
		{"trace", "trace"},
		{"warn", "warn"},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			out := runPolicyScript(t, zerolog.TraceLevel, "console."+tt.method+"('hello')")
			require.Contains(t, out, `"level":"`+tt.wantLevel+`"`)
			require.Contains(t, out, `"message":"hello"`)
		})
	}
}

func TestConsole_JoinsArgumentsWithASpace(t *testing.T) {
	out := runPolicyScript(t, zerolog.TraceLevel, "console.log('upstream', 'alchemy-1', 42, true)")
	require.Contains(t, out, `"message":"upstream alchemy-1 42 true"`)
}

func TestConsole_SkipsACallBelowTheConfiguredLevel(t *testing.T) {
	out := runPolicyScript(t, zerolog.WarnLevel, "console.debug('noisy'); console.trace('noisier')")
	require.Empty(t, out, "a debug call must emit nothing when the logger is at warn")

	out = runPolicyScript(t, zerolog.WarnLevel, "console.warn('kept')")
	require.Contains(t, out, `"message":"kept"`)
}

func TestConsole_ReturnsUndefinedSoAScriptCanChain(t *testing.T) {
	useTestLogger(t, zerolog.TraceLevel)

	rt, err := NewRuntime()
	require.NoError(t, err)

	v, err := rt.Evaluate("console.log('x')")
	require.NoError(t, err)
	require.True(t, v.Equals(rt.ToValue(nil)) || v.String() == "undefined",
		"console.log must answer undefined, got %q", v.String())
}

// TestRuntime_ExposesProcessEnvToAPolicyScript pins the other half of the
// script environment: a policy may branch on an environment variable, which is
// how an operator ships one policy file across staging and production.
func TestRuntime_ExposesProcessEnvToAPolicyScript(t *testing.T) {
	t.Setenv("ERPC_CONSOLE_TEST_VAR", "on")

	rt, err := NewRuntime()
	require.NoError(t, err)

	v, err := rt.Evaluate("process.env.ERPC_CONSOLE_TEST_VAR")
	require.NoError(t, err)
	require.Equal(t, "on", v.String())
}
