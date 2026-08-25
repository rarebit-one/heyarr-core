package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// systemClock is the wall clock for a one-shot CLI backup. The cadence loop
// injects its own; a single operator-driven backup reads real time.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

// backupJSON is the --json shape, golden-tested like every other (ADR-0015).
type backupJSON struct {
	Path          string    `json:"path"`
	Generation    int64     `json:"generation"`
	SchemaVersion int64     `json:"schema_version"`
	TakenAt       time.Time `json:"taken_at"`
	Digest        string    `json:"digest"`
	SizeBytes     int64     `json:"size_bytes"`
	Signed        bool      `json:"signed"`
	Omissions     []string  `json:"omissions"`
}

func newBackupCommand(_ Options, configPath *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Take a whole-database backup of this peer's control plane (§49, ADR-0044)",
		Long: `Write a backup of the control database that a restore can open.

This is host administration, like fsck and gc: it talks to the database
directly rather than through the API, so it works whether or not the controller
is running. The backup is a VACUUM INTO snapshot — a self-contained, consistent
database with no -wal sidecar, so there is no way to end up with a backup that
silently restored to an older state than it was taken from.

Each backup carries its own provenance: which peer's control plane it is, a
monotonic generation (the control plane's event high-water mark, so a backup of
unchanged state reports the same generation), the schema version, and when the
database was read. When this peer's identity key is present, the manifest is
signed with it, so a peer that later holds this backup can verify it came from
here rather than from whoever handed it over.

Provider credentials are never in the database (they live in the config file),
so a backup cannot carry them — it records that omission rather than surprising
a restore with it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackup(cmd.Context(), *configPath, cmd.OutOrStdout(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	cmd.AddCommand(newBackupPushCommand(Options{}, configPath))
	return cmd
}

func runBackup(ctx context.Context, configPath string, out io.Writer, asJSON bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	db, err := sqlite.Open(ctx, sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		return fmt.Errorf("opening the controller database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := sqlite.Migrate(ctx, db); err != nil {
		return fmt.Errorf("migrating the controller database: %w", err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		return err
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: cfg.Peer.Name, PeerSite: cfg.Peer.Site,
	})
	if err != nil {
		return err
	}
	self, err := cat.SelfPeer(ctx)
	if err != nil {
		return fmt.Errorf("resolving this peer's identity: %w", err)
	}

	art, err := backup.Take(ctx, backup.TakeOptions{
		DB:           db,
		Events:       eventLog,
		SourcePeerID: self,
		Signer:       optionalSigner(cfg.DataDir),
		Dir:          filepath.Join(cfg.DataDir, "backups"),
		Clock:        systemClock{},
	})
	if err != nil {
		return err
	}
	return printBackup(out, art, asJSON)
}

// optionalSigner loads the identity key to sign the manifest, or returns nil.
//
// Nil is the safe answer, not the convenient one, and it mirrors fsck's
// durabilityFor: a node without a peer key on disk (the ordinary single-peer
// early-milestone shape) still takes a backup, just an unsigned one. A backup
// that must cross to a peer is signed at that point (M7-03); a local one need
// not be, and refusing to take one because the key was absent would be refusing
// the backup a disaster most needs.
func optionalSigner(dataDir string) ed25519.PrivateKey {
	priv, err := identity.Signer(dataDir)
	if err != nil {
		return nil
	}
	return priv
}

func printBackup(out io.Writer, art backup.Artifact, asJSON bool) error {
	m := art.Manifest
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(backupJSON{
			Path:          art.Dir,
			Generation:    m.Generation,
			SchemaVersion: m.SchemaVersion,
			TakenAt:       m.TakenAt,
			Digest:        m.Digest,
			SizeBytes:     m.SizeBytes,
			Signed:        m.Signature != "",
			Omissions:     m.Omissions,
		})
	}
	signed := "unsigned"
	if m.Signature != "" {
		signed = "signed"
	}
	_, err := fmt.Fprintf(out, "backup generation %d taken (%s, schema %d, %d bytes)\n  %s\n",
		m.Generation, signed, m.SchemaVersion, m.SizeBytes, art.Dir)
	return err
}
