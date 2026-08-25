package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/backupsync"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/membership"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/persistence/backup"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// pushJSON is the --json shape of a distribution cycle.
type pushJSON struct {
	Generation int64            `json:"generation"`
	Digest     string           `json:"digest"`
	SizeBytes  int64            `json:"size_bytes"`
	Peers      []pushPeerResult `json:"peers"`
}

type pushPeerResult struct {
	PeerID         string `json:"peer_id"`
	Name           string `json:"name"`
	HeldGeneration int64  `json:"held_generation,omitempty"`
	OK             bool   `json:"ok"`
	Error          string `json:"error,omitempty"`
}

func newBackupPushCommand(_ Options, configPath *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Take a backup and push it to every trusted Full Peer (§50, ADR-0046)",
		Long: `Take a control-plane backup and push it to every trusted Full Peer.

A backup on the same disk as the database protects against a mistake, not
against a site (§50). This sends a signed copy to each Full Peer over the mTLS
peer surface — push, not pull, because the bytes are this node's own small state
(ADR-0046). Each peer verifies the signature and the digest before it stores
anything, and holds what it receives inert.

One unreachable peer does not stop the others: the cycle makes progress with
whoever it has and reports who it could not reach, rather than failing. What each
peer confirmed holding is recorded, so this node can later say a peer is a
generation behind even while that peer is unreachable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBackupPush(cmd.Context(), *configPath, cmd.OutOrStdout(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func runBackupPush(ctx context.Context, configPath string, out io.Writer, asJSON bool) error {
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

	// A push must be signed — a peer refuses an unsigned backup — so the identity
	// key is required here, unlike a local `heyarr backup`.
	priv, err := identity.Signer(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("a push needs this node's identity key to sign the backup: %w", err)
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: self})
	if err != nil {
		return fmt.Errorf("building this node's certificate material: %w", err)
	}

	art, err := backup.Take(ctx, backup.TakeOptions{
		DB: db, Events: eventLog, SourcePeerID: self, Signer: priv,
		Dir: filepath.Join(cfg.DataDir, "backups"), Clock: systemClock{},
	})
	if err != nil {
		return err
	}

	members, err := membership.New(membership.Options{DB: db, Events: eventLog})
	if err != nil {
		return err
	}
	all, err := members.List(ctx)
	if err != nil {
		return err
	}
	targets := backupsync.FullPeerTargets(all)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	pusher := backupsync.NewPusher(material, log)
	beliefs := backupsync.NewBeliefs(db)
	outcomes := backupsync.Distribute(ctx, pusher, beliefs, targets, art.Dir, systemClock{}, log)

	return printPush(out, art, targets, outcomes, asJSON)
}

func printPush(out io.Writer, art backup.Artifact, targets []backupsync.Target,
	outcomes []backupsync.Outcome, asJSON bool,
) error {
	nameByID := map[string]string{}
	for _, t := range targets {
		nameByID[t.Peer.PeerID] = t.Peer.Name
	}
	result := pushJSON{
		Generation: art.Manifest.Generation,
		Digest:     art.Manifest.Digest,
		SizeBytes:  art.Manifest.SizeBytes,
	}
	for _, o := range outcomes {
		r := pushPeerResult{PeerID: o.PeerID, Name: nameByID[o.PeerID], OK: o.Err == nil}
		if o.Err != nil {
			r.Error = o.Err.Error()
		} else {
			r.HeldGeneration = o.Generation
		}
		result.Peers = append(result.Peers, r)
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	if _, err := fmt.Fprintf(out, "backup generation %d (%d bytes) pushed to %d peer(s)\n",
		result.Generation, result.SizeBytes, len(result.Peers)); err != nil {
		return err
	}
	for _, p := range result.Peers {
		status := "ok, holds generation " + fmt.Sprint(p.HeldGeneration)
		if !p.OK {
			status = "FAILED: " + p.Error
		}
		if _, err := fmt.Fprintf(out, "  %s (%s): %s\n", p.Name, p.PeerID, status); err != nil {
			return err
		}
	}
	return nil
}
