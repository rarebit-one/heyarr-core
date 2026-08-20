package cli

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

func newWorksCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "works",
		Short: "Browse the catalog",
		Long: `A work is a semantic unit of content — a film, a book, a season (§11).

Listings follow pagination cursors to the end of the collection rather than
returning the first page, so ` + "`heyarr works list`" + ` on a library of 40 000 works
returns 40 000 works.`,
	}
	cmd.AddCommand(newWorksListCommand(opts, configPath), newWorksShowCommand(opts, configPath))
	return cmd
}

func newWorksListCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags       clientFlags
		list        listFlags
		contentType string
		library     string
		search      string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List works",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				q := url.Values{}
				if contentType != "" {
					q.Set("content_type", contentType)
				}
				if library != "" {
					lib, err := resolveLibrary(ctx, c, library)
					if err != nil {
						return err
					}
					q.Set("library_id", lib.ID)
				}
				if search != "" {
					q.Set("q", search)
				}
				opts := list.options()
				opts.Query = q
				works, err := client.List[client.Work](ctx, c, "/works", opts)
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), works)
				}
				t := newTable("ID", "TYPE", "YEAR", "TITLE")
				for _, w := range works {
					year := "-"
					if w.Year != nil {
						year = strconv.FormatInt(*w.Year, 10)
					}
					t.add(w.ID, w.ContentType, year, w.Title)
				}
				return t.render(cmd.OutOrStdout(), "no works — scan a library with `heyarr scan <library>`")
			})
		},
	}
	flags.register(cmd)
	list.register(cmd)
	cmd.Flags().StringVar(&contentType, "content-type", "", "only works of this content type")
	cmd.Flags().StringVar(&library, "library", "", "only works with an asset in this library (id or name)")
	cmd.Flags().StringVar(&search, "q", "", "only works whose sort title contains this")
	return cmd
}

func newWorksShowCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one work",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				var w client.Work
				if err := c.Get(ctx, "/works/"+url.PathEscape(args[0]), nil, &w); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), w)
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "  id            %s\n", w.ID)
				fmt.Fprintf(out, "  title         %s\n", w.Title)
				fmt.Fprintf(out, "  sort title    %s\n", w.SortTitle)
				fmt.Fprintf(out, "  content type  %s\n", w.ContentType)
				fmt.Fprintf(out, "  work key      %s\n", w.WorkKey)
				if w.Year != nil {
					fmt.Fprintf(out, "  year          %d\n", *w.Year)
				}
				fmt.Fprintf(out, "  created       %s\n", stamp(w.CreatedAt))
				fmt.Fprintf(out, "  updated       %s\n", stamp(w.UpdatedAt))
				if len(w.Attributes) > 0 && string(w.Attributes) != "{}" {
					fmt.Fprintf(out, "  attributes    %s\n", string(w.Attributes))
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

func newAssetsCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "assets",
		Short: "Browse the files behind the catalog",
		Long: `An asset is one file belonging to an edition (§14).

A managed asset has a blob; a linked asset has a path and no blob at all
(ADR-0020), which is why the blob column can legitimately be empty.`,
	}
	cmd.AddCommand(newAssetsListCommand(opts, configPath))
	return cmd
}

func newAssetsListCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags       clientFlags
		list        listFlags
		library     string
		contentType string
		state       string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List assets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				q := url.Values{}
				if library != "" {
					lib, err := resolveLibrary(ctx, c, library)
					if err != nil {
						return err
					}
					q.Set("library_id", lib.ID)
				}
				if contentType != "" {
					q.Set("content_type", contentType)
				}
				if state != "" {
					q.Set("state", state)
				}
				opts := list.options()
				opts.Query = q
				assets, err := client.List[client.Asset](ctx, c, "/assets", opts)
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), assets)
				}
				t := newTable("ID", "CLASS", "STATE", "BLOB", "PATH")
				for _, a := range assets {
					assetState := "present"
					if a.MissingSince != nil {
						assetState = "missing"
					}
					t.add(a.ID, a.SourceClass, assetState, dash(a.BlobHash), dash(a.SourcePath))
				}
				return t.render(cmd.OutOrStdout(), "no assets")
			})
		},
	}
	flags.register(cmd)
	list.register(cmd)
	cmd.Flags().StringVar(&library, "library", "", "only assets in this library (id or name)")
	cmd.Flags().StringVar(&contentType, "content-type", "", "only assets whose work is of this content type")
	cmd.Flags().StringVar(&state, "state", "", "present or missing")
	return cmd
}
