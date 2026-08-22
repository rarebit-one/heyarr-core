package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// The peer transport against the REAL trust root.
//
// The tests in internal/api/peerapi drive the handshake against a membership
// double, which is right for asserting what the transport does. This one
// asserts what the adapter does — that the question the transport asks reaches
// membership.Store, and that the store's answer is fresh enough for ADR-0012's
// "revocation is removing a membership record" to be literally true on a
// connection that is already open.
//
// It is here rather than in either package because peerLookup is here: the
// controller is the only place that holds both the transport and the store,
// and an adapter tested nowhere is where a cache gets added.

func realFabric(t *testing.T) (*membership.Store, *sqlite.DB) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlite.Open(ctx, sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	store, err := membership.New(membership.Options{
		DB: db, Events: eventLog, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, db
}

func TestRemovingAMembershipRecordSeversALivePeerConnection(t *testing.T) {
	ctx := context.Background()
	store, _ := realFabric(t)

	selfPub, selfPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	peerPub, peerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	self, err := store.Register(ctx, membership.Registration{
		Name: "peer-a", Site: "site-a", PublicKey: selfPub, IsSelf: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := store.Register(ctx, membership.Registration{
		Name: "peer-b", Site: "site-b", Endpoint: "https://b.example:8385", PublicKey: peerPub,
	})
	if err != nil {
		t.Fatal(err)
	}

	lookup := peerLookup{store: store}
	selfMaterial, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: selfPriv, PeerID: self.Member.PeerID})
	if err != nil {
		t.Fatal(err)
	}
	peerMaterial, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: peerPriv, PeerID: enrolled.Member.PeerID})
	if err != nil {
		t.Fatal(err)
	}

	srv, err := peerapi.New(peerapi.Options{
		Addr: "127.0.0.1:0", Material: selfMaterial, Members: lookup,
		SelfPeerID: self.Member.PeerID, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})

	client, err := mtls.Client(mtls.Options{Material: peerMaterial, Members: lookup})
	if err != nil {
		t.Fatal(err)
	}
	url := "https://" + srv.Addr() + peerapi.Prefix + "/identity"

	get := func() (int, string, bool) {
		t.Helper()
		var reused bool
		reqCtx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		})
		req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		resp, doErr := client.Do(req)
		if doErr != nil {
			t.Fatalf("GET %s: %v", url, doErr)
		}
		defer func() { _ = resp.Body.Close() }()
		body, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return resp.StatusCode, string(body), reused
	}

	// Reproduce the working case first. A test that only asserted the
	// after-state would pass unchanged on a node where peer requests never
	// worked at all.
	status, body, _ := get()
	if status != http.StatusOK {
		t.Fatalf("before removal: %d, want 200\n%s", status, body)
	}
	if !strings.Contains(body, enrolled.Member.PeerID) {
		t.Fatalf("the server did not derive the enrolled peer's id from its certificate:\n%s", body)
	}

	status, body, reused := get()
	if status != http.StatusOK {
		t.Fatalf("the second request before removal failed: %d\n%s", status, body)
	}
	if !reused {
		t.Fatal("the transport did not reuse its connection, so the revocation below would be " +
			"about a NEW connection and would say much less than it appears to")
	}

	if _, err := store.Remove(ctx, enrolled.Member.PeerID); err != nil {
		t.Fatal(err)
	}

	status, body, reused = get()
	if status != http.StatusForbidden {
		t.Fatalf("after removal: %d, want 403 — a peer whose membership record is gone is still "+
			"being served (ADR-0012)\n%s", status, body)
	}
	if !reused {
		t.Error("the refused request went over a NEW connection, so this run does not show that an " +
			"already-open connection was severed")
	}
	if !strings.Contains(body, "not a member") {
		t.Errorf("the refusal does not say why: %s", body)
	}
}

// TestPeerLookupTranslatesTheTrustRootsRefusal: the transport branches on its
// own error, and a refusal that arrived as an unrecognised database error would
// be reported as an unavailable trust root — which fails closed but tells the
// operator the wrong thing.
func TestPeerLookupTranslatesTheTrustRootsRefusal(t *testing.T) {
	store, _ := realFabric(t)
	stranger, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, err = peerLookup{store: store}.Lookup(context.Background(), stranger)
	if !errors.Is(err, mtls.ErrNotAMember) {
		t.Fatalf("an unenrolled key was refused with %v, want mtls.ErrNotAMember", err)
	}
	if !errors.Is(err, membership.ErrNotAMember) {
		t.Errorf("the trust root's own error was lost in translation: %v", err)
	}
}
