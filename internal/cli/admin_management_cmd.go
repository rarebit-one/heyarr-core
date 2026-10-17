package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// newAdminManagementCommand builds `heyarr admin management`: the host-side door
// to the ADR-0065 write authorisation of a device, the same rows
// POST/DELETE /api/v1/session/management-grants write. The Mnemosyne personal
// profile does not mount that route, yet a device needs write there to create
// a space it owns, register a service recipient and grant an executor
// (ADR-0104), so without this nothing on that profile could ever carry write.
func newAdminManagementCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "management",
		Short: "Authorise enrolled devices for write (ADR-0065), without the HTTP API",
		Long: `Authorise an enrolled device for write: with a grant, the device's own
credential carries write as well as read (ADR-0065). This is the same record
as POST /api/v1/session/management-grants, written on the host — the Mnemosyne
personal profile does not mount that route.

A grant names a device KEY, not a user: grant only a device you control, and
revoke it when the device is retired. Grants are local to this controller.`,
	}
	cmd.AddCommand(
		newAdminManagementGrantCommand(opts, configPath),
		newAdminManagementRevokeCommand(opts, configPath),
		newAdminManagementListCommand(opts, configPath),
	)
	return cmd
}

// withCatalog opens and migrates the controller database and hands the catalog
// to fn, the management-grant counterpart of withDeviceAuth.
func withCatalog(ctx context.Context, configPath string, fn func(context.Context, *catalog.Catalog) error) error {
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
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: cfg.Peer.Name, PeerSite: cfg.Peer.Site,
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		return err
	}
	return fn(ctx, cat)
}

// canonicalDeviceKey validates a device key and renders it the way the auth path
// compares it. A grant stored under any other spelling would never match.
func canonicalDeviceKey(s string) (string, error) {
	pub, err := identity.ParsePublicKey(strings.TrimSpace(s))
	if err != nil {
		return "", fmt.Errorf("admin: %q is not a device key (ed25519:<hex>): %w", s, err)
	}
	return identity.FormatPublicKey(pub), nil
}

func newAdminManagementGrantCommand(_ Options, configPath *string) *cobra.Command {
	var (
		reason string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "grant <ed25519:device-key>",
		Short: "Authorise a device for write",
		Long: `Authorise a device for write. Idempotent: granting an authorised device again
updates its reason. --reason is required, so the event log says why.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := canonicalDeviceKey(args[0])
			if err != nil {
				return err
			}
			if strings.TrimSpace(reason) == "" {
				return errors.New("admin: --reason is required (recorded with the grant and its event)")
			}
			return withCatalog(cmd.Context(), *configPath, func(ctx context.Context, cat *catalog.Catalog) error {
				g, err := cat.GrantManagement(ctx, key, strings.TrimSpace(reason))
				if err != nil {
					return err
				}
				if asJSON {
					return emitJSON(cmd.OutOrStdout(), g)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "device %s is authorised for write (%s)\n", g.DeviceKey, g.Reason)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why this device is authorised (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newAdminManagementRevokeCommand(_ Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "revoke <ed25519:device-key>",
		Short: "Return a device to the read floor",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, err := canonicalDeviceKey(args[0])
			if err != nil {
				return err
			}
			return withCatalog(cmd.Context(), *configPath, func(ctx context.Context, cat *catalog.Catalog) error {
				existed, err := cat.RevokeManagement(ctx, key)
				if err != nil {
					return err
				}
				if !existed {
					return fmt.Errorf("admin: device %s has no management grant", key)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "device %s is back on the read floor\n", key)
				return nil
			})
		},
	}
	return cmd
}

func newAdminManagementListCommand(_ Options, configPath *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the devices authorised for write",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCatalog(cmd.Context(), *configPath, func(ctx context.Context, cat *catalog.Catalog) error {
				grants, err := cat.ListManagementGrants(ctx)
				if err != nil {
					return err
				}
				return printManagementGrants(cmd.OutOrStdout(), grants, asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func printManagementGrants(w io.Writer, grants []catalog.ManagementGrant, asJSON bool) error {
	if asJSON {
		if grants == nil {
			grants = []catalog.ManagementGrant{}
		}
		return emitJSON(w, grants)
	}
	if len(grants) == 0 {
		fmt.Fprintln(w, "no device is authorised for write")
		return nil
	}
	for _, g := range grants {
		fmt.Fprintf(w, "%s  %s  %s\n", g.GrantedAt.UTC().Format(time.RFC3339), g.DeviceKey, g.Reason)
	}
	return nil
}
