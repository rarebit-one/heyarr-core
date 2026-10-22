package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// newVaultAdminCommand builds `heyarr vault`, which on heyarr holds only host
// administration for the vault data a media node kept from before the vault
// moved to the personal profile (ADR-0107, #754). The vault client itself is
// Mnemosyne's (`mnemosyne vault`).
func newVaultAdminCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "vault",
		Short: "Retire the legacy vault data a media node still holds (#823)",
		Long: `Host administration for the vault blobs a media node kept from before the
vault moved to the personal profile (ADR-0107). The vault client is Mnemosyne's
("mnemosyne vault"); heyarr serves media.`,
		// Runnable, so NoArgs is enforced: a client verb such as `heyarr vault ls`
		// is an unknown command here rather than a silent help page.
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newVaultPurgeCommand(opts, configPath))
	return cmd
}

func newVaultPurgeCommand(_ Options, configPath *string) *cobra.Command {
	var (
		yes    bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Release every legacy vault placement pin so GC can reclaim it (#823)",
		Long: `Remove every placement pin on this media node, recording release intent for
each blob nothing else wants (#779, ADR-0114), so a later
"heyarr gc --honour-releases --apply" can reclaim its last copy.

On a media node every placement pin is legacy vault: the media profile mounts no
vault routes and writes none. Since no device can reach those pins any more,
nothing else would ever release them, and GC's last-copy rule would keep the
bytes forever.

Library media is never released. A pin on a blob that an asset or a scanned
library file reaches is removed, and the bytes stay. A blob also taken from a
sibling is released only once that sibling withdraws it.

Like gc, this works on this node's database directly, as host administration,
and changes nothing unless you pass --yes. Run it as the service's user.

After a real run:

  heyarr gc --honour-releases --apply    starts each released blob's grace window
  (wait out the window; the default is a week)
  heyarr gc --honour-releases --apply    reclaims them, with the basis "released"

It refuses on the personal profile, where pins are devices' live retention
decisions.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadFromContext(cmd.Context(), *configPath)
			if err != nil {
				return err
			}
			if cfg.Profile == config.ProfilePersonal {
				return fmt.Errorf("vault purge: refused on the personal profile, where placement " +
					"pins are devices' live retention decisions; it retires a media node's legacy " +
					"vault pins (#823)")
			}
			return withIntegrity(cmd.Context(), *configPath, func(
				ctx context.Context, _ integrity.Options, _ *cas.FS, cat *catalog.Catalog, _ string,
			) error {
				var (
					p   catalog.PinPurge
					err error
				)
				if yes {
					p, err = cat.PurgePlacementPins(ctx)
				} else {
					p, err = cat.PlacementPinPurgePreview(ctx)
				}
				if err != nil {
					return err
				}
				return printPinPurge(cmd.OutOrStdout(), p, !yes, asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "remove the pins; without it nothing is changed")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func printPinPurge(w io.Writer, p catalog.PinPurge, dryRun, asJSON bool) error {
	if asJSON {
		return emitJSON(w, struct {
			DryRun bool `json:"dry_run"`
			catalog.PinPurge
		}{dryRun, p})
	}
	if dryRun {
		fmt.Fprintf(w, "vault purge (dry run — nothing was changed)\n\n")
		fmt.Fprintf(w, "  placement pins      %d\n", p.Pins)
		fmt.Fprintf(w, "  blobs               %d\n", p.Blobs)
		fmt.Fprintf(w, "  bytes               %d\n", p.Bytes)
		fmt.Fprintf(w, "  would be released   %d\n", p.Releasable)
		fmt.Fprintf(w, "  still wanted        %d\n", p.StillWanted)
	} else {
		fmt.Fprintf(w, "vault purge (applied)\n\n")
		fmt.Fprintf(w, "  pins removed        %d\n", p.Pins)
		fmt.Fprintf(w, "  blobs               %d\n", p.Blobs)
		fmt.Fprintf(w, "  bytes               %d\n", p.Bytes)
		fmt.Fprintf(w, "  released            %d\n", p.Releasable)
		fmt.Fprintf(w, "  still wanted        %d\n", p.StillWanted)
	}
	if p.StillWanted > 0 {
		fmt.Fprintf(w, "\nStill wanted: a taken placement, an asset or a scanned library file reaches\n"+
			"them, so they are unpinned but not released. Library media is never released.\n")
	}
	switch {
	case dryRun && p.Pins > 0:
		fmt.Fprintf(w, "\nNothing was removed. Re-run with --yes to remove the pins and record the releases.\n")
	case !dryRun && p.Releasable > 0:
		fmt.Fprintf(w, "\nNext:\n"+
			"  heyarr gc --honour-releases --apply    starts the grace window (%s by default)\n"+
			"  then, after the window:\n"+
			"  heyarr gc --honour-releases --apply    reclaims them, with the basis \"released\"\n",
			integrity.DefaultGrace)
	case p.Pins == 0:
		fmt.Fprintf(w, "\nNo placement pins: there is nothing to purge.\n")
	}
	return nil
}
