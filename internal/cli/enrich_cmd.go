package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

func newEnrichCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enrich",
		Short: "Enrich held works with artwork, descriptions and canonical ids",
		Long: `Held films, series, albums and books are identified from their filenames at
ingest — a title, maybe a year, no artwork. Enrichment fills that in from a keyed
lookup (TMDB for films and series, MusicBrainz and the Cover Art Archive for
music, Open Library for books): it writes the work's canonical ids and overview,
and fetches its poster (and, for a film or series, its backdrop) as ordinary
artwork assets the node stores and serves to every client. A confident book or
album match also corrects a noisy filename-derived title and author (ADR-0087,
ADR-0088); a film or series is decorated, never renamed (ADR-0122). MusicBrainz
and Open Library are keyless; TMDB needs its API read-access token.`,
	}
	cmd.AddCommand(newEnrichBackfillCommand(opts, configPath))
	cmd.AddCommand(newEnrichStatusCommand(opts, configPath))
	return cmd
}

func newEnrichBackfillCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags       clientFlags
		library     string
		work        string
		author      string
		contentType string
		all         bool
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Enrich held works now, ignoring the background cadence",
		Long: `Queue an enrich job for every held work in scope that is still under-enriched:
an album or book without a cover or canonical id, a film or series without a
poster, a backdrop or its TMDB id.

Enrichment otherwise runs on a background beat that paces itself with a backoff
schedule (ADR-0087). This is the one-time lever to enrich a scope NOW — after
wiring an enrich provider, or after ingesting a shelf of books.

A scope is required so "this one author" is never one forgotten flag from "every
book on the node":

  heyarr enrich backfill --work <work-id>
  heyarr enrich backfill --author "Reads"
  heyarr enrich backfill --library <library-id>
  heyarr enrich backfill --all
  heyarr enrich backfill --all --content-type movie

--content-type narrows any scope to one kind (movie, series, music, book) — the
films, after wiring TMDB, without re-asking Open Library about every book.

Only under-enriched works are touched, the job fetches only the images a work is
missing, and it dedupes per work — so a re-run is cheap and safe.`,
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
				if contentType != "" {
					body["content_type"] = contentType
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
	cmd.Flags().StringVar(&contentType, "content-type", "", "only works of this type: movie, series, music or book")
	cmd.Flags().BoolVar(&all, "all", false, "every under-enriched work on this node")
	return cmd
}

func newEnrichStatusCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show how many held works still lack their artwork or id",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				type count struct {
					UnderEnriched int `json:"under_enriched"`
					Total         int `json:"total"`
				}
				var res struct {
					UnderEnriched int              `json:"under_enriched"`
					Total         int              `json:"total"`
					Enriched      int              `json:"enriched"`
					ByContentType map[string]count `json:"by_content_type"`
				}
				if err := c.Get(ctx, "/enrich/status", nil, &res); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"%d of %d held works enriched, %d still to do\n",
					res.Enriched, res.Total, res.UnderEnriched)
				types := make([]string, 0, len(res.ByContentType))
				for ct := range res.ByContentType {
					types = append(types, ct)
				}
				sort.Strings(types)
				for _, ct := range types {
					n := res.ByContentType[ct]
					fmt.Fprintf(cmd.OutOrStdout(), "  %-7s %d of %d enriched\n",
						ct, n.Total-n.UnderEnriched, n.Total)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}
