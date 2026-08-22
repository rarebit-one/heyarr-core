package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/peer/endpoint"
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
		Short: "Inspect and manage the peers of this instance",
		Long: `One logical library, multiple complete sovereign peers (§2).

Membership is the only trust root in the inter-peer path (ADR-0012): a peer's
public key is pinned by the record ` + "`peers add`" + ` creates, and revocation is
` + "`peers remove`" + ` deleting it. There is no CA, no join token and no discovery.

A peer is its public key, not its address. Enrol it with the key the other site
prints here; move its endpoint later by registering the same key again.`,
	}
	cmd.AddCommand(newPeersListCommand(opts, configPath))
	cmd.AddCommand(newPeersAddCommand(opts, configPath))
	cmd.AddCommand(newPeersRemoveCommand(opts, configPath))
	cmd.AddCommand(newPeersShowCommand(opts, configPath))
	cmd.AddCommand(newPeersPingCommand(opts, configPath))
	cmd.AddCommand(newPeersAttachCommand(opts, configPath))
	cmd.AddCommand(newPeersReportCommand(opts, configPath))
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
				// The public key is a column rather than a detail view: an
				// operator enrolling this node at the other site needs a value
				// to copy, and telling them to read it out of SQLite is not an
				// answer (ADR-0012). It is the PUBLIC half — the private key is
				// a 0600 file the CLI never opens.
				//
				// HEALTH and LAST SEEN are two columns rather than one for the
				// reason PlacementVerdict.Missing gives (M4-10): "unreachable"
				// on its own is a status nobody can act on. Seeing that the
				// peer was last heard from forty seconds ago and seeing that it
				// was last heard from on Tuesday call for very different
				// afternoons.
				t := newTable("ID", "NAME", "SITE", "MODE", "SELF", "HEALTH", "LAST SEEN", "ENDPOINT", "PUBLIC KEY")
				for _, p := range peers {
					t.add(p.ID, p.Name, p.Site, p.Mode, strconv.FormatBool(p.IsSelf),
						p.Health, stampPtr(p.LastSeenAt), dash(p.Endpoint), dash(p.PublicKey))
				}
				return t.render(cmd.OutOrStdout(), "no peers")
			})
		},
	}
	flags.register(cmd)
	list.register(cmd)
	return cmd
}

// peerRow renders one peer the same way in `add`, `show` and `remove`, so that
// the value an operator copies out of one is the value they paste into the
// other.
func peerRow(out io.Writer, p client.Peer) {
	t := newTable("ID", "NAME", "SITE", "MODE", "SELF", "HEALTH", "LAST SEEN", "ENDPOINT", "PUBLIC KEY")
	t.add(p.ID, p.Name, p.Site, p.Mode, strconv.FormatBool(p.IsSelf),
		p.Health, stampPtr(p.LastSeenAt), dash(p.Endpoint), dash(p.PublicKey))
	_ = t.render(out, "no peer")
}

func newPeersAddCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags        clientFlags
		name         string
		site         string
		mode         string
		peerEndpoint string
		publicKey    string
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Enrol another peer by its public key",
		Long: `Register another node as a member of this fabric (§26, ADR-0012).

Enrolment is operator-mediated and explicit. There is no discovery, no join
token and no trust on first use: you read the other node's public key out of
` + "`heyarr peers list`" + ` at that site, and register it here. The operator at
the other site does the same in the other direction. Two nodes, two commands.

A peer is registered BY ITS PUBLIC KEY. --endpoint is where to reach it and may
change freely: run this again with the same --name and --public-key and a new
--endpoint, and the peer keeps its identity, its id and its enrolment date.
--public-key is required, and there is no form of this command without it —
registering a hostname and learning the key afterwards is trust on first use.

The endpoint is checked HERE rather than when something first dials it. Give it
as ` + "`https://host:port`" + `, or as a bare ` + "`host:port`" + `, which is read as https:
the inter-peer path is mutually authenticated TLS (ADR-0012), so there is one
scheme to guess and http is refused rather than upgraded. A ` + "`unix:///path`" + `
socket is accepted for a peer on this host. Anything else is refused before a
record exists, because registration is idempotent on the key: a typo would
otherwise replace a working endpoint and leave the peer looking healthy in
` + "`peers list`" + ` while being unreachable.

Membership is the only trust root in the inter-peer path, and revocation is
` + "`heyarr peers remove`" + `. It is consulted on every request, so a removed
peer loses access on the connection it is already holding open.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Checked here, before anything is sent, so the refusal names the
			// flag that carried the value (#169). The API checks it too — it
			// has to, since it is reachable without this CLI — but an operator
			// who typed --endpoint should be told about --endpoint.
			//
			// Only when the flag was given: a peer may be enrolled by its key
			// before anyone knows where it will live, and an omitted flag is
			// not an empty value.
			if cmd.Flags().Changed("endpoint") {
				normalised, err := endpoint.Normalise(peerEndpoint)
				if err != nil {
					return fmt.Errorf("--endpoint: %w", err)
				}
				peerEndpoint = normalised
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				var p client.Peer
				body := map[string]string{
					"name": name, "site": site, "mode": mode,
					"endpoint": peerEndpoint, "public_key": publicKey,
				}
				if err := c.Post(ctx, "/peers", body, &p); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), p)
				}
				peerRow(cmd.OutOrStdout(), p)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "the peer's name, unique within this instance")
	cmd.Flags().StringVar(&site, "site", "", "the peer's failure domain (§35)")
	cmd.Flags().StringVar(&mode, "mode", "full", "full, partial, cache, archive or compute (§9)")
	cmd.Flags().StringVar(&peerEndpoint, "endpoint", "",
		"where to reach the peer, as "+endpoint.Example+", a bare host:port or unix:///path; not its identity")
	cmd.Flags().StringVar(&publicKey, "public-key", "",
		"the peer's Ed25519 public key as ed25519:<64 hex characters> — who it is (required)")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("public-key")
	flags.register(cmd)
	return cmd
}

func newPeersRemoveCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "remove <name-or-id>",
		Short: "Revoke a peer's membership",
		Long: `Remove a peer's membership record, which is what revocation is (ADR-0012).

There is no revocation list and no certificate to expire: the record IS the
trust, so deleting it withdraws the trust. Membership is checked on every
request, so the removed peer stops being able to read bytes immediately — on
the connection it already has open, not at its next reconnect.

The peer's replica rows go with it. A peer this instance will not talk to is
not a peer whose copy counts towards placement.

This node cannot remove itself.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				var p client.Peer
				if err := c.DeleteInto(ctx, "/peers/"+url.PathEscape(args[0]), &p); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), p)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "revoked the membership of %s (%s)\n", p.Name, p.ID)
				peerRow(cmd.OutOrStdout(), p)
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

func newPeersShowCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "show <name-or-id>",
		Short: "Show one peer, by name or id",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				var p client.Peer
				if err := c.Get(ctx, "/peers/"+url.PathEscape(args[0]), nil, &p); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), p)
				}
				peerRow(cmd.OutOrStdout(), p)
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}
