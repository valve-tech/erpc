package common

import (
	"errors"
	"testing"

	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These cover the failure exits of LoadConfig and of the TypeScript loader it
// delegates to. Each one is a startup abort an operator sees on the console, so
// the test asserts on the message that reaches them, not just on "an error".

// writeTS puts a TypeScript config on the real filesystem, because esbuild
// reads the path itself and cannot see an afero memory filesystem.
func writeTS(t *testing.T, src string) string {
	t.Helper()
	path := t.TempDir() + "/erpc.ts"
	require.NoError(t, afero.WriteFile(afero.NewOsFs(), path, []byte(src), 0o644))
	return path
}

// withoutLegacyTranslate removes the process-wide translator hook so a test can
// decide for itself whether one runs.
func withoutLegacyTranslate(t *testing.T) {
	t.Helper()
	prevFn, prevLogger := LegacyTranslateFn, LegacyTranslateLogger
	LegacyTranslateFn, LegacyTranslateLogger = nil, nil
	t.Cleanup(func() { LegacyTranslateFn, LegacyTranslateLogger = prevFn, prevLogger })
}

// A TypeScript config that does not parse must abort the load. Falling through
// would start eRPC on a zero-valued config.
func TestLoadConfig_TypeScriptSyntaxErrorAbortsTheLoad(t *testing.T) {
	withoutLegacyTranslate(t)

	path := writeTS(t, "export default { logLevel: 'warn', ")

	cfg, err := LoadConfig(afero.NewOsFs(), path, &DefaultOptions{})

	require.Error(t, err)
	assert.Nil(t, cfg, "a failed load must not hand back a half-built config")
}

// A TypeScript config that throws while it runs must abort the load and say so.
func TestLoadConfig_TypeScriptRuntimeErrorAbortsTheLoad(t *testing.T) {
	withoutLegacyTranslate(t)

	path := writeTS(t, "throw new Error('boom from the config file'); export default {};")

	cfg, err := LoadConfig(afero.NewOsFs(), path, &DefaultOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom from the config file",
		"the operator must see the message their own config threw")
	assert.Nil(t, cfg)
}

// Forgetting the default export is the most common TypeScript config mistake.
// The message has to name the fix, because the file itself looks fine.
func TestLoadConfig_TypeScriptWithoutDefaultExportIsRejected(t *testing.T) {
	withoutLegacyTranslate(t)

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"default export is undefined", "export default undefined;"},
		{"default export is null", "export default null;"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfig(afero.NewOsFs(), writeTS(t, tc.src), &DefaultOptions{})

			require.Error(t, err)
			assert.Contains(t, err.Error(), "must be default exported")
			assert.Nil(t, cfg)
		})
	}
}

// A TypeScript config that exports NOTHING panics instead of returning the
// "must be default exported" error one line further down. Runtime.Exports()
// (common/runtime.go:55) calls ToObject on the `exports` global, and esbuild
// leaves that global null when the module has no exports at all, so sobek
// raises a JS TypeError that unwinds as a Go panic out of
// loadConfigFromTypescript (common/config.go:3254).
//
// This test PINS the defect. If someone guards Exports(), it fails and points
// here: swap it for the clean-error assertion the sibling test uses.
func TestLoadConfig_TypeScriptWithNoExportsPanics(t *testing.T) {
	withoutLegacyTranslate(t)

	path := writeTS(t, "const cfg = { logLevel: 'warn' };")

	assert.Panics(t, func() {
		_, _ = LoadConfig(afero.NewOsFs(), path, &DefaultOptions{})
	}, "today an operator who forgets `export default` gets a panic, not the guidance at config.go:3255")
}

// The TypeScript path decodes with the same strict schema as the YAML path. A
// typo must fail the load instead of being dropped on the floor.
func TestLoadConfig_TypeScriptUnknownFieldIsRejected(t *testing.T) {
	withoutLegacyTranslate(t)

	path := writeTS(t, "export default { logLevl: 'warn' };")

	cfg, err := LoadConfig(afero.NewOsFs(), path, &DefaultOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "ts config decode")
	assert.Contains(t, err.Error(), "logLevl", "the message must name the field that was not recognised")
	assert.Nil(t, cfg)
}

// The legacy translator runs between decode and defaults. When it fails the
// load must stop, and the message must say the failure came from the migration
// rather than from the operator's own file.
func TestLoadConfig_LegacyMigrationErrorAbortsTheLoad(t *testing.T) {
	prevFn, prevLogger := LegacyTranslateFn, LegacyTranslateLogger
	t.Cleanup(func() { LegacyTranslateFn, LegacyTranslateLogger = prevFn, prevLogger })

	sentinel := errors.New("cannot translate scoreMultipliers")
	LegacyTranslateFn = func(*Config) ([]string, error) { return nil, sentinel }
	LegacyTranslateLogger = nil

	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte("logLevel: DEBUG\n"), 0o644))

	cfg, err := LoadConfig(fs, "erpc.yaml", &DefaultOptions{})

	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel, "the translator's own error must stay reachable")
	assert.Contains(t, err.Error(), "legacy config migration")
	assert.Nil(t, cfg)
}

// Every warning the translator produces must reach the logger the process
// installed. A dropped warning is how a silent behaviour change ships.
func TestLoadConfig_LegacyMigrationWarningsReachTheLogger(t *testing.T) {
	prevFn, prevLogger := LegacyTranslateFn, LegacyTranslateLogger
	t.Cleanup(func() { LegacyTranslateFn, LegacyTranslateLogger = prevFn, prevLogger })

	LegacyTranslateFn = func(*Config) ([]string, error) {
		return []string{"routing.scoreMultipliers is deprecated", "group is deprecated"}, nil
	}
	var seen []string
	LegacyTranslateLogger = func(w string) { seen = append(seen, w) }

	fs := afero.NewMemMapFs()
	require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte("logLevel: DEBUG\n"), 0o644))

	_, err := LoadConfig(fs, "erpc.yaml", &DefaultOptions{})

	require.NoError(t, err)
	assert.Equal(t, []string{
		"routing.scoreMultipliers is deprecated",
		"group is deprecated",
	}, seen, "every warning must reach the operator, in order")
}

// An endpoint URL the parser rejects must stop the load. The upstream would
// otherwise get an id derived from a URL nothing can dial.
func TestLoadConfig_UnparseableEndpointAbortsTheLoad(t *testing.T) {
	withoutLegacyTranslate(t)

	fs := afero.NewMemMapFs()
	doc := `
projects:
  - id: main
    upstreams:
      - endpoint: "http://rpc.example.com/%zz"
`
	require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(doc), 0o644))

	cfg, err := LoadConfig(fs, "erpc.yaml", &DefaultOptions{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse endpoint")
	assert.Contains(t, err.Error(), "%zz", "the operator must see which endpoint was rejected")
	assert.Nil(t, cfg)
}

// Defaults run before validation, so a config that survives defaults and fails
// validation must still abort with the validation message.
func TestLoadConfig_ValidationErrorAbortsTheLoad(t *testing.T) {
	withoutLegacyTranslate(t)

	fs := afero.NewMemMapFs()
	doc := `
projects:
  - id: main
    upstreams:
      - id: u1
        endpoint: ""
`
	require.NoError(t, afero.WriteFile(fs, "erpc.yaml", []byte(doc), 0o644))

	cfg, err := LoadConfig(fs, "erpc.yaml", &DefaultOptions{})

	require.Error(t, err)
	assert.Nil(t, cfg)
}
