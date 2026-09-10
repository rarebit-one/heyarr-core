package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

func newEnrichCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enrich",
		Short: "Enrich held music and book works with covers and canonical ids",
		Long: `Held albums and books are identified from their filenames at ingest — a title,
maybe a year, no cover. Enrichment fills that in from a keyed lookup (MusicBrainz
and the Cover Art Archive for music, Open Library for books): it writes the work's
canonical id, fetches its cover as an ordinary artwork asset the library already
serves, and — when the match is confident — corrects a noisy filename-derived
title and author (ADR-0087, ADR-0088). The sources are keyless; no credential is
needed.`,
	}
	cmd.AddCommand(newEnrichBackfillCommand(opts, configPath))
	cmd.AddCommand(newEnrichStatusCommand(opts, configPath))
	return cmd
}

func newEnrichBackfillCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags   clientFlags
		library string
		work    string
		author  string
		all     bool
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Enrich held music/book works now, ignoring the background cadence",
		Long: `Queue an enrich job for every held music/book work in scope that still lacks a
cover or a canonical id.

Enrichment otherwise runs on a background beat that paces itself with a backoff
schedule (ADR-0087). This is the one-time lever to enrich a scope NOW — after
wiring an enrich provider, or after ingesting a shelf of books.

A scope is required so "this one author" is never one forgotten flag from "every
book on the node":

  heyarr enrich backfill --work <work-id>
  heyarr enrich backfill --author "Reads"
  heyarr enrich backfill --library <library-id>
  heyarr enrich backfill --all

Only works missing a cover or an id are touched, and the enrich job is idempotent
— so a re-run is cheap and safe.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if library == "" && work == "" && author == "" && !all {
				return fmt.Errorf("choose a scope: --work, --author, --library, or --all")
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				body := map[string]any{}
				if library != "" {
					body["library_id"] = library
				}
				if work != "" {
					body["work_id"] = work
				}
				if author != "" {
					body["author"] = author
				}
				if all {
					body["all"] = true
				}
				var res struct {
					Candidates int `json:"candidates"`
					Enqueued   int `json:"enqueued"`
				}
				if err := c.Post(ctx, "/enrich/backfill", body, &res); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"queued %d enrich job(s) across %d under-enriched work(s)\n",
					res.Enqueued, res.Candidates)
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&library, "library", "", "only works in this library id")
	cmd.Flags().StringVar(&work, "work", "", "only this work id")
	cmd.Flags().StringVar(&author, "author", "", "only works filed under this author/artist")
	cmd.Flags().BoolVar(&all, "all", false, "every under-enriched music/book work on this node")
	return cmd
}

func newEnrichStatusCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show how many held music/book works still lack a cover or id",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				var res struct {
					UnderEnriched int `json:"under_enriched"`
					Total         int `json:"total"`
					Enriched      int `json:"enriched"`
				}
				if err := c.Get(ctx, "/enrich/status", nil, &res); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"%d of %d held music/book works enriched, %d still to do\n",
					res.Enriched, res.Total, res.UnderEnriched)
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}
