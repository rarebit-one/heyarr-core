package cli

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/pairrelay"
)

// runIsolated runs one command with its own output buffers, so two can run
// concurrently against a shared relay without racing on a shared buffer.
func runIsolated(ctx context.Context, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := NewRootCommand(Options{Stdout: &out, Stderr: &errb, ShutdownGrace: 2 * time.Second})
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

// relayServer stands up the real pairrelay handler on an httptest server.
func relayServer(t *testing.T) string {
	t.Helper()
	r := chi.NewRouter()
	pairrelay.NewHandler(pairrelay.HandlerOptions{}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestPairEnrolsANewDeviceThroughTheCLI drives the whole pairing story through
// the real command tree over the real relay: an old device (holding a user
// identity) authorises a new device (with only its own key), and the new device
// ends up holding a cert that enrols it under the same user. This is #305's
// premise made reachable from the binary, not just from a Go test of the flow.
func TestPairEnrolsANewDeviceThroughTheCLI(t *testing.T) {
	relay := relayServer(t)
	idDir := identityDir(t)
	devDir := deviceDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, _, err := run(t, ctx, "identity", "generate", "--identity-dir", idDir, "--name", "owner"); err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	if _, _, err := run(t, ctx, "device", "generate", "--device-dir", devDir, "--name", "new-phone"); err != nil {
		t.Fatalf("device generate: %v", err)
	}
	idPub := showIdentityJSON(t, ctx, idDir).PublicKey

	session := "test-session-1"
	var authOut, enrolOut string
	var authErr, enrolErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		authOut, authErr = runIsolated(ctx, "pair", "authorise",
			"--identity-dir", idDir, "--relay", relay, "--session", session, "--yes",
			"--poll", "10ms")
	}()
	go func() {
		defer wg.Done()
		enrolOut, enrolErr = runIsolated(ctx, "pair", "enrol",
			"--device-dir", devDir, "--relay", relay, "--session", session, "--yes",
			"--poll", "10ms")
	}()
	wg.Wait()

	if authErr != nil {
		t.Fatalf("pair authorise: %v\n%s", authErr, authOut)
	}
	if enrolErr != nil {
		t.Fatalf("pair enrol: %v\n%s", enrolErr, enrolOut)
	}

	// Both sides derived the SAME short code — the human comparison, asserted.
	authSAS := extractSAS(t, authOut)
	enrolSAS := extractSAS(t, enrolOut)
	if authSAS != enrolSAS {
		t.Fatalf("the two sides derived different codes: authorise %q, enrol %q", authSAS, enrolSAS)
	}

	// The new device is now enrolled under the user identity.
	after := showDeviceJSON(t, ctx, devDir)
	if after.EnrolmentStatus != device.EnrolmentEnrolled {
		t.Fatalf("the paired device is %q, want enrolled", after.EnrolmentStatus)
	}
	if after.EnrolledUser != idPub {
		t.Fatalf("the paired device enrolled under %q, want the user %q", after.EnrolledUser, idPub)
	}
}

// TestPairRefusalOnMismatchedCodeEnrolsNobody: when the initiator is told the
// codes do NOT match (a wrong --confirm-sas), it refuses to sign, and the
// responder — seeing the abort — enrols nothing. The refusal is the deliverable
// as much as the success (#305).
func TestPairRefusalOnMismatchedCodeEnrolsNobody(t *testing.T) {
	relay := relayServer(t)
	idDir := identityDir(t)
	devDir := deviceDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, _, err := run(t, ctx, "identity", "generate", "--identity-dir", idDir, "--name", "owner"); err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	if _, _, err := run(t, ctx, "device", "generate", "--device-dir", devDir, "--name", "new-phone"); err != nil {
		t.Fatalf("device generate: %v", err)
	}

	session := "test-session-refuse"
	var authErr, enrolErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// A code that will never match the derived one.
		_, authErr = runIsolated(ctx, "pair", "authorise",
			"--identity-dir", idDir, "--relay", relay, "--session", session,
			"--confirm-sas", "0000000", "--poll", "10ms")
	}()
	go func() {
		defer wg.Done()
		_, enrolErr = runIsolated(ctx, "pair", "enrol",
			"--device-dir", devDir, "--relay", relay, "--session", session, "--yes",
			"--poll", "10ms")
	}()
	wg.Wait()

	if authErr == nil {
		t.Fatal("authorise did not refuse a mismatched code")
	}
	if enrolErr == nil {
		t.Fatal("enrol completed despite the initiator refusing")
	}
	after := showDeviceJSON(t, ctx, devDir)
	if after.EnrolmentStatus != device.EnrolmentNotEnrolled {
		t.Fatalf("a device was enrolled despite a refused pairing: %q", after.EnrolmentStatus)
	}
}

// extractSAS pulls the short code out of a pair command's output.
func extractSAS(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "short authentication code:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "short authentication code:"))
		}
	}
	t.Fatalf("no short authentication code in output:\n%s", out)
	return ""
}
