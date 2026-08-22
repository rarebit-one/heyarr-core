package downloads

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The credential reaches the WIRE intact, colon and all.
//
// This is the end of the round trip #123 is about: the assertions in
// internal/providers prove the password survives configuration, and this one
// proves it survives being sent. They are separate because the corruption used
// to happen between them.
const rpcPasswordWithAColon = "hunter2:the-real-part-8e91c4"

// A password containing a colon arrives at Transmission unaltered.
//
// Asserted through net/http's own BasicAuth decoder rather than by inspecting
// the header string, because RFC 7617 is where the colon genuinely IS a
// separator — the base64 payload is "user:pass". A password with a colon in it
// is legal there and decodes correctly; the bug was never in the protocol, it
// was in Heyarr parsing its own configuration.
func TestAPasswordWithAColonReachesTheWireIntact(t *testing.T) {
	type credentials struct {
		user, pass string
		ok         bool
	}
	var got credentials

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.user, got.pass, got.ok = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"success","arguments":{"rpc-version":18,` +
			`"rpc-version-minimum":14,"version":"4.1.3","download-dir":"/downloads"}}`))
	}))
	defer srv.Close()

	client := constructFor(t, providers.Entry{
		Name:     "a-download-client",
		Type:     string(providers.KindTransmission),
		Endpoint: srv.URL,
		Credential: &providers.CredentialEntry{
			Username: "heyarr",
			Password: providers.Secret(rpcPasswordWithAColon),
		},
	})

	health := client.Check(context.Background())
	if !health.Healthy {
		t.Fatalf("the probe should have succeeded: %+v", health)
	}

	if !got.ok {
		t.Fatal("no basic-auth credential was sent at all")
	}
	if got.user != "heyarr" {
		t.Errorf("username on the wire = %q, want %q", got.user, "heyarr")
	}
	// Byte for byte. The old parser sent user "hunter2" with password
	// "the-real-part-8e91c4" here.
	if got.pass != rpcPasswordWithAColon {
		t.Fatalf("password on the wire = %q, want %q", got.pass, rpcPasswordWithAColon)
	}
}

// A Transmission with authentication off sends no credential, which is an
// ordinary supported deployment on a trusted network and must stay one.
func TestNoCredentialSendsNoAuthorizationHeader(t *testing.T) {
	var sawAuthorization bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization") != ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"success","arguments":{"rpc-version":18,` +
			`"rpc-version-minimum":14,"version":"4.1.3","download-dir":"/downloads"}}`))
	}))
	defer srv.Close()

	client := constructFor(t, providers.Entry{
		Name:     "a-download-client",
		Type:     string(providers.KindTransmission),
		Endpoint: srv.URL,
	})
	if h := client.Check(context.Background()); !h.Healthy {
		t.Fatalf("the probe should have succeeded: %+v", h)
	}
	if sawAuthorization {
		t.Error("an unauthenticated Transmission must be sent no Authorization header")
	}
}

// The pre-#123 spelling — a bare api_key password with the assumed username —
// still authenticates exactly as it did, on the wire.
//
// This is the back-compatibility assertion at the level that matters: not that
// the value parses, but that the same bytes still arrive at the daemon.
func TestTheLegacyBareAPIKeyStillAuthenticatesAsBefore(t *testing.T) {
	var user, pass string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"success","arguments":{"rpc-version":18,` +
			`"rpc-version-minimum":14,"version":"4.1.3","download-dir":"/downloads"}}`))
	}))
	defer srv.Close()

	client := constructFor(t, providers.Entry{
		Name:     "a-download-client",
		Type:     string(providers.KindTransmission),
		Endpoint: srv.URL,
		APIKey:   "hunter2",
	})
	if h := client.Check(context.Background()); !h.Healthy {
		t.Fatalf("the probe should have succeeded: %+v", h)
	}

	if user != "transmission" {
		t.Errorf("username = %q; a bare api_key keeps the assumed username", user)
	}
	if pass != "hunter2" {
		t.Errorf("password = %q, want %q", pass, "hunter2")
	}
}

// Constructing and probing a download client must not log either half of the
// credential — asserted by scanning captured output, not by reading the code.
func TestConstructingAndProbingLogsNoCredential(t *testing.T) {
	const username = "heyarr-DO-NOT-LEAK-username"

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// A server that refuses, so the error path is exercised too: an
	// authentication failure is exactly when somebody logs the credential.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	entry := providers.Entry{
		Name:     "a-download-client",
		Type:     string(providers.KindTransmission),
		Endpoint: srv.URL,
		Credential: &providers.CredentialEntry{
			Username: username,
			Password: providers.Secret(rpcPasswordWithAColon),
		},
	}
	resolved, err := providers.Validate([]providers.Entry{entry})
	if err != nil {
		t.Fatal(err)
	}
	provider, handled, err := Constructor(resolved[0], func() time.Time { return fixedNow })
	if err != nil || !handled {
		t.Fatalf("Constructor: handled=%v err=%v", handled, err)
	}

	health := provider.Check(context.Background())
	// Three shapes, deliberately: the raw config block, the resolved struct,
	// and the credential ON ITS OWN. The third is not redundant — a struct
	// reaches slog through encoding/json, and a Credential logged directly
	// reaches it through LogValue, so a redaction removed from one of those
	// would be invisible to a test that only used the other.
	log.Info("probed", "config", entry, "resolved", resolved[0], "health", health)
	log.Info("probed", slog.Any("credential", resolved[0].Credential))
	log.Error("probe failed", "detail", health.Detail)

	output := buf.String()
	if strings.Contains(output, rpcPasswordWithAColon) {
		t.Fatalf("the password reached the log:\n%s", output)
	}
	if strings.Contains(output, username) {
		t.Fatalf("the username reached the log:\n%s", output)
	}
	// The credential must not reach the HEALTH DETAIL either, which is served
	// on GET /api/v1/providers.
	if strings.Contains(health.Detail, rpcPasswordWithAColon) ||
		strings.Contains(health.Detail, username) {
		t.Fatalf("the credential reached the health detail: %q", health.Detail)
	}
	// A leak test that passed against empty output would prove nothing.
	if !strings.Contains(output, "a-download-client") {
		t.Errorf("expected the provider to be logged at all, got:\n%s", output)
	}
}

// A credential in the endpoint is still refused, so the base64 form is the only
// way one reaches the wire.
func TestUserinfoInTheEndpointIsStillRefused(t *testing.T) {
	_, err := providers.Validate([]providers.Entry{{
		Name:     "a-download-client",
		Type:     string(providers.KindTransmission),
		Endpoint: "http://heyarr:" + url.QueryEscape(rpcPasswordWithAColon) + "@transmission.invalid:9091",
	}})
	if err == nil {
		t.Fatal("expected a refusal — a credential in a URL reaches process listings")
	}
	if strings.Contains(err.Error(), rpcPasswordWithAColon) {
		t.Fatalf("the refusal quoted the credential: %v", err)
	}
}

func constructFor(t *testing.T, e providers.Entry) providers.Provider {
	t.Helper()
	resolved, err := providers.Validate([]providers.Entry{e})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	provider, handled, err := Constructor(resolved[0], func() time.Time { return fixedNow })
	if err != nil {
		t.Fatalf("Constructor: %v", err)
	}
	if !handled {
		t.Fatal("Constructor did not handle a transmission provider")
	}
	return provider
}
