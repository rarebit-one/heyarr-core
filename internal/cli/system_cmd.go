package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// ErrDrift is returned when `heyarr system drift` finds an instance that is not
// running what it was expected to be running.
//
// It exists so the command exits non-zero, for the same reason fsck's ErrDamage
// does: this is a check meant to be wired into a cron job or a deploy pipeline,
// and a check that reports a problem and then exits 0 is worse than no check —
// its output stops being read and its success starts being trusted.
var ErrDrift = errors.New("drift: this instance is not running what was expected")

func newSystemCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Report what a running instance is and how far behind it has drifted",
		Long: `Ask a running Heyarr what it is: its build, its schema version, its peer
identity, and whether the things it depends on are working.

` + "`system drift`" + ` compares that against what it was expected to be, and
answers with a distance rather than a yes or no.`,
	}
	cmd.AddCommand(
		newSystemInfoCommand(opts, configPath),
		newSystemDriftCommand(opts, configPath),
	)
	return cmd
}

func newSystemInfoCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "info",
		Short: "Print what the instance is running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				info, err := c.System(ctx, client.Expectation{})
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), info)
				}
				return printSystemInfo(cmd.OutOrStdout(), info)
			})
		},
	}
	flags.register(cmd)
	return cmd
}

// driftFlags are the expectation `system drift` compares the instance against.
type driftFlags struct {
	version string
	commit  string
	schema  int64
}

func newSystemDriftCommand(_ Options, configPath *string) *cobra.Command {
	var (
		flags  clientFlags
		expect driftFlags
	)
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Report how far a running instance has drifted from what was expected",
		Long: `Compare a running instance against what it should be, and say HOW FAR behind
it is rather than whether it differs.

Two comparisons are made and they are reported separately, because they drift
separately. A binary that is current with its migrations unapplied is not a
mild case of being behind — it is a build running against a schema it was never
tested on — and one combined "up to date" flag would let either failure hide
the other.

  build   the version and commit the instance reports, against the expectation.
          Ordering comes from the semantic version when both sides carry one;
          otherwise the commits decide whether the builds are the same build,
          and the answer is "mismatch" rather than a guessed distance.
  schema  the migration version applied to its database, against the highest
          migration a binary embeds. Reported as a count of migrations, which
          is the number that says whether an upgrade is routine or wants a
          backup taken first.

By default the expectation is THIS binary: its own version and commit, and the
migrations it was compiled with. That answers the question somebody at a
terminal actually has — "is that host running what I have here?" — and it needs
no network access to anything but the instance itself. Override any part of it
with --expected-version, --expected-commit and --expected-schema to check
against a release you are not currently holding.

Nothing here reports "current" when it could not compare. An expectation it
cannot order is reported as "unknown", because a check that has quietly stopped
comparing looks exactly like a fleet that never drifts — which is how a
deployment ran two milestones behind with everything green (#132).

Exits non-zero when either half has drifted, so this can be a cron job.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			want, err := expect.expectation()
			if err != nil {
				return err
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				info, err := c.System(ctx, want)
				if err != nil {
					return err
				}
				if flags.asJSON {
					err = emitJSON(cmd.OutOrStdout(), info.Drift)
				} else {
					err = printDrift(cmd.OutOrStdout(), info.Drift)
				}
				if err != nil {
					return err
				}
				if info.Drift.Drifted() {
					return fmt.Errorf("%w: %s", ErrDrift, driftSummary(info.Drift))
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&expect.version, "expected-version", "",
		"the version the instance should be running (default: this binary's)")
	cmd.Flags().StringVar(&expect.commit, "expected-commit", "",
		"the commit the instance should be built from (default: this binary's)")
	cmd.Flags().Int64Var(&expect.schema, "expected-schema", 0,
		"the schema version its database should be at (default: the highest migration this binary embeds)")
	return cmd
}

// expectation resolves the flags into what to compare against, defaulting to
// this binary.
//
// The default is deliberate rather than convenient. A drift check whose
// expectation had to be supplied every time is one nobody runs; a drift check
// that defaults to "the thing you are holding" answers the question with no
// arguments at all, on a laptop with no network beyond the instance.
func (f driftFlags) expectation() (client.Expectation, error) {
	self := buildinfo.Get()
	want := client.Expectation{
		Build:  client.BuildIdentity{Version: f.version, Commit: f.commit},
		Schema: f.schema,
	}
	// All or nothing per FIELD, not per build: somebody who passes only
	// --expected-commit means "this commit, whatever it calls itself", and
	// filling the other half in from this binary would compare against a build
	// that never existed.
	if f.version == "" && f.commit == "" {
		want.Build = client.BuildIdentity{Version: self.Version, Commit: self.Commit}
	}
	if f.schema < 0 {
		return client.Expectation{}, errors.New("--expected-schema cannot be negative")
	}
	if f.schema == 0 {
		known, err := sqlite.KnownSchemaVersion()
		if err != nil {
			return client.Expectation{}, err
		}
		want.Schema = known
	}
	return want, nil
}

// driftSummary is the one-line reason the command exited non-zero.
func driftSummary(r client.DriftReport) string {
	switch {
	case r.Build.Drifted() && r.Schema.Drifted():
		return fmt.Sprintf("the build is %s and the schema is %s", r.Build.Status, r.Schema.Status)
	case r.Build.Drifted():
		return "the build is " + string(r.Build.Status)
	default:
		return "the schema is " + string(r.Schema.Status)
	}
}

func printDrift(w io.Writer, r client.DriftReport) error {
	t := newTable("CHECK", "STATUS", "DISTANCE", "EXPECTED", "ACTUAL")
	t.add("build", string(r.Build.Status), buildDistance(r.Build),
		identityCell(r.Build.Expected), identityCell(r.Build.Actual))
	t.add("schema", string(r.Schema.Status), schemaDistance(r.Schema),
		fmt.Sprint(r.Schema.Expected), fmt.Sprint(r.Schema.Applied))
	if err := t.render(w, "nothing to compare"); err != nil {
		return err
	}
	// The detail is where an "unknown" says why it could not compare, which is
	// the line somebody actually needs when this reports nothing useful.
	for _, d := range []string{r.Build.Detail, r.Schema.Detail} {
		if d == "" {
			continue
		}
		if _, err := fmt.Fprintf(w, "\n  %s\n", d); err != nil {
			return err
		}
	}
	return nil
}

// buildDistance renders how far behind, in words a person can act on.
func buildDistance(b client.BuildDrift) string {
	switch {
	case b.MajorBehind > 0:
		return plural(b.MajorBehind, "major version")
	case b.MinorBehind > 0:
		return plural(b.MinorBehind, "minor version")
	case b.PatchBehind > 0:
		return plural(b.PatchBehind, "patch")
	default:
		return "-"
	}
}

func schemaDistance(s client.SchemaDrift) string {
	switch {
	case s.MigrationsBehind > 0:
		return plural(int(s.MigrationsBehind), "migration") + " behind"
	case s.MigrationsAhead > 0:
		return plural(int(s.MigrationsAhead), "migration") + " ahead"
	default:
		return "-"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

func identityCell(i client.BuildIdentity) string {
	switch {
	case i.Version != "" && i.Commit != "":
		return i.Version + " (" + i.Commit + ")"
	case i.Version != "":
		return i.Version
	case i.Commit != "":
		return i.Commit
	default:
		return "-"
	}
}

func printSystemInfo(w io.Writer, info *client.SystemInfo) error {
	fmt.Fprintf(w, "heyarr %s (%s)\n\n", info.Build.Version, info.Build.Commit)
	fmt.Fprintf(w, "  peer            %s @ %s\n", info.Peer.Name, info.Peer.Site)
	fmt.Fprintf(w, "  schema version  %d\n", info.SchemaVersion)
	fmt.Fprintf(w, "  database        %s (%s)\n", info.Database.Path, okWord(info.Database.OK))
	fmt.Fprintf(w, "  cas             %s (%s)\n", info.CAS.Path, okWord(info.CAS.OK))
	fmt.Fprintf(w, "  events head     %d (%s)\n", info.Events.Head, okWord(info.Events.OK))
	fmt.Fprintf(w, "  auth            %s\n", enabledWord(info.AuthEnabled))
	if len(info.Media) > 0 {
		fmt.Fprintf(w, "\n")
		t := newTable("TOOL", "AVAILABLE", "VERSION", "DETAIL")
		for _, m := range info.Media {
			detail := m.Detail
			if detail == "" {
				detail = "-"
			}
			version := m.Version
			if version == "" {
				version = "-"
			}
			t.add(m.Name, okWord(m.Available), version, detail)
		}
		if err := t.render(w, "no external tools resolved"); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "\nschema drift    %s (%s)\n", info.Drift.Schema.Status, schemaDistance(info.Drift.Schema))
	return nil
}

func okWord(ok bool) string {
	if ok {
		return "ok"
	}
	return "not ok"
}

func enabledWord(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}
