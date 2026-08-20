package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

func newLibraryCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "library",
		Short: "Manage libraries and their roots",
		Long: `A library is a named collection of roots — directories Heyarr scans (§10).

Libraries can also be declared in the configuration file, which the controller
reconciles at start. These commands are the runtime equivalent and write
through the API, so they work against a controller on another host.`,
	}
	cmd.AddCommand(
		newLibraryAddCommand(opts, configPath),
		newLibraryListCommand(opts, configPath),
	)
	return cmd
}

func newLibraryAddCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags       clientFlags
		contentType string
		roots       []string
		ingestMode  string
		disabled    bool
	)
	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a library, optionally with roots",
		Long: `Create a library and add its roots.

The roots are added after the library exists, so a run that fails part way
leaves a library with fewer roots rather than nothing — re-running with the
same name reports the conflict rather than silently making a second library.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				enabled := !disabled
				var lib client.Library
				err := c.Post(ctx, "/libraries", client.CreateLibraryRequest{
					Name:        args[0],
					ContentType: contentType,
					Enabled:     &enabled,
				}, &lib)
				if err != nil {
					return err
				}
				for _, root := range roots {
					var created client.LibraryRoot
					if err := c.Post(ctx, "/libraries/"+lib.ID+"/roots", client.CreateRootRequest{
						Path:       root,
						IngestMode: ingestMode,
					}, &created); err != nil {
						return fmt.Errorf("the library %s was created but the root %s was not added: %w",
							lib.Name, root, err)
					}
					lib.Roots = append(lib.Roots, created)
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), lib)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "created library %s (%s, %s) with %d root(s)\n",
					lib.Name, lib.ID, lib.ContentType, len(lib.Roots))
				for _, r := range lib.Roots {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s\n", r.IngestMode, r.Path)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&contentType, "content-type", "", "what the library holds: movie, show, book, music (required)")
	cmd.Flags().StringArrayVar(&roots, "root", nil, "a directory to scan; repeat for several")
	cmd.Flags().StringVar(&ingestMode, "ingest-mode", "reflink",
		"how bytes are materialised for these roots: reflink, hardlink, copy or link (ADR-0014, ADR-0020)")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "create the library but do not scan it yet")
	_ = cmd.MarkFlagRequired("content-type")
	return cmd
}

func newLibraryListCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags       clientFlags
		list        listFlags
		contentType string
		search      string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List libraries and their roots",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				q := url.Values{}
				if contentType != "" {
					q.Set("content_type", contentType)
				}
				if search != "" {
					q.Set("q", search)
				}
				opts := list.options()
				opts.Query = q
				libs, err := client.List[client.Library](ctx, c, "/libraries", opts)
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), libs)
				}
				t := newTable("ID", "NAME", "TYPE", "ENABLED", "ROOTS")
				for _, l := range libs {
					t.add(l.ID, l.Name, l.ContentType, strconv.FormatBool(l.Enabled), rootSummary(l))
				}
				return t.render(cmd.OutOrStdout(), "no libraries — create one with `heyarr library add <name> --content-type movie`")
			})
		},
	}
	flags.register(cmd)
	list.register(cmd)
	cmd.Flags().StringVar(&contentType, "content-type", "", "only libraries of this content type")
	cmd.Flags().StringVar(&search, "q", "", "only libraries whose name contains this")
	return cmd
}

func rootSummary(l client.Library) string {
	switch len(l.Roots) {
	case 0:
		return "-"
	case 1:
		return l.Roots[0].Path
	default:
		return fmt.Sprintf("%s (+%d more)", l.Roots[0].Path, len(l.Roots)-1)
	}
}

// scanOutput is the --json shape of `heyarr scan`.
//
// The same fields are emitted whether or not --wait was given, because a script
// that has to branch on which keys exist is a script that will get it wrong.
// `outcome` is "queued" without --wait and "succeeded" or "dead" with it.
type scanOutput struct {
	LibraryID string        `json:"library_id"`
	Jobs      []client.Job  `json:"jobs"`
	Waited    bool          `json:"waited"`
	Succeeded int           `json:"succeeded"`
	Dead      int           `json:"dead"`
	Outcome   string        `json:"outcome"`
	Gaps      []*client.Gap `json:"gaps,omitempty"`
}

func newScanCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags   clientFlags
		wait    bool
		timeout time.Duration
		poll    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "scan <library>",
		Short: "Scan a library's roots, optionally waiting for the scan to finish",
		Long: `Enqueue a scan of every enabled root of a library.

The library may be named by id or by name. One job is enqueued per root, so a
library with three roots produces three jobs — and --wait waits for all of
them, because exiting 0 once the first finished would report success for a scan
that is a third done.

--wait exits 0 only when every job succeeded. If any job reaches dead it exits
non-zero and prints the last error, because a CLI that exits 0 when the work
failed is worse than no CLI: it will be put in a script, its output will stop
being read, and its silence will be trusted.

It waits by subscribing to the event stream before reading the jobs' state,
never the other way round: a job that finishes in between would otherwise be
waited on forever. A job that is already finished when the wait starts returns
immediately.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				lib, err := resolveLibrary(ctx, c, args[0])
				if err != nil {
					return err
				}
				var queued client.ScanResponse
				if err := c.Post(ctx, "/libraries/"+lib.ID+"/scan", nil, &queued); err != nil {
					return err
				}

				out := scanOutput{
					LibraryID: queued.LibraryID,
					Jobs:      queued.Jobs,
					Outcome:   "queued",
				}
				if !wait {
					return finishScan(cmd, flags.asJSON, out, false)
				}

				ids := make([]string, 0, len(queued.Jobs))
				for _, j := range queued.Jobs {
					ids = append(ids, j.ID)
				}

				waitCtx := ctx
				if timeout > 0 {
					var cancel context.CancelFunc
					waitCtx, cancel = context.WithTimeout(ctx, timeout)
					defer cancel()
				}

				result, err := c.WaitForJobs(waitCtx, ids, client.WaitOptions{
					PollInterval: poll,
					Progress:     scanProgressPrinter(cmd, flags.asJSON, &out),
				})
				out.Jobs = result.Jobs
				out.Waited = true
				out.Succeeded = result.Succeeded
				out.Dead = result.Dead
				out.Outcome = "succeeded"
				if result.Failed() {
					out.Outcome = "dead"
				}
				if err != nil {
					// The wait itself failed — a timeout, a broken stream. The
					// jobs may still be running, so say what is known rather
					// than claiming an outcome.
					out.Outcome = "unknown"
					_ = finishScan(cmd, flags.asJSON, out, true)
					return err
				}
				return finishScan(cmd, flags.asJSON, out, true)
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for every enqueued scan job to finish")
	cmd.Flags().DurationVar(&timeout, "wait-timeout", 0,
		"give up waiting after this long (default: wait indefinitely)")
	cmd.Flags().DurationVar(&poll, "poll-interval", client.DefaultPollInterval,
		"how often --wait re-reads job state as a backstop to the event stream; 0 waits on events alone")
	return cmd
}

// ErrScanFailed is returned when a scan job reached dead. It exists so the
// process exits non-zero, which is the entire contract of --wait.
var ErrScanFailed = errors.New("scan: a scan job died")

// finishScan renders the outcome and decides the exit status.
func finishScan(cmd *cobra.Command, asJSON bool, out scanOutput, waited bool) error {
	w := cmd.OutOrStdout()
	if asJSON {
		if err := emitJSON(w, out); err != nil {
			return err
		}
	} else {
		switch {
		case !waited:
			fmt.Fprintf(w, "queued %d scan job(s) for library %s\n", len(out.Jobs), out.LibraryID)
			for _, j := range out.Jobs {
				fmt.Fprintf(w, "  %s  %s  %s\n", j.ID, j.Type, j.State)
			}
			fmt.Fprintf(w, "\nWatch them with `heyarr jobs list --type scan_library`.\n")
		default:
			fmt.Fprintf(w, "scan of library %s finished: %d succeeded, %d dead\n",
				out.LibraryID, out.Succeeded, out.Dead)
			for _, j := range out.Jobs {
				line := fmt.Sprintf("  %s  %s  attempt %d/%d", j.ID, j.State, j.Attempts, j.MaxAttempts)
				if j.LastError != nil && *j.LastError != "" {
					line += "  " + *j.LastError
				}
				fmt.Fprintln(w, line)
			}
		}
	}
	if out.Dead > 0 {
		return fmt.Errorf("%w: %d of %d scan job(s) for library %s reached dead — see `heyarr jobs show <id>`",
			ErrScanFailed, out.Dead, len(out.Jobs), out.LibraryID)
	}
	return nil
}

// scanProgressPrinter reports progress to stderr while waiting.
//
// stderr rather than stdout, so that `heyarr scan lib --wait --json | jq` is
// still a single JSON document. A gap notice is printed loudly: it means this
// connection missed events, and the wait went back to the job rows to find out
// what it missed rather than assuming all was well.
func scanProgressPrinter(cmd *cobra.Command, asJSON bool, out *scanOutput) func(client.Progress) {
	return func(p client.Progress) {
		if p.Gap != nil {
			out.Gaps = append(out.Gaps, p.Gap)
			fmt.Fprintf(cmd.ErrOrStderr(),
				"the event stream dropped %d event(s) — re-reading job state from the API (resume after %d)\n",
				p.Gap.Dropped, p.Gap.ResumeAfter)
		}
		if asJSON {
			return
		}
		if progress, ok := client.DecodeScanProgress(p.Event); ok {
			fmt.Fprintf(cmd.ErrOrStderr(), "scan %s: %d seen, %d enqueued, %d skipped, %d missing\n",
				progress.State, progress.FilesSeen, progress.FilesEnqueued,
				progress.FilesSkipped, progress.FilesMissing)
			return
		}
		if p.Event != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "%s\n", p.Event.Type)
		}
	}
}

// resolveLibrary accepts an id or a name.
//
// Names are what a person types and ids are what a script has, and refusing
// either would make one of them do a lookup by hand. An ambiguous name is an
// error rather than a guess: scanning the wrong library is hours of disk IO.
func resolveLibrary(ctx context.Context, c *client.Client, ref string) (client.Library, error) {
	var lib client.Library
	err := c.Get(ctx, "/libraries/"+url.PathEscape(ref), nil, &lib)
	if err == nil {
		return lib, nil
	}
	if !client.IsNotFound(err) {
		return client.Library{}, err
	}

	q := url.Values{}
	q.Set("q", ref)
	matches, listErr := client.List[client.Library](ctx, c, "/libraries", client.ListOptions{Query: q})
	if listErr != nil {
		return client.Library{}, listErr
	}
	var exact []client.Library
	for _, l := range matches {
		if l.Name == ref {
			exact = append(exact, l)
		}
	}
	switch len(exact) {
	case 1:
		return exact[0], nil
	case 0:
		return client.Library{}, fmt.Errorf("no library called %q — `heyarr library list` shows what there is", ref)
	default:
		return client.Library{}, fmt.Errorf("%q matches %d libraries; name it by id", ref, len(exact))
	}
}
