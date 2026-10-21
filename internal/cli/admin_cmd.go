package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

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
	user.AddCommand(newAdminUserListCommand(opts, configPath))
	user.AddCommand(newAdminUserPinCommand(opts, configPath))
	user.AddCommand(newAdminUserRekeyCommand(opts, configPath))
	cmd.AddCommand(user, newAdminManagementCommand(opts, configPath))
	return cmd
}

// withDeviceAuth opens and migrates the controller database and hands a device
// identity store to fn, the deviceauth counterpart of withStore.
func withDeviceAuth(ctx context.Context, configPath string, fn func(context.Context, *deviceauth.Store) error) error {
	cfg, err := config.LoadFromContext(ctx, configPath)
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

// userListJSON is one element of the --json shape of `admin user list`. Every
// key in it is a PUBLIC key; nothing secret passes through this command.
type userListJSON struct {
	UserIdentityID        string    `json:"user_identity_id"`
	PrincipalID           string    `json:"principal_id"`
	Name                  string    `json:"name"`
	PublicKey             string    `json:"public_key"`
	RecoveryEncryptionKey string    `json:"recovery_encryption_key"`
	EnrolledAt            time.Time `json:"enrolled_at"`
	ActiveDevices         int       `json:"active_devices"`
	ExpiredDevices        int       `json:"expired_devices"`
	RevokedDevices        int       `json:"revoked_devices"`
}

// noUsersPinned is what `admin user list` says when the controller has pinned
// no one. It names the consequence, not just the absence: a node with no pin
// refuses every device credential with the same opaque 401 it gives a bad one,
// so an empty list is the answer to "why can't my phone sign in here?".
const noUsersPinned = "no users pinned on this controller; devices cannot authenticate here " +
	"(pin with `heyarr admin user pin`)"

func newAdminUserListCommand(_ Options, configPath *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the user identities pinned on this controller",
		Long: `List every user identity pinned in this controller's database: its name, its
signing and recovery public keys, when it was pinned, and how many of its
devices are enrolled here (active, expired and revoked).

A device authenticates at a controller only if its user is pinned THERE: pins
are local and peers do not replicate them. A controller that lists no users
refuses every device credential, with the same 401 any bad credential gets.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withDeviceAuth(cmd.Context(), *configPath, func(ctx context.Context, store *deviceauth.Store) error {
				users, err := store.ListUsers(ctx)
				if err != nil {
					return err
				}
				counts, err := store.DeviceCountsByUser(ctx)
				if err != nil {
					return err
				}
				return printUserList(cmd.OutOrStdout(), users, counts, asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func printUserList(w io.Writer, users []deviceauth.User, counts map[string]deviceauth.DeviceCounts, asJSON bool) error {
	if asJSON {
		out := make([]userListJSON, 0, len(users))
		for _, u := range users {
			c := counts[u.ID]
			out = append(out, userListJSON{
				UserIdentityID:        u.ID,
				PrincipalID:           u.PrincipalID,
				Name:                  u.Name,
				PublicKey:             u.PublicKey,
				RecoveryEncryptionKey: u.RecoveryEncryptionKey,
				EnrolledAt:            u.EnrolledAt.UTC(),
				ActiveDevices:         c.Active,
				ExpiredDevices:        c.Expired,
				RevokedDevices:        c.Revoked,
			})
		}
		return emitJSON(w, out)
	}
	if len(users) == 0 {
		fmt.Fprintln(w, noUsersPinned)
		return nil
	}
	for i, u := range users {
		if i > 0 {
			fmt.Fprintln(w)
		}
		recovery := u.RecoveryEncryptionKey
		if recovery == "" {
			recovery = "(none)"
		}
		c := counts[u.ID]
		fmt.Fprintf(w, "%s (principal %s)\n", u.Name, u.PrincipalID)
		fmt.Fprintf(w, "  public key    %s\n", u.PublicKey)
		fmt.Fprintf(w, "  recovery key  %s\n", recovery)
		fmt.Fprintf(w, "  enrolled at   %s\n", u.EnrolledAt.UTC().Format(time.RFC3339))
		fmt.Fprintf(w, "  devices       %d active, %d expired, %d revoked\n", c.Active, c.Expired, c.Revoked)
	}
	return nil
}

// userPinJSON is the --json shape of `admin user pin`. Every key in it is a
// PUBLIC key; nothing secret passes through this command.
type userPinJSON struct {
	UserIdentityID        string `json:"user_identity_id"`
	PrincipalID           string `json:"principal_id"`
	Name                  string `json:"name"`
	PublicKey             string `json:"public_key"`
	RecoveryEncryptionKey string `json:"recovery_encryption_key"`
}

func newAdminUserPinCommand(_ Options, configPath *string) *cobra.Command {
	var (
		recoveryKey string
		asJSON      bool
	)
	cmd := &cobra.Command{
		Use:   "pin <name> <ed25519:public-key>",
		Short: "Pin a user identity's public key (ADR-0032), without the HTTP API",
		Long: `Pin a user identity: create its principal (kind 'user') and register its
signing key and recovery encryption key, the out-of-band step that ADR-0032
requires before anything the user signs is honoured — the same write as
POST /api/v1/identities/users.

It exists for a controller that does not mount that route: the Mnemosyne
personal profile mounts no identities API, so this is how its first user is
pinned. On heyarr it is equivalent to the route.

Both keys are PUBLIC. --recovery-key is required: an identity pinned without
one cannot have new spaces wrapped for its paper recovery secret (§41).

A pin is local to this controller's database; peers do not replicate it. Pin
the user on every controller its devices enrol at.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(recoveryKey) == "" {
				return errors.New("admin: --recovery-key is required (the identity's x25519 recovery public key)")
			}
			if strings.TrimSpace(args[0]) == "" {
				return errors.New("admin: <name> must not be empty")
			}
			return withDeviceAuth(cmd.Context(), *configPath, func(ctx context.Context, store *deviceauth.Store) error {
				u, err := store.EnrolUser(ctx, args[1], args[0], recoveryKey)
				if err != nil {
					return err
				}
				return printUserPin(cmd.OutOrStdout(), u, asJSON)
			})
		},
	}
	cmd.Flags().StringVar(&recoveryKey, "recovery-key", "",
		"the identity's x25519 recovery encryption PUBLIC key (x25519:<hex>; required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func printUserPin(w io.Writer, u deviceauth.User, asJSON bool) error {
	if asJSON {
		return emitJSON(w, userPinJSON{
			UserIdentityID:        u.ID,
			PrincipalID:           u.PrincipalID,
			Name:                  u.Name,
			PublicKey:             u.PublicKey,
			RecoveryEncryptionKey: u.RecoveryEncryptionKey,
		})
	}
	fmt.Fprintf(w, "pinned user %s (principal %s)\n\n", u.Name, u.PrincipalID)
	fmt.Fprintf(w, "  public key    %s\n", u.PublicKey)
	fmt.Fprintf(w, "  recovery key  %s\n\n", u.RecoveryEncryptionKey)
	fmt.Fprintln(w, "Pins are local to this controller: pin the same key on every controller the user's devices enrol at.")
	return nil
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
			if strings.TrimSpace(recoveryKey) == "" {
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
