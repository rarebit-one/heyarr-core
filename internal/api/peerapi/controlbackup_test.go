package peerapi_test

import (
	"crypto/ed25519"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/backupsync"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// serveWithControlBackup is serve() with a control-backup store behind the
// route, so a real push can be verified end to end over mTLS.
func serveWithControlBackup(t *testing.T, self *peerNode, members mtls.Membership,
	sink peerapi.ControlBackupSink,
) *listener {
	t.Helper()
	logs := &syncBuffer{}
	srv, err := peerapi.New(peerapi.Options{
		Addr:          "127.0.0.1:0",
		Material:      self.material,
		Members:       members,
		SelfPeerID:    self.peerID,
		ControlBackup: sink,
		Logger:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	return &listener{srv: srv, self: self, addr: srv.Addr(), logs: logs}
}

// signedBackupBy produces a signed backup of a fresh control plane on disk,
// attributed to sourceID and signed with priv, and returns its directory and
// manifest.
func signedBackupBy(t *testing.T, sourceID string, priv ed25519.PrivateKey) (string, backup.Manifest) {
	t.Helper()
	dir := t.TempDir()
	db, err := sqlite.Open(t.Context(), sqlite.Options{Path: filepath.Join(dir, "heyarr.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlite.Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Emit(t.Context(), "test.seed", "s", "x", nil); err != nil {
		t.Fatal(err)
	}
	art, err := backup.Take(t.Context(), backup.TakeOptions{
		DB: db, Events: log, SourcePeerID: sourceID, Signer: priv,
		Dir: filepath.Join(dir, "backups"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return art.Dir, art.Manifest
}

// TestControlBackupPushCrossesToAPeer is the M7-03 acceptance: a backup crosses
// the peer surface to a second node and is byte-identical on arrival, asserted
// by digest at both ends.
func TestControlBackupPushCrossesToAPeer(t *testing.T) {
	sender := newPeerNode(t, "peer-a", "site-a")
	receiver := newPeerNode(t, "peer-b", "site-b")
	root := newTrustRoot(sender.member(), receiver.member())

	backupDir, manifest := signedBackupBy(t, sender.peerID, sender.priv)
	store := backupsync.NewStore(t.TempDir(), 0)
	l := serveWithControlBackup(t, receiver, root, store)

	pusher := backupsync.NewPusher(sender.material, slog.New(slog.DiscardHandler))
	target := backupsync.Target{Peer: receiver.member(), Endpoint: "https://" + l.addr}

	gen, err := pusher.PushTo(t.Context(), target, backupDir)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if gen != manifest.Generation {
		t.Errorf("peer reports holding generation %d, want %d", gen, manifest.Generation)
	}

	// The digest at the receiver matches the digest at the sender.
	held, err := store.Held(sender.peerID)
	if err != nil || len(held) != 1 {
		t.Fatalf("held: %d entries, err %v", len(held), err)
	}
	if held[0].Digest != manifest.Digest {
		t.Errorf("received digest %s, sent %s", held[0].Digest, manifest.Digest)
	}

	// And the sender can ask what the receiver holds of its control plane.
	gens, err := pusher.HeldBy(t.Context(), target)
	if err != nil {
		t.Fatalf("held-by: %v", err)
	}
	if len(gens) != 1 || gens[0] != manifest.Generation {
		t.Errorf("HeldBy = %v, want [%d]", gens, manifest.Generation)
	}
}

// TestControlBackupPushRefusedFromARevokedPeer proves a peer removed from
// membership cannot push — the mTLS handshake fails, so nothing lands.
func TestControlBackupPushRefusedFromARevokedPeer(t *testing.T) {
	sender := newPeerNode(t, "peer-a", "site-a")
	receiver := newPeerNode(t, "peer-b", "site-b")
	root := newTrustRoot(sender.member(), receiver.member())

	backupDir, _ := signedBackupBy(t, sender.peerID, sender.priv)
	store := backupsync.NewStore(t.TempDir(), 0)
	l := serveWithControlBackup(t, receiver, root, store)

	// Revoke the sender: delete its membership record (ADR-0012).
	root.remove(sender.pub)

	pusher := backupsync.NewPusher(sender.material, slog.New(slog.DiscardHandler))
	target := backupsync.Target{Peer: receiver.member(), Endpoint: "https://" + l.addr}
	if _, err := pusher.PushTo(t.Context(), target, backupDir); err == nil {
		t.Fatal("a revoked peer's push succeeded — revocation did not stop it")
	}
	if held, _ := store.Held(sender.peerID); len(held) != 0 {
		t.Errorf("a revoked peer's backup was stored: %d held", len(held))
	}
}

// TestControlBackupPushRefusesAForeignSource proves a peer cannot push a backup
// whose manifest names a different source than itself.
func TestControlBackupPushRefusesAForeignSource(t *testing.T) {
	sender := newPeerNode(t, "peer-a", "site-a")
	receiver := newPeerNode(t, "peer-b", "site-b")
	other := newPeerNode(t, "peer-c", "site-c")
	root := newTrustRoot(sender.member(), receiver.member())

	// A backup attributed to peer-c but pushed by peer-a.
	backupDir, _ := signedBackupBy(t, other.peerID, other.priv)
	store := backupsync.NewStore(t.TempDir(), 0)
	l := serveWithControlBackup(t, receiver, root, store)

	pusher := backupsync.NewPusher(sender.material, slog.New(slog.DiscardHandler))
	target := backupsync.Target{Peer: receiver.member(), Endpoint: "https://" + l.addr}
	if _, err := pusher.PushTo(t.Context(), target, backupDir); err == nil {
		t.Fatal("a peer pushed a backup under another peer's source and it was accepted")
	}
	if held, _ := store.Held(other.peerID); len(held) != 0 {
		t.Errorf("a foreign-source backup was stored: %d held", len(held))
	}
}
