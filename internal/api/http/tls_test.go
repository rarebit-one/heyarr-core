//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package httpapi_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/config"
)

// Opt-in native TLS (ADR-0072): with both files set the TCP client API is
// served over HTTPS, while the unix socket — the local IPC transport — stays
// plain HTTP. Neither wrapping the socket nor a silent plaintext fallback.
func TestTLSServesHTTPSOnTCPAndPlainOnTheSocket(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)

	h := newHarness(t, withAuthDisabled, func(c *config.Config) {
		c.HTTP.TLS = config.TLS{CertFile: certFile, KeyFile: keyFile}
	}).start(t)

	// The TCP listener speaks TLS. A plain-HTTP client trusting nothing but this
	// test's own certificate completes the handshake and gets the handler.
	httpsClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the test's own throwaway cert
	}}
	resp, err := httpsClient.Get("https://" + h.server.Addr() + "/api/v1/probe")
	if err != nil {
		t.Fatalf("HTTPS GET over TCP: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HTTPS /probe = %d, want 200", resp.StatusCode)
	}

	// A plain-HTTP request to the TLS listener must NOT reach the handler. Go's
	// server answers cleartext on a TLS port with its built-in 400 ("sent an
	// HTTP request to an HTTPS server") rather than an error, so the test is
	// that the handler's 200 is never what comes back.
	plain, err := http.Get("http://" + h.server.Addr() + "/api/v1/probe") //nolint:noctx // a throwaway probe
	if err == nil {
		t.Cleanup(func() { _ = plain.Body.Close() })
		if plain.StatusCode == http.StatusOK {
			t.Error("plain HTTP reached the handler on the TLS listener")
		}
	}

	// The unix socket stays plain HTTP — wrapping it would break every
	// in-process caller, and it never leaves the host.
	socket := h.server.SocketPath()
	if socket == "" {
		t.Fatal("no unix socket was bound")
	}
	unixClient := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	sockResp, err := unixClient.Get("http://unix/api/v1/probe")
	if err != nil {
		t.Fatalf("plain HTTP over the unix socket: %v", err)
	}
	t.Cleanup(func() { _ = sockResp.Body.Close() })
	if sockResp.StatusCode != http.StatusOK {
		t.Errorf("socket /probe = %d, want 200", sockResp.StatusCode)
	}
}

// writeSelfSignedCert writes a throwaway P-256 certificate and key to temp files
// and returns their paths.
func writeSelfSignedCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}
