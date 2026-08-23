package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/reachability"
)

// Enrolment refuses a one-way pairing (#186, ADR-0037).
//
// # What is being reproduced
//
// #186 was found on real hardware: one host reached the other, the other could
// not reach it at all, and the failure surfaced weeks later as a reconciliation
// that quietly emitted no work — correctly, because nothing had told it the far
// node held the bytes. TestOneWayPairingReconcilesToSilence in internal/worker
// reproduces that silence and stays green: the reconciler is not what is wrong.
// What is wrong is WHEN an operator finds out, and these tests move it to the
// one moment where the pairing is being created and a human is looking.
//
// Each case stands up a REAL peer surface over real mTLS with real pinning,
// and the only thing that varies between them is what that node reports about
// the return path — which is exactly the variable the network controls.

// oneWayTrustRoot pins one key: this node's. The peer surface below is the
// far site, and this is that site's membership table.
type oneWayTrustRoot struct {
	peer mtls.Peer
}

func (r oneWayTrustRoot) Lookup(_ context.Context, publicKey []byte) (mtls.Peer, error) {
	if !ed25519.PublicKey(publicKey).Equal(r.peer.PublicKey) {
		return mtls.Peer{}, mtls.ErrNotAMember
	}
	return r.peer, nil
}

// fixedReturnPath is the far site's answer about the return leg.
type fixedReturnPath struct {
	result reachability.Result
	target string
}

func (f fixedReturnPath) ProbeReturnPath(
	_ context.Context, _ string,
) (reachability.Result, string, error) {
	return f.result, f.target, nil
}

// farSite starts a peer surface that pins this node's identity and reports the
// given return-path result. It answers on 127.0.0.1, so the OUTBOUND leg is
// genuinely reachable in every case here and the return leg is the only
// variable.
func farSite(t *testing.T, returnPath peerapi.ReturnPathProber) (endpoint, publicKey string) {
	t.Helper()
	// This node's key, as plantSelfIdentity puts it on disk.
	selfPub, _ := fixedKeypair(t, 0x11)
	farPub, farPriv := fixedKeypair(t, 0x77)

	material, err := mtls.NewMaterial(mtls.MaterialOptions{
		PrivateKey: farPriv, PeerID: "01990000-0000-7000-8000-0000000farsi",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := peerapi.New(peerapi.Options{
		Addr:     "127.0.0.1:0",
		Material: material,
		Members: oneWayTrustRoot{peer: mtls.Peer{
			PeerID: "01990000-0000-7000-8000-000000000self",
			Name:   "peer-a", PublicKey: selfPub,
		}},
		SelfPeerID: "01990000-0000-7000-8000-0000000farsi",
		ReturnPath: returnPath,
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return "https://" + srv.Addr(), identity.FormatPublicKey(farPub)
}

// TestEnrolmentRefusesAOneWayPairing is the acceptance condition of #186: a
// deliberate one-way pairing is refused, at ENROLMENT, naming the direction
// that failed.
func TestEnrolmentRefusesAOneWayPairing(t *testing.T) {
	h := newAPIHarness(t).seed()
	plantSelfIdentity(t, h)
	endpoint, key := farSite(t, fixedReturnPath{
		result: reachability.ResultUnreachable,
		target: "https://peer-a.invalid:8385",
	})

	_, stderr, err := h.run("peers", "add", "--name", "peer-b", "--site", "site-b",
		"--endpoint", endpoint, "--public-key", key)
	if err == nil {
		t.Fatalf("the one-way pairing was enrolled:\n%s", stderr)
	}
	// The direction, by name. This is the sentence an operator acts on.
	for _, want := range []string{
		string(reachability.DirectionReturn),
		"peer-b", endpoint, "https://peer-a.invalid:8385",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}

	// Refused at ADD time: nothing was enrolled. Same assertion #169's
	// endpoint tests make, and for the same reason — a peer that exists here
	// is one `peers list` would show as healthy right up until a transfer.
	if _, _, err := h.run("peers", "show", "peer-b"); err == nil {
		t.Error("the peer was enrolled anyway, so the pairing was refused later rather than at enrolment")
	}
}

// TestEnrolmentAcceptsABidirectionalPairing is the guard against the fix being
// "always refuse". The far site reports the return path works; enrolment
// proceeds exactly as it did before this check existed.
func TestEnrolmentAcceptsABidirectionalPairing(t *testing.T) {
	h := newAPIHarness(t).seed()
	plantSelfIdentity(t, h)
	endpoint, key := farSite(t, fixedReturnPath{
		result: reachability.ResultReachable,
		target: "https://peer-a.invalid:8385",
	})

	out := h.mustRun("peers", "add", "--name", "peer-b", "--site", "site-b",
		"--endpoint", endpoint, "--public-key", key)
	if !strings.Contains(out, "peer-b") {
		t.Fatalf("a bidirectional pairing was not enrolled:\n%s", out)
	}
	if _, _, err := h.run("peers", "show", "peer-b"); err != nil {
		t.Errorf("the peer is not enrolled after a bidirectional check: %v", err)
	}
}

// TestEnrolmentAcceptsAnUnprovenPairing: a far site that cannot probe a return
// path — no membership table behind its peer surface, an older build, a node
// that simply does not know — answers `unknown`, and unknown is not evidence
// of a fault. A peer that is powered off looks exactly the same, and refusing
// those would be the check doing more damage than the fault it is for.
func TestEnrolmentAcceptsAnUnprovenPairing(t *testing.T) {
	h := newAPIHarness(t).seed()
	plantSelfIdentity(t, h)
	endpoint, key := farSite(t, nil)

	out := h.mustRun("peers", "add", "--name", "peer-b", "--site", "site-b",
		"--endpoint", endpoint, "--public-key", key)
	if !strings.Contains(out, "peer-b") {
		t.Fatalf("an unproven pairing was refused:\n%s", out)
	}
}

// TestEnrolmentCanBeForcedPastTheCheck: the check is a probe, and a probe can
// be wrong — a return path that opens later, a firewall changing this
// afternoon. The escape hatch exists so that an operator who knows better is
// not stuck, and it is named in the refusal itself.
func TestEnrolmentCanBeForcedPastTheCheck(t *testing.T) {
	h := newAPIHarness(t).seed()
	plantSelfIdentity(t, h)
	endpoint, key := farSite(t, fixedReturnPath{result: reachability.ResultUnreachable})

	h.mustRun("peers", "add", "--name", "peer-b", "--site", "site-b",
		"--endpoint", endpoint, "--public-key", key, "--skip-reachability-check")
	if _, _, err := h.run("peers", "show", "peer-b"); err != nil {
		t.Errorf("--skip-reachability-check did not enrol the peer: %v", err)
	}
}

// TestEnrolmentDoesNotCheckThisNodesOwnRecord: giving this node the address
// other peers reach it at is not a pairing. There is no return path to prove —
// the row a probe would read is the row being written — and reporting it as
// unverified every time would train an operator to ignore the note that
// matters. The endpoint here refuses connections (port 9 is discard), so a
// pairing check would certainly have said something.
func TestEnrolmentDoesNotCheckThisNodesOwnRecord(t *testing.T) {
	h := newAPIHarness(t).seed()
	plantSelfIdentity(t, h)
	selfPub, _ := fixedKeypair(t, 0x11)

	var self client.Peer
	for _, p := range decodePeers(t, h.mustRun("peers", "list", "--json")) {
		if p.IsSelf {
			self = p
		}
	}
	if self.Name == "" {
		t.Fatal("the harness has no self peer")
	}

	_, stderr, err := h.run("peers", "add", "--name", self.Name,
		"--public-key", identity.FormatPublicKey(selfPub), "--endpoint", "https://127.0.0.1:9")
	if err != nil {
		t.Fatalf("registering this node's own endpoint failed: %v\n%s", err, stderr)
	}
	if strings.Contains(stderr, "not verified") {
		t.Errorf("this node's own record was checked as a pairing:\n%s", stderr)
	}
}

// decodePeers reads `peers list --json`.
func decodePeers(t *testing.T, out string) []client.Peer {
	t.Helper()
	var peers []client.Peer
	if err := json.Unmarshal([]byte(out), &peers); err != nil {
		t.Fatalf("`peers list --json` is not a peer list: %v\n%s", err, out)
	}
	return peers
}
