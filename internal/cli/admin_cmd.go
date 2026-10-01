package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/deviceauth"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// newAdminCommand builds `heyarr admin`: host administration of state no API
// route changes. Like `token`, it opens the controller database directly — it
// is not a role, and it must work with the servers stopped (see
// newTokenCommand for why that does not cross ADR-0002).
func newAdminCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Host administration of the controller database",
		Long: `Host administration that no API route performs.

These commands operate on the controller database directly and must be run on
the host, as the user that owns the data directory. They work with the
controller stopped or running: the database is single-writer (ADR-0003), so a
command waits for the controller's write lock (up to the busy timeout) rather
than racing it.`,
	}
	user := &cobra.Command{
		Use:   "user",
		Short: "Administer pinned user identities",
	}
	user.AddCommand(newAdminUserRekeyCommand(opts, configPath))
	cmd.AddCommand(user)
	return cmd
}

// withDeviceAuth opens and migrates the controller database and hands a device
// identity store to fn, the deviceauth counterpart of withStore.
func withDeviceAuth(ctx context.Context, configPath string, fn func(context.Context, *deviceauth.Store) error) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	db, err := sqlite.Open(ctx, sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		return fmt.Errorf("admin: opening the controller database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := sqlite.Migrate(ctx, db); err != nil {
		return fmt.Errorf("admin: migrating the controller database: %w", err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		return err
	}
	store, err := deviceauth.New(deviceauth.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		return err
	}
	return fn(ctx, store)
}

// userRekeyJSON is the --json shape of `admin user rekey`. Every key in it is a
// PUBLIC key; nothing secret passes through this command.
type userRekeyJSON struct {
	UserIdentityID           string `json:"user_identity_id"`
	PrincipalID              string `json:"principal_id"`
	Name                     string `json:"name"`
	OldPublicKey             string `json:"old_public_key"`
	PublicKey                string `json:"public_key"`
	OldRecoveryEncryptionKey string `json:"old_recovery_encryption_key"`
	RecoveryEncryptionKey    string `json:"recovery_encryption_key"`
	OpsDeleted               int    `json:"ops_deleted"`
}

func newAdminUserRekeyCommand(_ Options, configPath *string) *cobra.Command {
	var (
		recoveryKey string
		asJSON      bool
	)
	cmd := &cobra.Command{
		Use:   "rekey <principal> <ed25519:public-key>",
		Short: "Replace a user identity's pinned key in place (void-which-binds ADR-0022)",
		Long: `Replace a pinned user identity's signing key and recovery encryption key in
place — the gen1 to gen2 cutover (void-which-binds ADR-0022, C2 step 6).

<principal> is a principal id, a user identity id or a principal name, and must
name exactly one pinned user. The principal and its user identity keep their
ids, so nothing keyed by them changes; the identity is NOT revoked and re-pinned.
The membership ops recorded under the old key are deleted, since every one of
them was signed by it.

Devices are untouched. Revoke each old-generation device separately with
'heyarr device revoke --no-rotate <device-key>'; new devices enrol under the new
key. Once this runs, nothing signed by the old key authenticates here, and
there is no undo short of rekeying back.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if recoveryKey == "" {
				return errors.New("admin: --recovery-key is required (the identity's x25519 recovery public key)")
			}
			return withDeviceAuth(cmd.Context(), *configPath, func(ctx context.Context, store *deviceauth.Store) error {
				rk, err := store.RekeyUser(ctx, args[0], args[1], recoveryKey)
				if err != nil {
					return err
				}
				return printUserRekey(cmd.OutOrStdout(), rk, asJSON)
			})
		},
	}
	cmd.Flags().StringVar(&recoveryKey, "recovery-key", "",
		"the identity's new x25519 recovery encryption PUBLIC key (x25519:<hex>; required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func printUserRekey(w io.Writer, rk deviceauth.Rekey, asJSON bool) error {
	if asJSON {
		return emitJSON(w, userRekeyJSON{
			UserIdentityID:           rk.User.ID,
			PrincipalID:              rk.User.PrincipalID,
			Name:                     rk.User.Name,
			OldPublicKey:             rk.OldPublicKey,
			PublicKey:                rk.User.PublicKey,
			OldRecoveryEncryptionKey: rk.OldRecoveryEncryptionKey,
			RecoveryEncryptionKey:    rk.User.RecoveryEncryptionKey,
			OpsDeleted:               rk.OpsDeleted,
		})
	}
	oldRecovery := rk.OldRecoveryEncryptionKey
	if oldRecovery == "" {
		oldRecovery = "(none)"
	}
	fmt.Fprintf(w, "rekeyed user %s (principal %s)\n\n", rk.User.Name, rk.User.PrincipalID)
	fmt.Fprintf(w, "  public key    %s\n", rk.OldPublicKey)
	fmt.Fprintf(w, "             -> %s\n", rk.User.PublicKey)
	fmt.Fprintf(w, "  recovery key  %s\n", oldRecovery)
	fmt.Fprintf(w, "             -> %s\n", rk.User.RecoveryEncryptionKey)
	fmt.Fprintf(w, "  ops deleted   %d\n\n", rk.OpsDeleted)
	fmt.Fprintln(w, "Devices are untouched: revoke each old-key device with `heyarr device revoke --no-rotate <device-key>`.")
	return nil
}
