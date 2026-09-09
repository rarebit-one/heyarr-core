package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

func newSubtitlesCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "subtitles",
		Short: "Subtitle operations",
		Long: `Subtitles are ordinary role='subtitle' assets — an external .srt beside a
video, or one lifted out of the container (ADR-0084). The caption path serves
whichever exists.`,
	}
	cmd.AddCommand(newSubtitlesBackfillCommand(opts, configPath))
	cmd.AddCommand(newSubtitlesRequestCommand(opts, configPath))
	return cmd
}

func newSubtitlesRequestCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags    clientFlags
		library  string
		work     string
		all      bool
		language string
	)
	cmd := &cobra.Command{
		Use:   "want",
		Short: "Request subtitles for held content that has none",
		Long: `Create a subtitle want for every managed video in scope that holds no subtitle
in the requested language. The fetch driver then acquires each from a subtitle
provider (ADR-0085).

This is for a subtitle that exists NOWHERE — neither shipped beside the video nor
embedded in its container (subtitles backfill covers those). A scope is required
so "this one show" is never one forgotten flag from "every video on the node":

  heyarr subtitles want --work <work-id> --lang en
  heyarr subtitles want --library <library-id> --lang en
  heyarr subtitles want --all --lang en

Only videos with no subtitle in the language get a want, and an existing want is
left alone — so a re-run is cheap and safe.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if library == "" && work == "" && !all {
				return fmt.Errorf("choose a scope: --work, --library, or --all")
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				body := map[string]any{"language": language}
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
					Created    int `json:"created"`
				}
				if err := c.Post(ctx, "/subtitles/want", body, &res); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"created %d subtitle want(s) across %d video(s) missing %s subtitles\n",
					res.Created, res.Candidates, language)
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&library, "library", "", "only videos in this library id")
	cmd.Flags().StringVar(&work, "work", "", "only videos in this work id")
	cmd.Flags().BoolVar(&all, "all", false, "every video on this node missing the subtitle")
	cmd.Flags().StringVar(&language, "lang", "en", "subtitle language as an ISO-639-1 code")
	return cmd
}

func newSubtitlesBackfillCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags   clientFlags
		library string
		work    string
		all     bool
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Extract embedded subtitles from already-ingested video",
		Long: `Queue an embedded-subtitle extraction for every managed video in scope whose
Edition has no subtitle yet.

Extraction otherwise runs at INGEST (ADR-0084), so a video ingested before that
existed — or before this node had ffmpeg — has embedded tracks that were never
lifted out, and a rescan will not fix it (the bytes are unchanged, so the ingest
deduplicates and enqueues nothing). This is the one-time lever to reprocess that
existing content.

A scope is required so "this one show" is never one forgotten flag from "every
video on the node":

  heyarr subtitles backfill --work <work-id>
  heyarr subtitles backfill --library <library-id>
  heyarr subtitles backfill --all

Only videos with no subtitle are touched, and the extraction is a no-op on a
video with no embedded TEXT tracks — so a re-run is cheap and safe.`,
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
				if err := c.Post(ctx, "/subtitles/backfill", body, &res); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), res)
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"queued %d extraction job(s) across %d video(s) missing subtitles\n",
					res.Enqueued, res.Candidates)
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&library, "library", "", "only videos in this library id")
	cmd.Flags().StringVar(&work, "work", "", "only videos in this work id")
	cmd.Flags().BoolVar(&all, "all", false, "every video on this node missing subtitles")
	return cmd
}
