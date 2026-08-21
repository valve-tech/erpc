package consensus

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The S3 exporter is where misbehavior forensics go. If the constructor accepts
// a destination it cannot actually write to, erpc runs for weeks believing it
// archives disputes and the archive stays empty. So the constructor must probe
// the bucket at startup and refuse anything it cannot reach.

// s3StubServer answers the one call the constructor makes (HeadBucket) and
// records the paths it saw, so a test can prove path-style addressing is used.
type s3StubServer struct {
	*httptest.Server
	headPaths atomic.Value // []string
	auths     atomic.Value // []string
	status    int
}

func newS3StubServer(t *testing.T, status int) *s3StubServer {
	t.Helper()
	s := &s3StubServer{status: status}
	s.headPaths.Store([]string{})
	s.auths.Store([]string{})
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			appendTo(&s.headPaths, r.URL.Path)
			appendTo(&s.auths, r.Header.Get("Authorization"))
			w.WriteHeader(s.status)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)
	return s
}

func appendTo(v *atomic.Value, s string) {
	prev, _ := v.Load().([]string)
	v.Store(append(append([]string{}, prev...), s))
}

func (s *s3StubServer) paths() []string {
	v, _ := s.headPaths.Load().([]string)
	return v
}

func (s *s3StubServer) lastAuth() string {
	v, _ := s.auths.Load().([]string)
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

func s3Cfg(path, endpoint string, creds *common.AwsAuthConfig) *common.MisbehaviorsDestinationConfig {
	return &common.MisbehaviorsDestinationConfig{
		Type:        common.MisbehaviorsDestinationTypeS3,
		Path:        path,
		FilePattern: "{timestampMs}-{method}-{networkId}",
		S3: &common.S3FlushConfig{
			MaxRecords:    10,
			MaxSize:       1 << 20,
			FlushInterval: common.Duration(time.Hour),
			Region:        "us-east-1",
			Endpoint:      endpoint,
			Credentials:   creds,
			ContentType:   "application/jsonl",
		},
	}
}

func TestNewS3MisbehaviorExporter(t *testing.T) {
	lg := zerolog.Nop()

	t.Run("a reachable bucket produces a working exporter", func(t *testing.T) {
		srv := newS3StubServer(t, http.StatusOK)

		exp, err := newS3MisbehaviorExporter(
			s3Cfg("s3://catches/integrity/", srv.URL, &common.AwsAuthConfig{
				Mode:            "secret",
				AccessKeyID:     "key",
				SecretAccessKey: "secret",
			}), &lg)

		require.NoError(t, err)
		require.NotNil(t, exp)
		t.Cleanup(func() { _ = exp.Close() })

		assert.Equal(t, "catches", exp.bucket)
		assert.Equal(t, "integrity/", exp.keyPrefix,
			"a prefix without a trailing slash must gain one, or every key merges into its parent")
		assert.NotNil(t, exp.batches)
		assert.Contains(t, srv.paths(), "/catches",
			"path-style addressing keeps bucket resolution off DNS for S3-compatible providers")
	})

	t.Run("a bucket it cannot reach is refused", func(t *testing.T) {
		// The whole point of the startup probe: fail loudly now rather than
		// silently drop every record later.
		srv := newS3StubServer(t, http.StatusForbidden)

		exp, err := newS3MisbehaviorExporter(
			s3Cfg("s3://catches/integrity/", srv.URL, &common.AwsAuthConfig{
				Mode: "secret", AccessKeyID: "key", SecretAccessKey: "secret",
			}), &lg)

		require.Error(t, err)
		assert.Nil(t, exp)
		assert.Contains(t, err.Error(), "unable to access S3 bucket catches")
	})

	t.Run("a nil configuration is refused", func(t *testing.T) {
		exp, err := newS3MisbehaviorExporter(nil, &lg)
		require.Error(t, err)
		assert.Nil(t, exp)
	})

	t.Run("an empty path is refused as unconfigured", func(t *testing.T) {
		// "not configured" and "malformed" are different operator mistakes
		// and must not collapse into the same message.
		exp, err := newS3MisbehaviorExporter(s3Cfg("", "", nil), &lg)
		require.Error(t, err)
		assert.Nil(t, exp)
		assert.Contains(t, err.Error(), "empty S3 path configuration")
	})

	t.Run("a path that is not an s3 URI is refused", func(t *testing.T) {
		exp, err := newS3MisbehaviorExporter(s3Cfg("/var/log/catches", "", nil), &lg)
		require.Error(t, err)
		assert.Nil(t, exp)
		assert.Contains(t, err.Error(), "s3://")
	})

	t.Run("a bucket-only path keeps an empty key prefix", func(t *testing.T) {
		srv := newS3StubServer(t, http.StatusOK)

		exp, err := newS3MisbehaviorExporter(
			s3Cfg("s3://catches", srv.URL, &common.AwsAuthConfig{
				Mode: "secret", AccessKeyID: "key", SecretAccessKey: "secret",
			}), &lg)

		require.NoError(t, err)
		t.Cleanup(func() { _ = exp.Close() })
		assert.Equal(t, "catches", exp.bucket)
		assert.Equal(t, "", exp.keyPrefix)
	})

	t.Run("each credentials mode signs with its own key", func(t *testing.T) {
		// The operator picks the mode in config. A mode the constructor wires
		// to the wrong source signs with the wrong key and every upload gets
		// a 403 — long after startup, where nobody is watching. Each mode
		// here carries a distinct access key, and the assertion reads the key
		// back off the signed request.
		srv := newS3StubServer(t, http.StatusOK)

		credsFile := filepath.Join(t.TempDir(), "credentials")
		require.NoError(t, os.WriteFile(credsFile,
			[]byte("[default]\naws_access_key_id = FILEKEY\naws_secret_access_key = filesecret\n"), 0o600))

		t.Setenv("AWS_ACCESS_KEY_ID", "ENVKEY")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "envsecret")

		for _, tc := range []struct {
			name    string
			creds   *common.AwsAuthConfig
			wantKey string
		}{
			{"secret", &common.AwsAuthConfig{Mode: "secret", AccessKeyID: "SECRETKEY", SecretAccessKey: "s"}, "SECRETKEY"},
			{"file", &common.AwsAuthConfig{Mode: "file", CredentialsFile: credsFile, Profile: "default"}, "FILEKEY"},
			{"env", &common.AwsAuthConfig{Mode: "env"}, "ENVKEY"},
			{"unknown mode falls back to the default chain", &common.AwsAuthConfig{Mode: "somethingelse"}, "ENVKEY"},
			{"no credentials block", nil, "ENVKEY"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				exp, err := newS3MisbehaviorExporter(s3Cfg("s3://catches/p/", srv.URL, tc.creds), &lg)
				require.NoError(t, err)
				require.NotNil(t, exp)
				t.Cleanup(func() { _ = exp.Close() })
				assert.Contains(t, srv.lastAuth(), "Credential="+tc.wantKey+"/",
					"the bucket probe must be signed with the key this mode names")
			})
		}
	})
}

// TestCreateMisbehaviorExporter_S3 pins the wiring from operator config to the
// S3 exporter, including the "export is best-effort" contract: a broken
// destination disables export instead of failing startup.
func TestCreateMisbehaviorExporter_S3(t *testing.T) {
	lg := zerolog.Nop()

	t.Run("a reachable S3 destination yields an S3 exporter", func(t *testing.T) {
		srv := newS3StubServer(t, http.StatusOK)
		cfg := s3Cfg("s3://catches/p/", srv.URL, &common.AwsAuthConfig{
			Mode: "secret", AccessKeyID: "key", SecretAccessKey: "secret",
		})

		exp := createMisbehaviorExporter(cfg, &lg)
		require.NotNil(t, exp)
		s3exp, ok := exp.(*s3MisbehaviorExporter)
		require.True(t, ok, "an s3 destination must not silently become a file exporter")
		_ = s3exp.Close()
	})

	t.Run("an unreachable S3 destination disables export", func(t *testing.T) {
		srv := newS3StubServer(t, http.StatusForbidden)
		cfg := s3Cfg("s3://catches/p/", srv.URL, &common.AwsAuthConfig{
			Mode: "secret", AccessKeyID: "key", SecretAccessKey: "secret",
		})

		// Compared against the interface, not with assert.Nil: returning a
		// nil *s3MisbehaviorExporter through the interface produces a
		// non-nil interface, and every `if exporter != nil` caller would
		// then call methods on it.
		exp := createMisbehaviorExporter(cfg, &lg)
		assert.True(t, exp == nil,
			"export is best-effort; a broken archive must not take the process down, "+
				"and it must return a true nil interface: %#v", exp)
	})

	t.Run("an empty path disables export", func(t *testing.T) {
		exp := createMisbehaviorExporter(&common.MisbehaviorsDestinationConfig{Type: "s3"}, &lg)
		assert.True(t, exp == nil)
		assert.True(t, createMisbehaviorExporter(nil, &lg) == nil)
	})
}
