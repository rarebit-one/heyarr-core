package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

func newProbeCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "probe",
		Short: "Media probe operations",
		Long: `A probe is what ffprobe said about a blob's bytes (§29): its container, its
streams, and for a picture its size and colour. It is taken once, at ingest,
and read by playback planning and copy selection.`,
	}
	cmd.AddCommand(newProbeBackfillCommand(opts, configPath))
	return cmd
}

func newProbeBackfillCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags   clientFlags
		library string
		work    string
		all     bool
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Re-probe video whose probe cannot say whether it is HDR",
		Long: `Queue a probe for every managed video in scope whose stored probe records
neither a transfer characteristic nor a Dolby Vision record on its picture,
including videos never probed at all.

A probe runs at INGEST and the bytes never change, so a rescan enqueues
nothing. Probes taken before colour was recorded (#885, #886) read as SDR,
which steers an SDR screen onto an HDR copy and re-encodes it without a
tone-map. This is the one-time lever to re-read them.

A scope is required so "this one film" is never one forgotten flag from "every
video on the node":

  heyarr probe backfill --work <work-id>
  heyarr probe backfill --library <library-id>
  heyarr probe backfill --all

A probe that already answers is left alone, so a re-run is cheap. SDR video
whose container declares no colour at all is re-probed each run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if library == "" && work == "" && !all {
				return fmt.Errorf("choose a scope: --work, --library, or --all")
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				body := map[string]any{}
				if library != "" {
					body["library_id"] = library
				}
				if work != "" {
					body["work_id"] = work
				}
				if all {
					body["all"] = true
				}
				var res struct {
					Candidates int `json:"candidates"`
					Enqueued   int `json:"enqueued"`
				}
				if err := c.Post(ctx, "/probes/backfill", body, &res); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"queued %d probe job(s) across %d blob(s) whose probe does not record colour\n",
					res.Enqueued, res.Candidates)
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&library, "library", "", "only videos in this library id")
	cmd.Flags().StringVar(&work, "work", "", "only videos in this work id")
	cmd.Flags().BoolVar(&all, "all", false, "every video on this node whose probe does not record colour")
	return cmd
}
