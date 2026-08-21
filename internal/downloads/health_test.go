package downloads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Health, version compatibility, and the paths the corpus does not cover.
//
// # These use a hand-built server, and say so
//
// Everything here needs a response the captured instance does not produce: a
// 401 needs rpc-authentication enabled, an unsupported version needs an old
// Transmission, an unreachable endpoint needs one that is down. None of those
// can be captured from a healthy instance without changing it, and
// Transmission rewrites settings.json on clean shutdown — so enabling
// authentication to get one 401 is not a change to make in passing.
//
// So these are TEST DOUBLES, and they are named as such rather than dressed up
// as captures. ADR-0026's objection is to invented fixtures passing as
// recorded ones; a server written in a test file, in the test that uses it,
// misleads nobody.

// stubSession answers session-get with a given rpc-version, after the
// handshake. The handshake itself is the real behaviour, so even the doubles
// exercise it.
func stubSession(t *testing.T, rpcVersion int) *httptest.Server {
	t.Helper()
	const id = "test-session-id"
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Transmission-Session-Id") != id {
			w.Header().Set("X-Transmission-Session-Id", id)
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"success","arguments":{
			"version":"stub","rpc-version":` + itoa(rpcVersion) + `,
			"rpc-version-minimum":14,"download-dir":"/downloads",
			"incomplete-dir":"/incomplete","incomplete-dir-enabled":false}}`))
	}))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// An unsupported version is REPORTED, naming both numbers — never a startup
// failure. ADR-0025: a download client that is too old must not stop Heyarr
// serving the library.
func TestAnUnsupportedRPCVersionIsUnhealthyAndNamesBothNumbers(t *testing.T) {
	srv := stubSession(t, 9)
	defer srv.Close()

	client := clientFor(t, srv.URL)
	h := client.Check(context.Background())

	if h.Healthy {
		t.Fatal("an RPC version below the minimum must not report healthy")
	}
	for _, want := range []string{"9", itoa(minimumRPCVersion)} {
		if !strings.Contains(h.Detail, want) {
			t.Errorf("the detail should name %q so an operator knows what to upgrade to; "+
				"got %q", want, h.Detail)
		}
	}
}

// Below RPC 16 there are no labels, so the subdirectory fallback applies — and
// the degradation is REPORTED rather than silent. An operator wondering why
// their transfers land in a subdirectory should find the answer in health.
func TestAnInstanceWithoutLabelsIsHealthyAndSaysSo(t *testing.T) {
	srv := stubSession(t, 15)
	defer srv.Close()

	client := clientFor(t, srv.URL)
	h := client.Check(context.Background())

	if !h.Healthy {
		t.Fatalf("no labels is a degradation, not a failure: %s", h.Detail)
	}
	if client.Session().SupportsLabels() {
		t.Error("RPC 15 has no labels")
	}
	for _, want := range []string{"label", "subdirectory"} {
		if !strings.Contains(strings.ToLower(h.Detail), want) {
			t.Errorf("the detail should explain the fallback, mentioning %q; got %q",
				want, h.Detail)
		}
	}
}

// The captured instance is above the threshold, so labels are the primary path.
func TestTheCapturedVersionUsesLabels(t *testing.T) {
	srv := stubSession(t, 19)
	defer srv.Close()

	client := clientFor(t, srv.URL)
	if h := client.Check(context.Background()); !h.Healthy {
		t.Fatal(h.Detail)
	}
	if !client.Session().SupportsLabels() {
		t.Error("RPC 19 has labels and they are the primary path")
	}
}

// ## UNFIXTURED — this is a test double, not a capture.
//
// The corpus contains no 401: obtaining one means enabling rpc-authentication
// on a real instance, and Transmission rewrites settings.json on clean
// shutdown. A synthesised 401 in the corpus would be a fixture testing that
// this client agrees with whoever invented it (ADR-0026), so the path is
// covered here instead, where nothing pretends it was recorded.
func TestARefusedCredentialIsAConfigurationProblemNotAnOutage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := clientFor(t, srv.URL)
	h := client.Check(context.Background())

	if h.Healthy {
		t.Fatal("a refused credential is not healthy")
	}
	// It must say CREDENTIAL, not "unreachable". Retrying a wrong password
	// forever produces an unhealthy provider whose detail sends an operator to
	// look at the network.
	if !strings.Contains(strings.ToLower(h.Detail), "credential") {
		t.Errorf("the detail should name the credential rather than the network; got %q",
			h.Detail)
	}
	if strings.Contains(strings.ToLower(h.Detail), "unreachable") {
		t.Errorf("a 401 is not unreachability: %q", h.Detail)
	}
}

// Configured-but-unreachable is a health report, never a startup failure.
func TestAnUnreachableInstanceIsReportedNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	client := clientFor(t, url)
	h := client.Check(context.Background())

	if h.Healthy {
		t.Fatal("nothing is listening")
	}
	if h.CheckedAt.IsZero() {
		t.Error("an unhealthy report still records when it was observed")
	}
	if strings.TrimSpace(h.Detail) == "" {
		t.Error("a status with no reason is one nobody can act on")
	}
}

// A 409 that never resolves must terminate rather than spin. The likely cause
// is a proxy stripping the header, and retrying will not fix a proxy.
func TestAPermanent409TerminatesRatherThanSpinning(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("X-Transmission-Session-Id", "rotating-every-time-"+itoa(attempts))
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()

	client := clientFor(t, srv.URL)
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.Check(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a permanent 409 spun instead of giving up")
	}
	if attempts > maxHandshakeRetries+1 {
		t.Errorf("%d attempts for a permanent 409; the bound is %d",
			attempts, maxHandshakeRetries+1)
	}
}

// A reverse proxy in front of the service answers HTML, and the error must say
// what arrived rather than "invalid character '<'".
func TestANonJSONResponseSaysWhatArrived(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Transmission-Session-Id") == "" {
			w.Header().Set("X-Transmission-Session-Id", "id")
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><head><title>502 Bad Gateway</title></head><body>…</body></html>"))
	}))
	defer srv.Close()

	client := clientFor(t, srv.URL)
	h := client.Check(context.Background())
	if h.Healthy {
		t.Fatal("an HTML error page is not a healthy Transmission")
	}
	if !strings.Contains(h.Detail, "502") && !strings.Contains(h.Detail, "html") {
		t.Errorf("the detail should quote what arrived so a proxy is recognisable; got %q",
			h.Detail)
	}
}

// A credential must never reach a health detail, which is served by the API.
func TestNoCredentialReachesAHealthDetail(t *testing.T) {
	const password = "hunter2-do-not-leak"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	client, err := New(Options{
		Name: "test", Endpoint: srv.URL,
		Username: "someone", Password: password,
		Now: func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	h := client.Check(context.Background())
	if strings.Contains(h.Detail, password) {
		t.Fatalf("the credential reached a health detail served by the API: %q", h.Detail)
	}
}
