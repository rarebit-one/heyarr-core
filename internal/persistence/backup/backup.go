package backup

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; see ADR-0004

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// SnapshotFile and ManifestFile are the two files a backup directory holds.
// They are named, not guessed: a directory listing should say what each is.
const (
	SnapshotFile = "snapshot.db"
	ManifestFile = "manifest.json"
)

// Event types for the state transitions this package performs (invariant 7).
const (
	EventTaken     = "control.backup_taken"
	EventRefused   = "control.backup_refused"
	EventRestored  = "control.restore_performed"
	subjectBackup  = "backup"
	subjectRestore = "restore"
)

// busyTimeout bounds how long a read-only open of a snapshot waits for a lock.
const busyTimeout = 5 * time.Second

// Clock is injected so cadence and provenance do not depend on wall time
// (ADR-0017). It matches events.Clock.
type Clock interface{ Now() time.Time }

// Artifact is a taken backup on disk.
type Artifact struct {
	// Dir is the directory holding the snapshot and its manifest.
	Dir      string
	Manifest Manifest
}

// SnapshotPath is the snapshot database file within the artefact.
func (a Artifact) SnapshotPath() string { return filepath.Join(a.Dir, SnapshotFile) }

// TakeOptions configure a single backup.
type TakeOptions struct {
	// DB is the live control database to back up.
	DB *sqlite.DB
	// Events records the state transition. Required: a backup that emits no
	// event is a state transition the log does not know about (invariant 7).
	Events *events.Log
	// SourcePeerID is the peer id whose control plane this is.
	SourcePeerID string
	// Signer, when set, signs the manifest with the origin peer's identity key
	// (ADR-0044 question 2). A backup that will cross to a peer must be signed
	// (M7-03); a single-node backup need not be.
	Signer ed25519.PrivateKey
	// Dir is where the backup directory is created. Each backup lands in a
	// generation-named subdirectory beneath it.
	Dir string
	// Clock provides the read instant. Nil uses the wall clock; a test or the
	// cadence injects its own (ADR-0017).
	Clock Clock
	// Omissions names what this backup does not carry. When nil, the standing
	// omission (provider credentials, which are never in the database) is
	// recorded, because a restore is owed the truthful list even when the caller
	// did not think to pass one.
	Omissions []string
}

// Take writes a whole-database backup of opts.DB into a generation-named
// directory under opts.Dir, and records the transition.
//
// It is idempotent per generation (invariant 9): taking two backups with no
// state change between them yields one directory, because the generation — the
// event high-water mark — did not move. The second call returns the existing
// artefact rather than a second copy of identical bytes.
func (o TakeOptions) validate() error {
	switch {
	case o.DB == nil:
		return errors.New("backup: a database is required")
	case o.Events == nil:
		return errors.New("backup: an event log is required — a backup is a state transition (invariant 7)")
	case o.SourcePeerID == "":
		return errors.New("backup: the source peer id is required")
	case o.Dir == "":
		return errors.New("backup: a destination directory is required")
	}
	return nil
}

func (o TakeOptions) clock() Clock {
	if o.Clock != nil {
		return o.Clock
	}
	return wallClock{}
}

// Take performs the backup. See [TakeOptions].
func Take(ctx context.Context, opts TakeOptions) (Artifact, error) {
	if err := opts.validate(); err != nil {
		return Artifact{}, err
	}

	generation, err := meaningfulGeneration(ctx, opts.DB)
	if err != nil {
		return Artifact{}, err
	}
	if generation <= 0 {
		// Nothing has ever happened on this control plane. A backup at
		// generation zero is refused for catalog.Meta's reason: absent is the
		// absence of a backup, never a backup at zero.
		return Artifact{}, opts.refuse(ctx, generation, "the control plane has recorded no events yet")
	}
	schemaVersion, err := sqlite.AppliedSchemaVersion(ctx, opts.DB)
	if err != nil {
		return Artifact{}, err
	}

	if err := os.MkdirAll(opts.Dir, 0o750); err != nil {
		return Artifact{}, fmt.Errorf("backup: preparing the backup directory: %w", err)
	}
	finalDir := filepath.Join(opts.Dir, generationName(generation))
	if existing, ok, err := loadIfPresent(finalDir); err != nil {
		return Artifact{}, err
	} else if ok {
		// A backup at this generation already exists — the state did not move.
		return existing, nil
	}

	// VACUUM INTO needs a path that does not yet exist, and the write must be
	// atomic against a reader or a crash, so build in a temp directory and
	// rename it into place.
	tmpDir, err := os.MkdirTemp(opts.Dir, ".taking-*")
	if err != nil {
		return Artifact{}, fmt.Errorf("backup: preparing a staging directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	snapPath := filepath.Join(tmpDir, SnapshotFile)
	if err := vacuumInto(ctx, opts.DB, snapPath); err != nil {
		return Artifact{}, err
	}

	digest, size, err := hashing.HashFile(snapPath)
	if err != nil {
		return Artifact{}, fmt.Errorf("backup: hashing the snapshot: %w", err)
	}

	omissions := opts.Omissions
	if omissions == nil {
		omissions = []string{OmitProviderCredentials}
	}
	core := Core{
		SourcePeerID:  opts.SourcePeerID,
		Generation:    generation,
		SchemaVersion: schemaVersion,
		TakenAt:       opts.clock().Now(),
		Digest:        digest.String(),
		SizeBytes:     size,
		Omissions:     omissions,
	}
	manifest := Manifest{Core: core}
	if opts.Signer != nil {
		manifest, err = core.sign(opts.Signer)
		if err != nil {
			return Artifact{}, err
		}
	}
	if err := writeManifest(filepath.Join(tmpDir, ManifestFile), manifest); err != nil {
		return Artifact{}, err
	}

	if err := os.Rename(tmpDir, finalDir); err != nil {
		// A concurrent Take at the same generation may have won the rename; if
		// the destination now exists, that is success, not failure.
		if existing, ok, lerr := loadIfPresent(finalDir); lerr == nil && ok {
			return existing, nil
		}
		return Artifact{}, fmt.Errorf("backup: publishing the backup: %w", err)
	}
	cleanup = false

	art := Artifact{Dir: finalDir, Manifest: manifest}
	if _, err := opts.Events.Emit(ctx, EventTaken, subjectBackup, generationName(generation), takenPayload{
		Generation:    generation,
		SchemaVersion: schemaVersion,
		TakenAt:       core.TakenAt,
		Digest:        core.Digest,
		SizeBytes:     size,
		Signed:        opts.Signer != nil,
	}); err != nil {
		return Artifact{}, err
	}
	return art, nil
}

// refuse records a refused backup and returns an error naming the reason.
func (o TakeOptions) refuse(ctx context.Context, generation int64, reason string) error {
	if o.Events != nil {
		_, _ = o.Events.Emit(ctx, EventRefused, subjectBackup, generationName(generation), map[string]any{
			"generation": generation,
			"reason":     reason,
		})
	}
	return fmt.Errorf("backup: refused: %s", reason)
}

type takenPayload struct {
	Generation    int64     `json:"generation"`
	SchemaVersion int64     `json:"schema_version"`
	TakenAt       time.Time `json:"taken_at"`
	Digest        string    `json:"digest"`
	SizeBytes     int64     `json:"size_bytes"`
	Signed        bool      `json:"signed"`
}

// vacuumInto writes a transactionally consistent, self-contained snapshot.
//
// VACUUM INTO does not accept a bound parameter for the destination path — it
// is a SQL literal — so the path is escaped by doubling single quotes. The path
// is always one this package built under a caller-controlled directory, never
// user input, and the escaping keeps it correct regardless.
func vacuumInto(ctx context.Context, db *sqlite.DB, dest string) error {
	// Run on the writer pool. VACUUM INTO reads a consistent snapshot and does
	// not modify the source, but funnelling it through the single writer keeps
	// it serialised with ordinary writes (ADR-0003) rather than racing them.
	escaped := strings.ReplaceAll(dest, "'", "''")
	if _, err := db.Writer().ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
		return fmt.Errorf("backup: VACUUM INTO %s: %w", dest, err)
	}
	return nil
}

// meaningfulGeneration is the high-water mark of the control plane's own state,
// EXCLUDING the backup subsystem's bookkeeping events.
//
// It is not events.Latest, and the difference is load-bearing. Taking a backup
// emits a backup_taken event (invariant 7), which would itself advance
// events.Latest — so two backups taken back to back with no user change would
// report different generations, the assertion "generation advanced" would become
// a tautology (a mechanism that ran twice reading as one that made progress),
// and the idempotency this function underwrites would be impossible. Generation
// measures what a backup captures, and a backup does not capture its own record
// of having been taken (it is emitted after the snapshot). So two backups of the
// same control-plane state report the SAME generation, and only a real state
// transition moves it.
func meaningfulGeneration(ctx context.Context, db *sqlite.DB) (int64, error) {
	var seq sql.NullInt64
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT max(seq) FROM events WHERE type NOT IN (?, ?, ?)`,
		EventTaken, EventRefused, EventRestored).Scan(&seq); err != nil {
		return 0, fmt.Errorf("backup: reading the control-plane generation: %w", err)
	}
	return seq.Int64, nil
}

// generationName is the directory name for a backup at a generation. Zero-padded
// so a lexical listing is also a chronological one.
func generationName(generation int64) string { return fmt.Sprintf("%020d", generation) }

func writeManifest(path string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: encoding the manifest: %w", err)
	}
	b = append(b, '\n')
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("backup: writing the manifest: %w", err)
	}
	return nil
}

// ReadManifest reads and validates the manifest in a backup directory without
// touching the snapshot.
func ReadManifest(dir string) (Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestFile)) //nolint:gosec // dir is a backup path the caller named, never user input
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: reading the manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("backup: decoding the manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// loadIfPresent returns the artefact at dir if both files are present and the
// manifest is valid. A directory missing either file is treated as absent
// rather than corrupt: it is a torn write from an interrupted Take, and the
// next Take rebuilds it.
func loadIfPresent(dir string) (Artifact, bool, error) {
	if _, err := os.Stat(filepath.Join(dir, SnapshotFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Artifact{}, false, nil
		}
		return Artifact{}, false, fmt.Errorf("backup: looking for a backup: %w", err)
	}
	m, err := ReadManifest(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Artifact{}, false, nil
		}
		return Artifact{}, false, err
	}
	return Artifact{Dir: dir, Manifest: m}, true, nil
}
