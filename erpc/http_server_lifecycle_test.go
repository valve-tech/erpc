package erpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

// Everything here runs once, at startup, on the path an operator only sees
// through a process that either serves traffic or exits. A misconfiguration that
// is silently accepted here becomes a node that looks healthy and is not: TLS
// that never loaded the client CA, or a listener that was never started because
// its port was missing from the config.

// writeSelfSignedCert writes a usable cert/key pair into dir and returns their
// paths. It is a real ECDSA key and a real DER certificate, so tls.LoadX509KeyPair
// exercises its actual parse rather than a stub.
func writeSelfSignedCert(t *testing.T, dir string) (certFile, keyFile string, certPEM []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "erpc-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	require.NoError(t, os.WriteFile(certFile, certPEM, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyPEM, 0o600))
	return certFile, keyFile, certPEM
}

func tlsServer(cfg *common.TLSConfig) *HttpServer {
	return &HttpServer{serverCfg: &common.ServerConfig{TLS: cfg}}
}

// TestCreateTLSConfig_LoadsTheKeyPairAndPinsAModernFloor covers the happy path.
// The TLS 1.2 floor is the property worth pinning: it is the one line standing
// between this listener and a downgrade to TLS 1.0/1.1, and nothing else in the
// process would notice if it disappeared.
func TestCreateTLSConfig_LoadsTheKeyPairAndPinsAModernFloor(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeSelfSignedCert(t, dir)

	cfg, err := tlsServer(&common.TLSConfig{
		Enabled:  true,
		CertFile: certFile,
		KeyFile:  keyFile,
	}).createTLSConfig()

	require.NoError(t, err)
	require.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	require.Len(t, cfg.Certificates, 1)
	require.Nil(t, cfg.ClientCAs, "no CA configured means no client-cert requirement")
	require.Equal(t, tls.NoClientCert, cfg.ClientAuth)
	require.False(t, cfg.InsecureSkipVerify)
}

// TestCreateTLSConfig_RefusesToStartWithAnUnreadableKeyPair. Startup must fail
// loudly: a server that came up without the cert it was told to use would serve
// plaintext on a port the operator believes is encrypted.
func TestCreateTLSConfig_RefusesToStartWithAnUnreadableKeyPair(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeSelfSignedCert(t, dir)

	_, err := tlsServer(&common.TLSConfig{
		Enabled:  true,
		CertFile: filepath.Join(dir, "absent.pem"),
		KeyFile:  keyFile,
	}).createTLSConfig()
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to load TLS certificate and key")

	// A key that is not the cert's key parses individually and still must not
	// be accepted as a pair.
	otherDir := t.TempDir()
	_, otherKey, _ := writeSelfSignedCert(t, otherDir)
	_, err = tlsServer(&common.TLSConfig{
		Enabled:  true,
		CertFile: certFile,
		KeyFile:  otherKey,
	}).createTLSConfig()
	require.Error(t, err, "a cert and a key from different pairs must be rejected")
}

// TestCreateTLSConfig_RequiresClientCertsOnceACaIsConfigured. Naming a CA is how
// an operator asks for mutual TLS. If the CA loaded but ClientAuth stayed
// permissive, the port would accept anonymous clients while the config says
// otherwise — the failure mode is an open door, not an outage.
func TestCreateTLSConfig_RequiresClientCertsOnceACaIsConfigured(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, certPEM := writeSelfSignedCert(t, dir)

	caFile := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caFile, certPEM, 0o600))

	cfg, err := tlsServer(&common.TLSConfig{
		Enabled:  true,
		CertFile: certFile,
		KeyFile:  keyFile,
		CAFile:   caFile,
	}).createTLSConfig()

	require.NoError(t, err)
	require.NotNil(t, cfg.ClientCAs)
	require.Equal(t, tls.RequireAndVerifyClientCert, cfg.ClientAuth,
		"a configured CA must make client certificates mandatory, not optional")
}

// TestCreateTLSConfig_RejectsACaFileItCannotUse covers both CA failures: the
// file is missing, and the file exists but holds no certificate. The second is
// the dangerous one — AppendCertsFromPEM reports failure by returning false, and
// an unchecked result leaves an empty pool that verifies nothing.
func TestCreateTLSConfig_RejectsACaFileItCannotUse(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeSelfSignedCert(t, dir)

	base := func(caFile string) *common.TLSConfig {
		return &common.TLSConfig{
			Enabled: true, CertFile: certFile, KeyFile: keyFile, CAFile: caFile,
		}
	}

	_, err := tlsServer(base(filepath.Join(dir, "absent-ca.pem"))).createTLSConfig()
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to read CA file")

	notPEM := filepath.Join(dir, "garbage-ca.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("this is not a certificate"), 0o600))
	_, err = tlsServer(base(notPEM)).createTLSConfig()
	require.Error(t, err, "a CA file with no parseable certificate must not yield an empty trust pool")
	require.Contains(t, err.Error(), "failed to parse CA certificate")
}

// TestCreateTLSConfig_CarriesInsecureSkipVerifyThrough. The flag exists for
// self-signed upstream setups; it must land where it was asked for and nowhere
// else, so both settings are checked rather than only the dangerous one.
func TestCreateTLSConfig_CarriesInsecureSkipVerifyThrough(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile, _ := writeSelfSignedCert(t, dir)

	cfg, err := tlsServer(&common.TLSConfig{
		Enabled: true, CertFile: certFile, KeyFile: keyFile, InsecureSkipVerify: true,
	}).createTLSConfig()
	require.NoError(t, err)
	require.True(t, cfg.InsecureSkipVerify)
}

// TestStart_RefusesAConfigWithNoListener. Both listeners off is a config that
// can never serve a request. Returning nil here would produce a process that
// starts, reports healthy to its supervisor and answers nothing.
func TestStart_RefusesAConfigWithNoListener(t *testing.T) {
	logger := log.Logger
	srv := &HttpServer{serverCfg: &common.ServerConfig{}}

	err := srv.Start(&logger)
	require.Error(t, err)
	require.Contains(t, err.Error(), "at least one of server.listenV4 or server.listenV6")
}

// TestStart_RefusesAnEnabledListenerWithNoPort covers the half-configured case
// for each family. listenV4 without httpPortV4 is a config an operator can
// easily write; the listener has no address to bind, so it must be an error and
// not a silently skipped listener.
func TestStart_RefusesAnEnabledListenerWithNoPort(t *testing.T) {
	logger := log.Logger

	v4 := &HttpServer{
		serverV4: &http.Server{},
		serverCfg: &common.ServerConfig{
			ListenV4: util.BoolPtr(true), HttpHostV4: util.StringPtr("127.0.0.1"),
		},
	}
	err := v4.Start(&logger)
	require.Error(t, err)
	require.Contains(t, err.Error(), "server.httpPortV4 is not configured")

	v6 := &HttpServer{
		serverV6: &http.Server{},
		serverCfg: &common.ServerConfig{
			ListenV6: util.BoolPtr(true), HttpHostV6: util.StringPtr("::1"),
		},
	}
	err = v6.Start(&logger)
	require.Error(t, err)
	require.Contains(t, err.Error(), "server.httpPortV6 is not configured")
}

// TestNewHttpServer_ResolvesResponseHeadersAtStartupAndSkipsEmptyOnes covers
// the custom response-header surface. The values are env-expanded once, at
// startup — an operator who references an unset variable gets that header
// dropped rather than sent empty, because an empty header on every response is
// indistinguishable from a header the proxy in front stripped.
func TestNewHttpServer_ResolvesResponseHeadersAtStartupAndSkipsEmptyOnes(t *testing.T) {
	t.Setenv("ERPC_TEST_REGION", "eu-central-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.Logger

	srv, err := NewHttpServer(ctx, &logger, &common.ServerConfig{
		ResponseHeaders: map[string]string{
			"X-Region":  "${ERPC_TEST_REGION}",
			"X-Cluster": "$ERPC_TEST_UNSET_VARIABLE",
			"X-Fixed":   "constant",
		},
	}, nil, nil, nil, nil)
	require.NoError(t, err)

	require.Equal(t, map[string]string{
		"X-Region": "eu-central-1",
		"X-Fixed":  "constant",
	}, srv.resolvedResponseHeaders,
		"an unset variable must drop the header, not emit it empty")
}

// TestStart_RefusesToServePlaintextWhenTlsCannotBeLoaded is the failure an
// operator meets after rotating a certificate to a path that is not there.
// Start must abort: a server that fell back to plaintext on port 443 would
// serve every request unencrypted while the config still said TLS.
func TestStart_RefusesToServePlaintextWhenTlsCannotBeLoaded(t *testing.T) {
	logger := log.Logger
	port := 0
	missing := filepath.Join(t.TempDir(), "not-written.pem")

	v4 := &HttpServer{
		serverV4: &http.Server{},
		serverCfg: &common.ServerConfig{
			ListenV4:   util.BoolPtr(true),
			HttpHostV4: util.StringPtr("127.0.0.1"),
			HttpPortV4: &port,
			TLS:        &common.TLSConfig{Enabled: true, CertFile: missing, KeyFile: missing},
		},
	}
	err := v4.Start(&logger)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to create TLS config")
	require.Nil(t, v4.serverV4.TLSConfig, "no TLS config may be installed when loading it failed")

	v6 := &HttpServer{
		serverV6: &http.Server{},
		serverCfg: &common.ServerConfig{
			ListenV6:   util.BoolPtr(true),
			HttpHostV6: util.StringPtr("::1"),
			HttpPortV6: &port,
			TLS:        &common.TLSConfig{Enabled: true, CertFile: missing, KeyFile: missing},
		},
	}
	err = v6.Start(&logger)
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to create TLS config")
}

// TestStart_ReportsAPortItCannotBind is the other startup failure, and the one
// that proves Start really waits on its listener goroutine. A port already in
// use has to end the process, not leave it running with no listener — the
// supervisor would keep a dead node in rotation.
func TestStart_ReportsAPortItCannotBind(t *testing.T) {
	logger := log.Logger

	// Hold the port for the length of the test so the bind below cannot win.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port

	srv := &HttpServer{
		serverV4: &http.Server{},
		serverCfg: &common.ServerConfig{
			ListenV4:   util.BoolPtr(true),
			HttpHostV4: util.StringPtr("127.0.0.1"),
			HttpPortV4: &port,
		},
	}

	err = srv.Start(&logger)
	require.Error(t, err, "Start must return the listener's failure, not nil")
	require.Contains(t, err.Error(), "IPv4 server error")
	require.Contains(t, err.Error(), "address already in use")
}

// TestStart_ServesOverTlsAndReturnsCleanlyWhenShutDown is the success path the
// three failure tests above never reach. Two claims matter to an operator, and
// neither is visible from a process that started without complaint.
//
// First, a listener configured for TLS must actually speak TLS. Nothing else in
// eRPC would notice a server that loaded a certificate and then served
// plaintext on port 443, so the test drives a real HTTPS client that trusts
// only this certificate, and then proves a plaintext request to the same port
// is not served.
//
// Second, Start must return nil after Shutdown. Start blocks on its listener
// goroutines, so a supervisor reads its return value to tell an orderly stop
// from a crash. Returning an error on a clean shutdown would restart a node an
// operator deliberately drained.
func TestStart_ServesOverTlsAndReturnsCleanlyWhenShutDown(t *testing.T) {
	logger := log.Logger
	dir := t.TempDir()
	certFile, keyFile, certPEM := writeSelfSignedCert(t, dir)

	// Take a port, then release it so Start can bind the same number.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := held.Addr().(*net.TCPAddr).Port
	require.NoError(t, held.Close())

	srv := &HttpServer{
		serverV4: &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`served`))
			}),
			ReadHeaderTimeout: 5 * time.Second,
		},
		serverCfg: &common.ServerConfig{
			ListenV4:   util.BoolPtr(true),
			HttpHostV4: util.StringPtr("127.0.0.1"),
			HttpPortV4: &port,
			TLS: &common.TLSConfig{
				Enabled:  true,
				CertFile: certFile,
				KeyFile:  keyFile,
			},
		},
	}

	started := make(chan error, 1)
	go func() { started <- srv.Start(&logger) }()

	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(certPEM))
	tlsClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}
	url := fmt.Sprintf("https://127.0.0.1:%d/", port)

	var body []byte
	require.Eventually(t, func() bool {
		resp, gerr := tlsClient.Get(url)
		if gerr != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusOK
	}, 20*time.Second, 50*time.Millisecond,
		"a TLS-enabled listener never answered an HTTPS request")
	require.Equal(t, "served", string(body))

	// A plaintext request to the same port must not be served. This is the half
	// that catches a listener that loaded the certificate and then ignored it.
	plainResp, plainErr := (&http.Client{Timeout: 5 * time.Second}).
		Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if plainErr == nil {
		defer plainResp.Body.Close()
		plainBody, _ := io.ReadAll(plainResp.Body)
		require.NotEqual(t, "served", string(plainBody),
			"the listener answered a plaintext request while TLS was configured")
	}

	require.NoError(t, srv.Shutdown(&logger))

	select {
	case err := <-started:
		require.NoError(t, err,
			"a deliberate shutdown must not look like a crash to the supervisor")
	case <-time.After(30 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}
