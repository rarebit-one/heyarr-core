// Package cli defines the cobra command tree for both roles and the client
// surface.
//
// This package is the composition root. Roles are constructed here and nowhere
// else, so that `heyarr all` and a three-process deployment share one wiring
// path — see ADR-0002.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/controller"
	"github.com/rarebit-one/heyarr-core/internal/peer"
	"github.com/rarebit-one/heyarr-core/internal/worker"
)

// Options are the process-level inputs to the command tree, injected so tests
// can drive it without touching the real stdio or signal handling.
type Options struct {
	Stdout io.Writer
	Stderr io.Writer
	// ShutdownGrace bounds how long roles may take to stop. Zero means
	// DefaultShutdownGrace.
	ShutdownGrace time.Duration
}

// NewRootCommand builds the full command tree.
func NewRootCommand(opts Options) *cobra.Command {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.ShutdownGrace == 0 {
		opts.ShutdownGrace = DefaultShutdownGrace
	}

	var configPath string

	root := &cobra.Command{
		Use:   "heyarr",
		Short: "Self-hosted content lifecycle, replication and consumption",
		Long: `Heyarr manages content across its full lifecycle — discovery, acquisition,
ingest, identification, storage, replication and consumption — while brokering
encrypted user state across trusted devices and peers.

One logical library, multiple complete sovereign peers.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(opts.Stdout)
	root.SetErr(opts.Stderr)
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "",
		"path to the configuration file (default: built-in defaults plus HEYARR_ environment)")

	root.AddCommand(
		newVersionCommand(opts),
		newConfigCommand(opts, &configPath),
		newTokenCommand(opts, &configPath),
		newFsckCommand(opts, &configPath),
		newBackupCommand(opts, &configPath),
		// The device commands. Not a client of the controller and not host
		// administration: they manage the key of the person at the keyboard,
		// in that person's own config directory (§40, ADR-0032). They take no
		// --config on purpose.
		newDeviceCommand(opts),
		// The identity commands sit beside the device commands and for the same
		// reason: they manage the key of the person at the keyboard — here the
		// user identity that vouches for device keys (§40, ADR-0048) — in that
		// person's own config directory, taking no --config (ADR-0032).
		newIdentityCommand(opts),
		// Renderer discovery sits here for the same reason: it is multicast
		// on the local segment and needs neither a controller nor a
		// credential. An operator standing in the living room can run it
		// before Heyarr is configured at all.
		newRenderersCommand(opts, &configPath),
		newGCCommand(opts, &configPath),
		newRecoverCommand(opts, &configPath),
		// The client commands. Everything below this line talks to a running
		// controller over /api/v1; everything above it is host administration
		// that has to work before a credential exists or when the controller
		// will not start.
		newLibraryCommand(opts, &configPath),
		newDesiredCommand(opts, &configPath),
		newQualityProfileCommand(opts, &configPath),
		newScanCommand(opts, &configPath),
		newWorksCommand(opts, &configPath),
		newAssetsCommand(opts, &configPath),
		newPlayCommand(opts, &configPath),
		newBlobsCommand(opts, &configPath),
		newJobsCommand(opts, &configPath),
		newPeersCommand(opts, &configPath),
		newEventsCommand(opts, &configPath),
		newSystemCommand(opts, &configPath),
		newRoleCommand("controller",
			"Own coordinated mutable state: catalog, policy, jobs, API", opts, &configPath, rolesController),
		newRoleCommand("worker",
			"Execute leased jobs", opts, &configPath, rolesWorker),
		newRoleCommand("peer",
			"Serve and replicate bytes", opts, &configPath, rolesPeer),
		newRoleCommand("all",
			"Run every role in one process (small deployments)", opts, &configPath,
			rolesController|rolesWorker|rolesPeer),
	)
	return root
}

// roleSet selects which roles a command starts. `all` is the union rather than a
// separate code path, so there is exactly one way a role gets constructed.
type roleSet uint8

const (
	rolesController roleSet = 1 << iota
	rolesWorker
	rolesPeer
)

func newVersionCommand(opts Options) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "heyarr %s (%s, built %s, %s)\n",
				info.Version, info.Commit, info.Date, info.GoVersion)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newConfigCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect configuration",
	}
	print := &cobra.Command{
		Use:   "print",
		Short: "Print the fully resolved configuration",
		Long: `Print configuration after defaults, the config file and HEYARR_ environment
have been layered and validated — which is what Heyarr will actually use, and
frequently not what any single source says.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(cfg)
		},
	}
	// --redacted is accepted now and does nothing, because nothing in the
	// configuration is secret yet: credentials are argon2id-hashed in the
	// database (ADR-0011), never in this file. It exists so the habit of
	// reaching for it is already correct when provider credentials arrive in
	// Milestone 3.
	var redacted bool
	print.Flags().BoolVar(&redacted, "redacted", false,
		"hide secret values (no-op today — configuration holds no secrets)")
	cmd.AddCommand(print)
	return cmd
}

func newRoleCommand(name, short string, opts Options, configPath *string, roles roleSet) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRoles(cmd.Context(), opts, *configPath, roles)
		},
	}
}

// runRoles loads configuration, builds the selected roles and supervises them.
// Every role is constructed the same way regardless of how many are running,
// which is what keeps `heyarr all` honest about ADR-0002.
func runRoles(ctx context.Context, opts Options, configPath string, roles roleSet) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}

	log := newLogger(cfg.Log, opts.Stderr)
	info := buildinfo.Get()
	log.Info("heyarr starting",
		"version", info.Version,
		"commit", info.Commit,
		"go", info.GoVersion,
		"peer", cfg.Peer.Name,
		"site", cfg.Peer.Site,
		"data_dir", cfg.DataDir,
		"roles", roleNames(roles))

	var running []Role
	if roles&rolesController != 0 {
		running = append(running, controller.New(cfg, log))
	}
	if roles&rolesWorker != 0 {
		running = append(running, worker.New(cfg, log))
	}
	if roles&rolesPeer != 0 {
		running = append(running, peer.New(cfg, log))
	}

	err = supervise(ctx, log, opts.ShutdownGrace, running...)
	log.Info("heyarr stopped")
	return err
}

func roleNames(roles roleSet) []string {
	var names []string
	if roles&rolesController != 0 {
		names = append(names, "controller")
	}
	if roles&rolesWorker != 0 {
		names = append(names, "worker")
	}
	if roles&rolesPeer != 0 {
		names = append(names, "peer")
	}
	return names
}

// Main is the process entry point. It wires signal handling to context
// cancellation so that SIGTERM from systemd produces the same clean shutdown as
// Ctrl-C from a terminal.
func Main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := NewRootCommand(Options{})
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "heyarr: %v\n", err)
		os.Exit(1)
	}
}
