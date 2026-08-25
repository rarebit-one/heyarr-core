package backupsync

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
)

// ErrSourceMismatch is a peer pushing a backup whose manifest names a different
// source than the peer itself. A peer may push its OWN control plane, never
// another's under its name.
var ErrSourceMismatch = errors.New("backupsync: the backup's source does not match the peer pushing it")

// ErrBadSourceID is a source peer id that is not a safe single path component.
var ErrBadSourceID = errors.New("backupsync: the source peer id is not a safe path component")

// DefaultRetain is how many generations a peer keeps per source when the caller
// does not say. More than one, because keeping only the newest leaves nothing
// when the newest turns out to be the copy written during the incident; a small
// bound, because a control plane is small and a homelab disk is not infinite.
const DefaultRetain = 3

// Store holds control-plane backups pushed to this peer (§50), keyed by the
// SOURCE peer's id and the generation. Everything it holds is inert: it is
// verified before it lands and never opened as a control plane.
type Store struct {
	root   string
	retain int
}

// NewStore roots a receiver store at dir (conventionally
// <data_dir>/received-backups), keeping the newest retain generations per
// source (zero uses [DefaultRetain]). It is created on first write.
func NewStore(dir string, retain int) *Store {
	if retain <= 0 {
		retain = DefaultRetain
	}
	return &Store{root: dir, retain: retain}
}

// ReceivedPathFor is the conventional receiver-store location beneath a data
// directory. It is deliberately NOT the catalog snapshot's path: the two
// artefacts are different things for different disasters (§50), and a directory
// listing should say so.
func ReceivedPathFor(dataDir string) string {
	return filepath.Join(dataDir, "received-backups")
}

// Receive verifies a pushed backup and stores it under the source peer.
//
// Verification is [backup.Open]'s, reused whole: the manifest signature against
// sourceKey (a pushed backup MUST be signed, so an unsigned one is refused), the
// snapshot digest against its bytes, integrity_check and foreign_key_check, and
// the read-only open that proves it cannot be run as a control plane. Only then
// is it moved into place. retain bounds how many generations are kept for this
// source; zero uses [DefaultRetain].
func (s *Store) Receive(ctx context.Context, sourcePeerID string, sourceKey ed25519.PublicKey,
	manifest backup.Manifest, snapshot io.Reader,
) (backup.Manifest, error) {
	if err := safeComponent(sourcePeerID); err != nil {
		return backup.Manifest{}, err
	}
	if err := manifest.Validate(); err != nil {
		return backup.Manifest{}, err
	}
	if manifest.SourcePeerID != sourcePeerID {
		return backup.Manifest{}, fmt.Errorf("%w: manifest says %q, pushed by %q",
			ErrSourceMismatch, manifest.SourcePeerID, sourcePeerID)
	}

	sourceDir := filepath.Join(s.root, sourcePeerID)
	if err := os.MkdirAll(sourceDir, 0o750); err != nil {
		return backup.Manifest{}, fmt.Errorf("backupsync: preparing the source directory: %w", err)
	}

	// Stage the bundle in a temp directory, verify it there, and only move it
	// into place once [backup.Open] has accepted it. A half-written or unverified
	// bundle never appears under its generation.
	tmpDir, err := os.MkdirTemp(sourceDir, ".receiving-*")
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("backupsync: preparing a staging directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	if err := writeBundle(tmpDir, manifest, snapshot); err != nil {
		return backup.Manifest{}, err
	}
	opened, err := backup.Open(ctx, tmpDir, backup.OpenOptions{PublicKey: sourceKey})
	if err != nil {
		return backup.Manifest{}, fmt.Errorf("backupsync: a pushed backup failed verification: %w", err)
	}
	_ = opened.Close()

	finalDir := filepath.Join(sourceDir, genDir(manifest.Generation))
	if _, err := os.Stat(finalDir); err == nil {
		// Already held at this generation — idempotent, the same bytes verified.
		return manifest, nil
	}
	if err := os.Rename(tmpDir, finalDir); err != nil {
		return backup.Manifest{}, fmt.Errorf("backupsync: storing the received backup: %w", err)
	}
	cleanup = false

	if err := s.prune(sourcePeerID); err != nil {
		// Retention is housekeeping; a failure to prune does not unstore a good
		// backup that already landed. Report it without discarding the receive.
		return manifest, fmt.Errorf("backupsync: stored the backup but could not prune old generations: %w", err)
	}
	return manifest, nil
}

// ReceiveBackup is the peer-surface adapter: it parses the raw manifest bytes a
// pushing peer sent and stores the backup, returning the generation now held.
// It is the method the peer-surface handler calls, kept in primitive types so
// the API layer need not know the manifest's shape.
func (s *Store) ReceiveBackup(ctx context.Context, sourcePeerID string, sourceKey ed25519.PublicKey,
	manifestJSON []byte, snapshot io.Reader,
) (int64, error) {
	var m backup.Manifest
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return 0, fmt.Errorf("backupsync: decoding the pushed manifest: %w", err)
	}
	stored, err := s.Receive(ctx, sourcePeerID, sourceKey, m, snapshot)
	if err != nil {
		return 0, err
	}
	return stored.Generation, nil
}

// HeldBackups lists the generations held for a source, newest first — the
// primitive form the peer-surface handler reports.
func (s *Store) HeldBackups(sourcePeerID string) ([]int64, error) {
	held, err := s.Held(sourcePeerID)
	if err != nil {
		return nil, err
	}
	gens := make([]int64, len(held))
	for i, m := range held {
		gens[i] = m.Generation
	}
	return gens, nil
}

// Held lists the manifests this peer holds for a source, newest generation
// first. A source with nothing held returns an empty slice, not an error.
func (s *Store) Held(sourcePeerID string) ([]backup.Manifest, error) {
	if err := safeComponent(sourcePeerID); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.root, sourcePeerID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("backupsync: listing held backups: %w", err)
	}
	var held []backup.Manifest
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		m, err := backup.ReadManifest(filepath.Join(s.root, sourcePeerID, e.Name()))
		if err != nil {
			// A torn directory from an interrupted receive is skipped, not fatal:
			// the next push rebuilds it.
			continue
		}
		held = append(held, m)
	}
	sort.Slice(held, func(i, j int) bool { return held[i].Generation > held[j].Generation })
	return held, nil
}

// Latest returns the newest generation held for a source.
func (s *Store) Latest(sourcePeerID string) (backup.Manifest, bool, error) {
	held, err := s.Held(sourcePeerID)
	if err != nil {
		return backup.Manifest{}, false, err
	}
	if len(held) == 0 {
		return backup.Manifest{}, false, nil
	}
	return held[0], true, nil
}

// PathFor is the directory holding a specific generation from a source, for a
// restore to read (M7-04).
func (s *Store) PathFor(sourcePeerID string, generation int64) string {
	return filepath.Join(s.root, sourcePeerID, genDir(generation))
}

// prune keeps only the newest s.retain generations for a source.
func (s *Store) prune(sourcePeerID string) error {
	held, err := s.Held(sourcePeerID)
	if err != nil {
		return err
	}
	for _, m := range held[min(s.retain, len(held)):] {
		if err := os.RemoveAll(s.PathFor(sourcePeerID, m.Generation)); err != nil {
			return err
		}
	}
	return nil
}

// writeBundle writes the manifest and snapshot into dir in the layout
// [backup.Open] expects.
func writeBundle(dir string, manifest backup.Manifest, snapshot io.Reader) error {
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("backupsync: encoding the manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("backupsync: writing the manifest: %w", err)
	}
	//nolint:gosec // dir is a staging directory this package created under its own root
	f, err := os.OpenFile(filepath.Join(dir, backup.SnapshotFile), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("backupsync: creating the snapshot file: %w", err)
	}
	if _, err := io.Copy(f, snapshot); err != nil {
		_ = f.Close()
		return fmt.Errorf("backupsync: writing the snapshot: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("backupsync: syncing the snapshot: %w", err)
	}
	return f.Close()
}

// genDir is the directory name for a generation, zero-padded so a lexical
// listing is chronological — the same shape the producer uses.
func genDir(generation int64) string { return fmt.Sprintf("%020d", generation) }

// safeComponent refuses a peer id that is not a single, non-traversing path
// component. Peer ids are hex-ish identifiers, never paths, and this keeps a
// crafted one from escaping the store root.
func safeComponent(id string) error {
	if id == "" || id == "." || id == ".." ||
		strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		return fmt.Errorf("%w: %q", ErrBadSourceID, id)
	}
	return nil
}
