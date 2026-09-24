package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rarebit-one/voidbind-go/encryption"
	"github.com/rarebit-one/voidbind-go/identity"
	"github.com/rarebit-one/voidbind-go/useridentity"
	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spacerecover"
)

// blobClock is the export's clock, a variable so a test can pin generated_at.
var blobClock = time.Now

// spaceExportView is the --json shape of `space export-recovery`. Like the
// recover result it carries no key material.
type spaceExportView struct {
	Out         string   `json:"out"`
	Recipient   string   `json:"recipient"`
	UserID      string   `json:"user_id,omitempty"`
	GeneratedAt string   `json:"generated_at"`
	SpaceIDs    []string `json:"space_ids"`
}

// staleBlobHint is what `space create` and `space rotate` say after changing
// which keys a recovery blob should hold.
const staleBlobHint = "note: an exported recovery blob does not include this change — " +
	"re-export it with `heyarr space export-recovery` if you keep one"

func newSpaceExportRecoveryCommand(_ Options, configPath *string) *cobra.Command {
	var (
		out         string
		recipient   string
		identityDir string
		asJSON      bool
	)
	cmd := &cobra.Command{
		Use:   "export-recovery --out <file>",
		Short: "Export every space's recovery-wrapped key into one recovery blob (ADR-0022)",
		Long: `Export an encrypted recovery blob: the copy of every space's key that is
wrapped for your recovery key, gathered from this node's control database into
one small file you can keep anywhere — a USB stick, a cloud drive, an email to
yourself.

With it, ` + "`heyarr space recover --from-blob`" + ` recovers your space keys from the
recovery secret even when no control database survives. It is a DURABILITY aid,
not extra protection: the file is sealed so that only your recovery secret opens
it, and it holds nothing the peers do not already hold (ADR-0022).

The blob is sealed to your recovery PUBLIC key, so exporting needs no secret —
the key comes from your user identity (--identity-dir) or --recipient. It is a
snapshot: a space created or re-keyed afterwards is missing from it, so
re-export after either.

This reads the control database directly (--config), so it works with the
controller running or stopped.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSpaceExportRecovery(cmd.Context(), cmd, *configPath, identityDir, recipient, out, asJSON)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write the recovery blob to this file (required)")
	cmd.Flags().StringVar(&recipient, "recipient", "", "the recovery key to export for (x25519:<hex>); default: your user identity's")
	cmd.Flags().StringVar(&identityDir, "identity-dir", "",
		"where your user identity lives (default: your config directory; "+useridentity.EnvDir+" overrides)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

func runSpaceExportRecovery(ctx context.Context, cmd *cobra.Command, configPath, identityDir, recipient, out string, asJSON bool) error {
	userID := ""
	if recipient == "" {
		store, err := openUserIdentityStore(identityDir)
		if err != nil {
			return err
		}
		id, err := store.Get()
		if errors.Is(err, useridentity.ErrNoIdentity) {
			return fmt.Errorf("no user identity here to take the recovery key from — pass --recipient x25519:<hex> or --identity-dir: %w", err)
		}
		if err != nil {
			return err
		}
		if id.EncryptionKey == "" {
			return fmt.Errorf("%w — regenerate your identity, or pass --recipient", errNoRecoveryKey)
		}
		recipient = id.EncryptionKey
		userID = identity.FormatPublicKey(id.PublicKey)
	}
	if _, err := encryption.ParsePublicKey(recipient); err != nil {
		return fmt.Errorf("--recipient: %w", err)
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	db, err := sqlite.Open(ctx, sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		return fmt.Errorf("opening the controller database: %w", err)
	}
	defer func() { _ = db.Close() }()

	spaces, err := recoverySpacesFor(ctx, db.Reader(), recipient)
	if err != nil {
		return err
	}
	if len(spaces) == 0 {
		return fmt.Errorf("no space keys are wrapped for recovery key %s here — there is nothing to export", recipient)
	}

	blob := spacerecover.Blob{
		UserID:            userID,
		RecoveryRecipient: recipient,
		GeneratedAt:       blobClock().UTC().Truncate(time.Second),
		Spaces:            spaces,
	}
	data, err := spacerecover.SealBlob(blob)
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, data, 0o600); err != nil {
		return fmt.Errorf("writing the recovery blob: %w", err)
	}

	view := spaceExportView{
		Out:         out,
		Recipient:   recipient,
		UserID:      userID,
		GeneratedAt: blob.GeneratedAt.Format(time.RFC3339),
		SpaceIDs:    make([]string, 0, len(spaces)),
	}
	for _, s := range spaces {
		view.SpaceIDs = append(view.SpaceIDs, s.SpaceID)
	}
	if asJSON {
		return emitJSON(cmd.OutOrStdout(), view)
	}
	printSpaceExport(cmd.OutOrStdout(), view)
	return nil
}

// recoverySpacesFor reads every space's copy wrapped for the recovery recipient,
// with the space's kind, sorted by space id.
func recoverySpacesFor(ctx context.Context, r *sql.DB, recipient string) ([]spacerecover.BlobSpace, error) {
	rows, err := r.QueryContext(ctx, `
		SELECT s.id, s.kind, w.wrapped
		FROM wrapped_keys w JOIN encrypted_spaces s ON s.id = w.space_id
		WHERE w.recipient = ? ORDER BY s.id`, recipient)
	if err != nil {
		return nil, fmt.Errorf("reading wrapped keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []spacerecover.BlobSpace
	for rows.Next() {
		var s spacerecover.BlobSpace
		if err := rows.Scan(&s.SpaceID, &s.Kind, &s.Wrapped); err != nil {
			return nil, fmt.Errorf("scanning a wrapped key: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating wrapped keys: %w", err)
	}
	return out, nil
}

func printSpaceExport(w io.Writer, v spaceExportView) {
	fmt.Fprintf(w, "Exported the recovery copies of %d space(s) to %s.\n", len(v.SpaceIDs), v.Out)
	fmt.Fprintf(w, "  sealed for recovery key %s — only your recovery secret opens it\n", v.Recipient)
	fmt.Fprintf(w, "  generated %s — re-export after creating or re-keying a space\n", v.GeneratedAt)
}
