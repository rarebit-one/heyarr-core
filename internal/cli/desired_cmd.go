package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

func newDesiredCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "desired",
		Aliases: []string{"want"},
		Short:   "Say what should exist, whether or not it does yet",
		Long: `A desired item is "this content should exist under these conditions" (§55).

The point is that you can want something Heyarr has never seen. Name it by what
it is — a content type, a title and a year — and the Work is created for you,
using the same normalisation the scanner uses, so wanting a film and later
scanning it converge on one Work rather than producing two.

A quality profile is required: "this should exist", with no statement of what
would count as existing, cannot be evaluated.`,
	}
	cmd.AddCommand(
		newDesiredAddCommand(opts, configPath),
		newDesiredListCommand(opts, configPath),
		newDesiredSetCommand(opts, configPath),
		newDesiredRemoveCommand(opts, configPath),
	)
	return cmd
}

func newDesiredAddCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags       clientFlags
		contentType string
		year        int
		workID      string
		editionID   string
		profile     string
		reason      string
		noMonitor   bool
	)
	cmd := &cobra.Command{
		Use:   "add <title>",
		Short: "Want something",
		Long: `Want content, creating the Work if Heyarr has never seen it.

  heyarr desired add "The Conversation" --content-type movie --year 1974 \
      --quality-profile living-room

Pass --work-id instead of a title to want something already in the catalog.

Two wants over the same content with DIFFERENT profiles are legal and are the
point — the living-room copy and the phone-sized copy are two wants. The same
content under the same profile twice is one want written twice, and is refused.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && workID == "" {
				return fmt.Errorf("name what you want: a title, or --work-id for something already catalogued")
			}
			if len(args) == 1 && workID != "" {
				return fmt.Errorf("name the work with either a title or --work-id, not both")
			}
			if len(args) == 1 && contentType == "" {
				return fmt.Errorf("--content-type is required when wanting by title " +
					"(movie, series, music, book)")
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				req := client.CreateDesiredRequest{
					WorkID:         workID,
					EditionID:      editionID,
					QualityProfile: profile,
					Reason:         reason,
				}
				if editionID != "" {
					req.Scope = "edition"
				}
				if len(args) == 1 {
					req.Work = &client.WorkDescriptor{
						ContentType: contentType, Title: args[0], Year: year,
					}
				}
				if noMonitor {
					monitor := false
					req.Monitor = &monitor
				}
				var item client.DesiredItem
				if err := c.Post(ctx, "/desired", req, &item); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), item)
				}
				kind, target := "work", item.WorkID
				if item.Scope == "edition" {
					kind, target = "edition", item.EditionID
				}
				fmt.Fprintf(cmd.OutOrStdout(), "wanted %s %s under profile %s (monitor: %v)\n",
					kind, target, item.QualityProfileID, item.Monitor)
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&contentType, "content-type", "",
		"what this is: movie, series, music, book (required when wanting by title)")
	cmd.Flags().IntVar(&year, "year", 0, "the year, when it is part of the identity")
	cmd.Flags().StringVar(&workID, "work-id", "", "want something already in the catalog")
	cmd.Flags().StringVar(&editionID, "edition-id", "",
		"want one edition — a season, a particular release — rather than the whole work")
	cmd.Flags().StringVar(&profile, "quality-profile", "",
		"the profile this want is measured against, by name (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "a note to your future self")
	cmd.Flags().BoolVar(&noMonitor, "no-monitor", false,
		"get it once and stop — do not keep looking for something better")
	_ = cmd.MarkFlagRequired("quality-profile")
	return cmd
}

func newDesiredListCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags     clientFlags
		list      listFlags
		scope     string
		workID    string
		monitored string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List what should exist",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				q := url.Values{}
				if scope != "" {
					q.Set("scope", scope)
				}
				if workID != "" {
					q.Set("work_id", workID)
				}
				if monitored != "" {
					q.Set("monitor", monitored)
				}
				opts := list.options()
				opts.Query = q
				items, err := client.List[client.DesiredItem](ctx, c, "/desired", opts)
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), items)
				}
				t := newTable("ID", "SCOPE", "TARGET", "PROFILE", "MONITOR", "STATE")
				for _, d := range items {
					target := d.WorkID
					if d.Scope == "edition" {
						target = d.EditionID
					}
					// §64's name rather than the reason: "where has this got
					// to" is the question someone runs this command to answer,
					// and the reason is already in --json for whoever wants it.
					state := "—"
					if d.Acquisition != nil {
						state = d.Acquisition.State
					}
					t.add(d.ID, d.Scope, target, d.QualityProfileID,
						strconv.FormatBool(d.Monitor), state)
				}
				return t.render(cmd.OutOrStdout(),
					"nothing is wanted — try `heyarr desired add \"A Title\" --content-type movie --quality-profile living-room`")
			})
		},
	}
	flags.register(cmd)
	list.register(cmd)
	cmd.Flags().StringVar(&scope, "scope", "", "only work-scoped or edition-scoped wants")
	cmd.Flags().StringVar(&workID, "work-id", "", "only wants for this work")
	cmd.Flags().StringVar(&monitored, "monitor", "",
		"only monitored (true) or unmonitored (false) wants")
	return cmd
}

func newDesiredSetCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags     clientFlags
		profile   string
		reason    string
		monitor   bool
		noMonitor bool
	)
	cmd := &cobra.Command{
		Use:   "set <id>",
		Short: "Change the conditions, the monitoring or the note",
		Long: `Change what a want is measured against, whether it keeps looking, or its note.

The target cannot be changed: repointing a want at different content is not an
edit, it is a different want. Remove it and want the other thing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if monitor && noMonitor {
				return fmt.Errorf("--monitor and --no-monitor say opposite things")
			}
			req := client.UpdateDesiredRequest{}
			if profile != "" {
				req.QualityProfile = &profile
			}
			if cmd.Flags().Changed("reason") {
				req.Reason = &reason
			}
			switch {
			case monitor:
				on := true
				req.Monitor = &on
			case noMonitor:
				off := false
				req.Monitor = &off
			}
			if req.QualityProfile == nil && req.Reason == nil && req.Monitor == nil {
				return fmt.Errorf("nothing to change — pass --quality-profile, --reason, " +
					"--monitor or --no-monitor")
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				var item client.DesiredItem
				if err := c.Patch(ctx, "/desired/"+args[0], req, &item); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), item)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "updated %s (profile %s, monitor: %v)\n",
					item.ID, item.QualityProfileID, item.Monitor)
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&profile, "quality-profile", "", "measure this want against a different profile, by name")
	cmd.Flags().StringVar(&reason, "reason", "", "replace the note")
	cmd.Flags().BoolVar(&monitor, "monitor", false, "keep looking for something better")
	cmd.Flags().BoolVar(&noMonitor, "no-monitor", false, "stop looking once it is satisfied")
	return cmd
}

func newDesiredRemoveCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:     "rm <id>",
		Aliases: []string{"remove"},
		Short:   "Stop wanting something",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				if err := c.Delete(ctx, "/desired/"+args[0]); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), map[string]string{
						"id": args[0], "status": "removed",
					})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "no longer wanted: %s\n", args[0])
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

func newQualityProfileCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags clientFlags
		list  listFlags
	)
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List the quality profiles",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				opts := list.options()
				profiles, err := client.List[client.QualityProfile](ctx, c, "/quality-profiles", opts)
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), profiles)
				}
				t := newTable("ID", "NAME", "SEEDED", "TERMINAL", "DESCRIPTION")
				for _, p := range profiles {
					// "never" is the honest rendering of no terminal rules: it
					// means there is no condition under which this profile is
					// finished, which is a real thing to want. Showing "0"
					// would read as a count and invite the wrong conclusion.
					terminal := "never"
					if body := strings.TrimSpace(string(p.Terminal)); body != "" && body != "[]" && body != "null" {
						terminal = "yes"
					}
					t.add(p.ID, p.Name, strconv.FormatBool(p.Seeded), terminal, p.Description)
				}
				return t.render(cmd.OutOrStdout(), "no quality profiles")
			})
		},
	}
	flags.register(listCmd)
	list.register(listCmd)

	var (
		createFlags                           clientFlags
		description, accept, prefer, terminal string
	)
	createCmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Author a quality profile (§62)",
		Long: `Create a quality profile from its accept/prefer/terminal rules.

The three groups are three different KINDS of statement (§62), and each is
given as a JSON array of rules — the same shape the API takes:

  accept    a GATE  — fail it and a candidate is rejected outright
  prefer    a SCORE — a weighted preference; missing all of them is still acceptable
  terminal  a STOP  — the point at which the upgrade workflow stops looking

  heyarr quality-profile create living-room \
    --accept  '[{"attribute":"resolution","op":"gte","value":1080}]' \
    --prefer  '[{"attribute":"video_codec","op":"eq","value":"hevc","weight":20}]'

An omitted group is left empty; a profile with no terminal rules is never
"finished", which is legal — that is what the seeded "archival" profile is.
Authoring is deliberate: a name that already exists is reported as a conflict,
never silently replaced.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return createFlags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				req := client.CreateQualityProfileRequest{Name: args[0], Description: description}
				// Reject malformed JSON here so a typo is a local error that
				// names the flag, rather than a 400 the operator has to decode.
				for _, g := range []struct {
					name string
					raw  string
					dst  *json.RawMessage
				}{
					{"accept", accept, &req.Accept},
					{"prefer", prefer, &req.Prefer},
					{"terminal", terminal, &req.Terminal},
				} {
					if strings.TrimSpace(g.raw) == "" {
						continue
					}
					if !json.Valid([]byte(g.raw)) {
						return fmt.Errorf("--%s is not valid JSON", g.name)
					}
					*g.dst = json.RawMessage(g.raw)
				}
				var out client.QualityProfile
				if err := c.Post(ctx, "/quality-profiles", req, &out); err != nil {
					return err
				}
				if createFlags.asJSON {
					return emitJSON(cmd.OutOrStdout(), out)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "created quality profile %s (%s)\n", out.Name, out.ID)
				return nil
			})
		},
	}
	createFlags.register(createCmd)
	createCmd.Flags().StringVar(&description, "description", "", "a human description of what the profile is for")
	createCmd.Flags().StringVar(&accept, "accept", "", "gate rules as a JSON array — a candidate failing any is rejected")
	createCmd.Flags().StringVar(&prefer, "prefer", "", "scoring rules as a JSON array — weighted preferences, never gates")
	createCmd.Flags().StringVar(&terminal, "terminal", "", "stop rules as a JSON array — when the upgrade workflow stops looking")

	var (
		setFlags                                                   clientFlags
		setName, setDescription, setAccept, setPrefer, setTerminal string
	)
	setCmd := &cobra.Command{
		Use:   "set <name|id>",
		Short: "Change an existing quality profile's rules or description (§62)",
		Long: `Change a quality profile in place, named the way "list" shows it — by name or id.

Only the parts you pass change. A rule group you OMIT is left as it was; a group
you pass as an explicit empty array is CLEARED. Those are different intentions —
"leave the terminal rules alone" and "remove them" are not the same edit:

  # prefer English on everyday, leaving its accept and terminal untouched
  heyarr quality-profile set everyday \
    --prefer '[{"attribute":"language","op":"eq","value":"en","weight":50}]'

  # clear a profile's terminal rules (make it never "finished")
  heyarr quality-profile set archival --terminal '[]'

Rename with --name; --description replaces the description. The new rules take
effect the next time each want is evaluated — its next search, upgrade scan or
satisfaction read — not as an immediate mass re-judge.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return setFlags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				// Resolve name-or-id to an id the way an operator names it: the
				// profile they see in "list", by either column. Profiles are
				// few, so a list-and-match is cheaper to reason about than a
				// second by-name lookup route.
				profiles, err := client.List[client.QualityProfile](ctx, c, "/quality-profiles", client.ListOptions{})
				if err != nil {
					return err
				}
				id := ""
				for _, p := range profiles {
					if p.ID == args[0] || p.Name == args[0] {
						id = p.ID
						break
					}
				}
				if id == "" {
					return fmt.Errorf("no quality profile named or with id %q", args[0])
				}

				req := client.UpdateQualityProfileRequest{Name: setName, Description: setDescription}
				// A rule group changes only when its flag was given — a flag
				// left off means "leave it", which is why Changed is checked
				// rather than the value being empty. Malformed JSON is a local
				// error naming the flag, not a 400 to decode.
				for _, g := range []struct {
					name string
					raw  string
					dst  **json.RawMessage
				}{
					{"accept", setAccept, &req.Accept},
					{"prefer", setPrefer, &req.Prefer},
					{"terminal", setTerminal, &req.Terminal},
				} {
					if !cmd.Flags().Changed(g.name) {
						continue
					}
					if !json.Valid([]byte(g.raw)) {
						return fmt.Errorf("--%s is not valid JSON (pass a JSON array, or [] to clear)", g.name)
					}
					rm := json.RawMessage(g.raw)
					*g.dst = &rm
				}

				var out client.QualityProfile
				if err := c.Put(ctx, "/quality-profiles/"+id, req, &out); err != nil {
					return err
				}
				if setFlags.asJSON {
					return emitJSON(cmd.OutOrStdout(), out)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "updated quality profile %s (%s)\n", out.Name, out.ID)
				return nil
			})
		},
	}
	setFlags.register(setCmd)
	setCmd.Flags().StringVar(&setName, "name", "", "rename the profile")
	setCmd.Flags().StringVar(&setDescription, "description", "", "replace the description")
	setCmd.Flags().StringVar(&setAccept, "accept", "", "replace gate rules (JSON array; [] clears)")
	setCmd.Flags().StringVar(&setPrefer, "prefer", "", "replace scoring rules (JSON array; [] clears)")
	setCmd.Flags().StringVar(&setTerminal, "terminal", "", "replace stop rules (JSON array; [] clears)")

	cmd := &cobra.Command{
		Use:     "quality-profile",
		Aliases: []string{"profile"},
		Short:   "Author and inspect the quality profiles a want is measured against",
		Long: `A quality profile says three different KINDS of thing (§62):

  accept    a GATE  — fail it and a candidate is rejected outright
  prefer    a SCORE — never a gate; a candidate meeting none is still acceptable
  terminal  a STOP  — the point at which the upgrade workflow stops looking

A profile with no terminal rules is never finished, which is legal and is what
the seeded "archival" profile is.

Use "create" to author one, "set" to change one, and "list" to read them.`,
	}
	cmd.AddCommand(listCmd)
	cmd.AddCommand(createCmd)
	cmd.AddCommand(setCmd)
	return cmd
}
