package cli

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

func newJobsCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Inspect and retry durable work",
		Long: `Every unit of work Heyarr does is a durable, leased, idempotent job (§75,
ADR-0008).

The state worth understanding is the difference between failed and dead:
"failed" is a spent attempt that the queue will retry with backoff, and "dead"
is terminal — attempts are exhausted and nothing further will happen until an
operator retries it.`,
	}
	cmd.AddCommand(
		newJobsListCommand(opts, configPath),
		newJobsShowCommand(opts, configPath),
		newJobsRetryCommand(opts, configPath),
	)
	return cmd
}

func newJobsListCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags   clientFlags
		list    listFlags
		state   string
		jobType string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List jobs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				q := url.Values{}
				if state != "" {
					q.Set("state", state)
				}
				if jobType != "" {
					q.Set("type", jobType)
				}
				opts := list.options()
				opts.Query = q
				jobs, err := client.List[client.Job](ctx, c, "/jobs", opts)
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), jobs)
				}
				t := newTable("ID", "TYPE", "STATE", "ATTEMPTS", "UPDATED", "LAST ERROR")
				for _, j := range jobs {
					t.add(j.ID, j.Type, string(j.State),
						strconv.Itoa(j.Attempts)+"/"+strconv.Itoa(j.MaxAttempts),
						stamp(j.UpdatedAt), dash(j.LastError))
				}
				return t.render(cmd.OutOrStdout(), "no jobs")
			})
		},
	}
	flags.register(cmd)
	list.register(cmd)
	cmd.Flags().StringVar(&state, "state", "", "pending, leased, succeeded, failed or dead")
	cmd.Flags().StringVar(&jobType, "type", "", "only jobs of this type, e.g. scan_library")
	return cmd
}

func newJobsShowCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				var j client.Job
				if err := c.Get(ctx, "/jobs/"+url.PathEscape(args[0]), nil, &j); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), j)
				}
				printJob(cmd, j)
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

func printJob(cmd *cobra.Command, j client.Job) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "  id           %s\n", j.ID)
	fmt.Fprintf(out, "  type         %s\n", j.Type)
	fmt.Fprintf(out, "  state        %s\n", j.State)
	fmt.Fprintf(out, "  attempts     %d of %d\n", j.Attempts, j.MaxAttempts)
	fmt.Fprintf(out, "  run after    %s\n", stamp(j.RunAfter))
	fmt.Fprintf(out, "  lease owner  %s\n", dash(j.LeaseOwner))
	fmt.Fprintf(out, "  created      %s\n", stamp(j.CreatedAt))
	fmt.Fprintf(out, "  updated      %s\n", stamp(j.UpdatedAt))
	fmt.Fprintf(out, "  finished     %s\n", stampPtr(j.FinishedAt))
	fmt.Fprintf(out, "  payload      %s\n", string(j.Payload))
	if j.LastError != nil && *j.LastError != "" {
		fmt.Fprintf(out, "  last error   %s\n", *j.LastError)
	}
	if j.State == client.JobDead {
		fmt.Fprintf(out, "\nThis job is dead: its attempts are exhausted and nothing will run it again.\n"+
			"Fix the cause, then `heyarr jobs retry %s`.\n", j.ID)
	}
}

func newJobsRetryCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "retry <id>",
		Short: "Put a finished job back on the queue",
		Long: `Retry a succeeded, failed or dead job.

This is an operator action: it says that whatever was wrong has been fixed. A
job that is still pending or leased cannot be retried, because it has not
stopped.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				var j client.Job
				if err := c.Post(ctx, "/jobs/"+url.PathEscape(args[0])+"/retry", nil, &j); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), j)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "requeued %s (%s), now %s\n", j.ID, j.Type, j.State)
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

func newPeersCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peers",
		Short: "Inspect the peers of this instance",
		Long: `One logical library, multiple complete sovereign peers (§2).

Milestone 1 has exactly one peer and it is this node (ADR-0010). The command
exists now so that the shape of the answer does not change when there are
several.`,
	}
	cmd.AddCommand(newPeersListCommand(opts, configPath))
	return cmd
}

func newPeersListCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags clientFlags
		list  listFlags
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List peers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				opts := list.options()
				peers, err := client.List[client.Peer](ctx, c, "/peers", opts)
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), peers)
				}
				t := newTable("ID", "NAME", "SITE", "MODE", "SELF", "ENDPOINT")
				for _, p := range peers {
					t.add(p.ID, p.Name, p.Site, p.Mode, strconv.FormatBool(p.IsSelf), dash(p.Endpoint))
				}
				return t.render(cmd.OutOrStdout(), "no peers")
			})
		},
	}
	flags.register(cmd)
	list.register(cmd)
	return cmd
}
