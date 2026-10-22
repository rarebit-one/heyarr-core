package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// pinVault stores bytes as a legacy vault blob: its blobs row and this node's
// placement pin, with no asset, the way a media node held vault data before the
// split (#823).
func (f *integrityFixture) pinVault(contents string) string {
	f.t.Helper()
	desc, err := f.store.Put(f.t.Context(), strings.NewReader(contents))
	if err != nil {
		f.t.Fatal(err)
	}
	dbPath := filepath.Join(f.dir, "heyarr.db")
	// seed writes a fresh template every time; this keeps whatever is there, so
	// seed the library media first and pin the vault blob after.
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		testdb.WriteMigrated(f.t, dbPath)
	}
	db, err := sqlite.Open(f.t.Context(), sqlite.Options{Path: dbPath})
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		f.t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{DB: db, Events: eventLog, PeerName: "test", PeerSite: "test-site"})
	if err != nil {
		f.t.Fatal(err)
	}
	self, err := cat.SelfPeer(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	if err := cat.RecordVaultBlob(f.t.Context(), desc.Hash.String(), desc.Size, self, nil); err != nil {
		f.t.Fatal(err)
	}
	return desc.Hash.String()
}

// runHeyarr drives heyarr's own tree. The shared run helper sends every `vault`
// command to Mnemosyne's, whose vault is the client; heyarr's `vault` holds only
// the legacy purge.
func runHeyarr(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	cmd := NewRootCommand(Options{Stdout: &out, Stderr: &errb, ShutdownGrace: 2 * time.Second})
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

// hasBlob is whether the store still holds a file for hash, walking the store
// the way blobFile does rather than assuming its layout.
func (f *integrityFixture) hasBlob(hash string) bool {
	f.t.Helper()
	hex := hash[len("blake3:"):]
	found := false
	err := filepath.WalkDir(f.store.Root(), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if d.Name() == hex && strings.Contains(path, filepath.Join("blobs", "blake3")) {
			found = true
		}
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return found
}

func (f *integrityFixture) pinCount() int {
	f.t.Helper()
	db, err := sqlite.Open(f.t.Context(), sqlite.Options{Path: filepath.Join(f.dir, "heyarr.db")})
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.Reader().QueryRowContext(f.t.Context(), "SELECT count(*) FROM placement_pins").Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// The whole operator path on a media node (#823): a purge without --yes
// changes nothing, a real one releases the legacy vault blob, and gc with
// --honour-releases then reclaims it after the window, while library media in
// the same store stays.
func TestVaultPurgeReleasesLegacyVaultForGC(t *testing.T) {
	f := newIntegrityFixture(t)
	media := f.seed("library media", true)
	vault := f.pinVault("legacy vault ciphertext")
	ctx := context.Background()

	out, err := runHeyarr(t, ctx, "--config", f.configPath, "vault", "purge")
	if err != nil {
		t.Fatalf("vault purge: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dry run") || !strings.Contains(out, "would be released   1") {
		t.Fatalf("the dry run did not report the legacy pin:\n%s", out)
	}
	if n := f.pinCount(); n != 1 {
		t.Fatalf("the dry run changed the pins: %d left, want 1", n)
	}

	out, err = runHeyarr(t, ctx, "--config", f.configPath, "vault", "purge", "--yes")
	if err != nil {
		t.Fatalf("vault purge --yes: %v\n%s", err, out)
	}
	if !strings.Contains(out, "released            1") || !strings.Contains(out, "heyarr gc --honour-releases --apply") {
		t.Fatalf("the purge did not report the release and the next steps:\n%s", out)
	}
	if n := f.pinCount(); n != 0 {
		t.Fatalf("pins left after the purge: %d", n)
	}

	// A second run is a no-op.
	out, err = runHeyarr(t, ctx, "--config", f.configPath, "vault", "purge", "--yes")
	if err != nil || !strings.Contains(out, "nothing to purge") {
		t.Fatalf("a second purge was not a no-op (%v):\n%s", err, out)
	}

	// gc: the first applying sweep starts the window, the next reclaims.
	if _, _, err := run(t, ctx, "--config", f.configPath, "gc", "--honour-releases", "--apply"); err != nil {
		t.Fatal(err)
	}
	out, _, err = run(t, ctx, "--config", f.configPath, "gc", "--honour-releases", "--apply", "--grace", "1ns")
	if err != nil {
		t.Fatalf("gc: %v\n%s", err, out)
	}
	if f.hasBlob(vault) {
		t.Fatalf("the released legacy vault blob is still in the store:\n%s", out)
	}
	if !strings.Contains(out, "reclaimed           1") {
		t.Fatalf("gc did not report reclaiming the released blob:\n%s", out)
	}
	if !f.hasBlob(media) {
		t.Fatalf("library media was reclaimed:\n%s", out)
	}
}

func TestVaultPurgeRefusesThePersonalProfile(t *testing.T) {
	f := newIntegrityFixture(t)
	body, err := os.ReadFile(f.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.configPath, append(body, []byte("profile: personal\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	f.pinVault("a device's live vault blob")

	_, err = runHeyarr(t, context.Background(), "--config", f.configPath, "vault", "purge", "--yes")
	if err == nil || !strings.Contains(err.Error(), "personal profile") {
		t.Fatalf("vault purge on the personal profile = %v, want a refusal", err)
	}
	if n := f.pinCount(); n != 1 {
		t.Fatalf("the refused purge changed the pins: %d left, want 1", n)
	}
}
