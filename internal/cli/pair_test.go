package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rarebit-one/voidbind-go/device"
	"github.com/rarebit-one/voidbind-go/enrolment"

	"github.com/rarebit-one/heyarr-core/internal/api/relay"
)

// lockedBuffer is an output buffer one goroutine writes while another polls it —
// authorise prints its invite and then blocks in the handshake, and the test
// reads the invite from its output to start the new device.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// runIsolated runs one command with its own output buffers, so two can run
// concurrently against a shared relay without racing on a shared buffer.
func runIsolated(ctx context.Context, out *lockedBuffer, args ...string) error {
	var errb bytes.Buffer
	cmd := NewRootCommand(Options{Stdout: out, Stderr: &errb, ShutdownGrace: 2 * time.Second})
	cmd.SetArgs(args)
	return cmd.ExecuteContext(ctx)
}

// relayServer stands up the node's Voidbind relay mount (the one the controller
// serves under /pair/v1) on an httptest server, and returns the node address.
func relayServer(t *testing.T) string {
	t.Helper()
	r := chi.NewRouter()
	relay.New(relay.Options{}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv.URL
}

// pairRun is one authorise ↔ enrol exchange, run concurrently the way the two
// devices run it.
type pairRun struct {
	authOut, enrolOut *lockedBuffer
	authErr, enrolErr error
}

// runPair starts `pair authorise` with authArgs, waits for the invite it prints,
// then runs `pair enrol --invite <invite>` with enrolArgs beside it.
func runPair(t *testing.T, ctx context.Context, authArgs, enrolArgs []string) pairRun {
	t.Helper()
	res := pairRun{authOut: &lockedBuffer{}, enrolOut: &lockedBuffer{}}
	authDone := make(chan struct{})
	go func() {
		defer close(authDone)
		res.authErr = runIsolated(ctx, res.authOut, append([]string{"pair", "authorise"}, authArgs...)...)
	}()
	invite := ""
	for invite == "" {
		select {
		case <-authDone:
			t.Fatalf("pair authorise ended before printing an invite: %v\n%s", res.authErr, res.authOut)
		case <-ctx.Done():
			t.Fatalf("no invite from pair authorise:\n%s", res.authOut)
		case <-time.After(5 * time.Millisecond):
		}
		invite = extractLine(res.authOut.String(), "invite:")
	}
	res.enrolErr = runIsolated(ctx, res.enrolOut,
		append([]string{"pair", "enrol", "--invite", invite}, enrolArgs...)...)
	<-authDone
	return res
}

// TestPairAdmitsNewDevicesThroughTheCLI drives the whole pairing story through
// the real command tree over the node's real relay mount, with both kinds of
// initiator: the user identity (genesis, signing through the identity store's
// signer) admits a first device, and that device — now a member — admits a
// second one. Each new device ends up holding a membership op under the same
// user, and the member's replica records the add it signed (ADR-0068).
func TestPairAdmitsNewDevicesThroughTheCLI(t *testing.T) {
	relayAddr := relayServer(t)
	idDir := identityDir(t)
	firstDir := deviceDir(t)
	secondDir := deviceDir(t)
	emptyDevDir := deviceDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, _, err := run(t, ctx, "identity", "generate", "--identity-dir", idDir, "--name", "owner"); err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	if _, _, err := run(t, ctx, "device", "generate", "--device-dir", firstDir, "--name", "new-laptop"); err != nil {
		t.Fatalf("device generate: %v", err)
	}
	idPub := showIdentityJSON(t, ctx, idDir).PublicKey

	// 1. GENESIS: the identity admits the first device. The initiator's device
	// dir is an empty one, so it holds no replica to cite.
	genesis := runPair(t, ctx,
		[]string{"--identity-dir", idDir, "--device-dir", emptyDevDir, "--relay", relayAddr, "--yes", "--poll", "10ms"},
		[]string{"--device-dir", firstDir, "--yes", "--poll", "10ms"})
	assertPaired(t, genesis)
	if !strings.Contains(genesis.authOut.String(), "authorising as: user identity "+idPub) {
		t.Fatalf("auto did not pick the identity where one is present:\n%s", genesis.authOut)
	}
	first := showDeviceJSON(t, ctx, firstDir)
	if first.EnrolmentStatus != device.EnrolmentEnrolled || first.EnrolledUser != idPub {
		t.Fatalf("the first device is %q under %q, want enrolled under %q",
			first.EnrolmentStatus, first.EnrolledUser, idPub)
	}
	if !strings.Contains(genesis.authOut.String(), first.PublicKey) {
		t.Fatalf("authorise did not name the device it admitted (%s):\n%s", first.PublicKey, genesis.authOut)
	}

	// 2. MEMBER DEVICE: the first device admits a second. No identity here, so
	// auto falls back to the device; the second device's dir is fresh, so enrol
	// generates its keys.
	member := runPair(t, ctx,
		[]string{"--identity-dir", identityDir(t), "--device-dir", firstDir, "--relay", relayAddr, "--yes", "--poll", "10ms"},
		[]string{"--device-dir", secondDir, "--yes", "--poll", "10ms"})
	assertPaired(t, member)
	if !strings.Contains(member.authOut.String(), "authorising as: member device "+first.PublicKey) {
		t.Fatalf("auto did not fall back to the member device:\n%s", member.authOut)
	}
	second := showDeviceJSON(t, ctx, secondDir)
	if second.EnrolmentStatus != device.EnrolmentEnrolled || second.EnrolledUser != idPub {
		t.Fatalf("the second device is %q under %q, want enrolled under %q",
			second.EnrolmentStatus, second.EnrolledUser, idPub)
	}

	// The second device's admission is an add signed BY the first device, and
	// the first device's replica recorded it.
	secondStore, err := openDeviceStore(secondDir)
	if err != nil {
		t.Fatal(err)
	}
	secondDev, err := secondStore.Get("")
	if err != nil {
		t.Fatal(err)
	}
	tok, ok := secondDev.EnrolmentCert()
	if !ok {
		t.Fatal("the second device holds no admitting op")
	}
	op, err := enrolment.VerifyOp(tok)
	if err != nil {
		t.Fatalf("the second device's admission is not a membership op: %v", err)
	}
	if op.Kind != enrolment.OpAdd || op.By != first.PublicKey || op.Device != second.PublicKey {
		t.Fatalf("admission is %s of %s by %s, want add of %s by %s",
			op.Kind, op.Device, op.By, second.PublicKey, first.PublicKey)
	}
	firstStore, err := openDeviceStore(firstDir)
	if err != nil {
		t.Fatal(err)
	}
	view, err := firstStore.Membership(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !view.IsMember(second.PublicKey) {
		t.Fatalf("the admitting device's replica does not record the add it signed")
	}
}

// TestPairRefusalOnMismatchedCodeEnrolsNobody: when the authorising side is told
// the codes do NOT match (a wrong --confirm-sas), it signs nothing and posts a
// signed refusal (ADR-0012), and the new device hears it — it fails with
// errPairRefusedByPeer, not a timeout — and stores nothing.
func TestPairRefusalOnMismatchedCodeEnrolsNobody(t *testing.T) {
	relayAddr := relayServer(t)
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

	res := runPair(t, ctx,
		[]string{
			"--as", "identity", "--identity-dir", idDir, "--device-dir", deviceDir(t),
			"--relay", relayAddr, "--confirm-sas", "0000000", "--poll", "10ms",
		},
		[]string{"--device-dir", devDir, "--yes", "--poll", "10ms", "--timeout", "2s"})

	if res.authErr == nil || !strings.Contains(res.authErr.Error(), "pairing refused") {
		t.Fatalf("authorise did not refuse a mismatched code: %v\n%s", res.authErr, res.authOut)
	}
	if res.enrolErr == nil {
		t.Fatalf("enrol completed despite the initiator refusing:\n%s", res.enrolOut)
	}
	// The refusal, not the 2s --timeout running out: a timeout is a context
	// deadline, never errPairRefusedByPeer.
	if !errors.Is(res.enrolErr, errPairRefusedByPeer) {
		t.Fatalf("enrol did not hear the refusal: %v\n%s", res.enrolErr, res.enrolOut)
	}
	after := showDeviceJSON(t, ctx, devDir)
	if after.EnrolmentStatus != device.EnrolmentNotEnrolled {
		t.Fatalf("a device was enrolled despite a refused pairing: %q", after.EnrolmentStatus)
	}
}

// TestPairRefusalOverARelayWithoutTheSlot: a relay that predates ADR-0012 has
// no `refuse` slot and answers the refusal 400. That is advisory — authorise
// still reports the mismatch as the refusal, not as a relay failure — and the
// new device falls back to waiting out its --timeout, enrolling nothing.
func TestPairRefusalOverARelayWithoutTheSlot(t *testing.T) {
	r := chi.NewRouter()
	relay.New(relay.Options{Types: []string{"commit", "reveal", "cert"}}).Mount(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	idDir := identityDir(t)
	devDir := deviceDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, _, err := run(t, ctx, "identity", "generate", "--identity-dir", idDir, "--name", "owner"); err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	res := runPair(t, ctx,
		[]string{
			"--as", "identity", "--identity-dir", idDir, "--device-dir", deviceDir(t),
			"--relay", srv.URL, "--confirm-sas", "0000000", "--poll", "10ms",
		},
		[]string{"--device-dir", devDir, "--yes", "--poll", "10ms", "--timeout", "1s"})

	if !errors.Is(res.authErr, errSASRefused) {
		t.Fatalf("authorise: %v, want the SAS refusal even though the relay rejected the refuse slot\n%s",
			res.authErr, res.authOut)
	}
	if res.enrolErr == nil || errors.Is(res.enrolErr, errPairRefusedByPeer) {
		t.Fatalf("enrol: %v, want a timeout (the relay could not carry the refusal)\n%s", res.enrolErr, res.enrolOut)
	}
	if after := showDeviceJSON(t, ctx, devDir); after.EnrolmentStatus != device.EnrolmentNotEnrolled {
		t.Fatalf("a device was enrolled despite a refused pairing: %q", after.EnrolmentStatus)
	}
}

// TestPairAuthoriseAsDeviceRefusesANonMember: a device that is not a member of
// any identity has nothing to vouch with, so it cannot open a pairing.
func TestPairAuthoriseAsDeviceRefusesANonMember(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	devDir := deviceDir(t)
	if _, _, err := run(t, ctx, "device", "generate", "--device-dir", devDir, "--name", "loner"); err != nil {
		t.Fatalf("device generate: %v", err)
	}
	_, _, err := run(t, ctx, "pair", "authorise", "--as", "device", "--device-dir", devDir,
		"--identity-dir", identityDir(t), "--relay", "127.0.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "not a member") {
		t.Fatalf("a non-member device was allowed to authorise: %v", err)
	}
}

func TestRelayAddressForms(t *testing.T) {
	cases := []struct {
		addr, base, invite string
	}{
		{"127.0.0.1:8080", "http://127.0.0.1:8080/pair", "http://127.0.0.1:8080/pair"},
		{"http://node.invalid:8080/", "http://node.invalid:8080/pair", "http://node.invalid:8080/pair"},
		{"https://node.invalid/pair", "https://node.invalid/pair", "https://node.invalid/pair"},
		{"/run/heyarr.sock", unixRelayHost + "/pair", "unix:///run/heyarr.sock"},
		{"unix:///run/heyarr.sock", unixRelayHost + "/pair", "unix:///run/heyarr.sock"},
	}
	for _, c := range cases {
		ep, err := relayForNode(c.addr)
		if err != nil {
			t.Fatalf("%s: %v", c.addr, err)
		}
		if ep.base != c.base || ep.invite != c.invite {
			t.Errorf("%s: base %q invite %q, want %q %q", c.addr, ep.base, ep.invite, c.base, c.invite)
		}
		// What the invite carries resolves back to the same relay.
		back, err := relayFromInvite(ep.invite)
		if err != nil {
			t.Fatalf("%s: invite %q: %v", c.addr, ep.invite, err)
		}
		if back.base != ep.base {
			t.Errorf("%s: the invite resolves to base %q, want %q", c.addr, back.base, ep.base)
		}
	}
	if _, err := relayFromInvite("ftp://node.invalid"); err == nil {
		t.Error("an invite relay that is neither http(s) nor unix:// was accepted")
	}
}

// assertPaired fails unless both sides completed and derived the same code.
func assertPaired(t *testing.T, res pairRun) {
	t.Helper()
	if res.authErr != nil {
		t.Fatalf("pair authorise: %v\n%s", res.authErr, res.authOut)
	}
	if res.enrolErr != nil {
		t.Fatalf("pair enrol: %v\n%s", res.enrolErr, res.enrolOut)
	}
	authSAS := extractLine(res.authOut.String(), "short authentication code:")
	enrolSAS := extractLine(res.enrolOut.String(), "short authentication code:")
	if authSAS == "" || authSAS != enrolSAS {
		t.Fatalf("the two sides derived different codes: authorise %q, enrol %q", authSAS, enrolSAS)
	}
}

// extractLine returns the rest of the first output line with prefix, or "".
func extractLine(out, prefix string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}
