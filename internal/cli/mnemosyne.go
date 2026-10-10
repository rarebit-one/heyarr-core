package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/controller"
	"github.com/rarebit-one/heyarr-core/internal/worker"
)

// NewMnemosyneRootCommand builds the cobra command tree for the mnemosyne
// binary. It is deliberately smaller than NewRootCommand: Mnemosyne is a
// single-service binary that runs only the controller (personal profile), so
// the worker, peer, library, scanner, playback and provider subcommands are
// absent. The token and admin commands are kept — operators still need them to
// manage credentials on a personal node (ADR-0107).
func NewMnemosyneRootCommand(opts Options) *cobra.Command {
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
		Use:   "mnemosyne",
		Short: "Personal media service — encrypted vaults, drive CRDT, placement pins",
		Long: `Mnemosyne manages encrypted personal media across trusted devices and peers.

It is the personal-plane half of the heyarr-core repository (ADR-0107): a
second binary that mounts only the personal-state plane, vault blob upload,
placement pins, blob content serving, auth/tokens/device enrolment/recovery
and encrypted-state replication. Libraries, scanner, ingest, search,
identification, render and compat adapters are not mounted.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(opts.Stdout)
	root.SetErr(opts.Stderr)
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "",
		"path to the configuration file (default: $"+config.MnemosyneConfigPathEnv+", else "+
			config.MnemosyneSystemConfigPath+" if present, else built-in defaults plus MNEMOSYNE_ environment)")

	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		configPath = config.ResolveMnemosynePath(configPath)
		return nil
	}

	root.AddCommand(
		newMnemosyneVersionCommand(opts),
		newMnemosyneConfigCommand(opts, &configPath),
		newTokenCommand(opts, &configPath),
		newAdminCommand(opts, &configPath),
		newFsckCommand(opts, &configPath),
		newBackupCommand(opts, &configPath),
		// Device/identity commands are kept: Mnemosyne still enrolls devices.
		newDeviceCommand(opts, &configPath),
		newIdentityCommand(opts),
		newPairCommand(opts),
		newGCCommand(opts, &configPath),
		newRecoverCommand(opts, &configPath),
		// The vault client: the main reason to run Mnemosyne.
		newVaultCommand(opts, &configPath),
		// The space (personal-state) client.
		newSpaceCommand(opts, &configPath),
		// Service recipients (ADR-0104).
		newRecipientCommand(opts, &configPath),
		newPeersCommand(opts, &configPath),
		newEventsCommand(opts, &configPath),
		newSystemCommand(opts, &configPath),
		// Mnemosyne runs only the controller role: personal profile (ADR-0107).
		newMnemosyneRoleCommand(opts, &configPath),
		// The worker role: GC, peer convergence and blob transfer (Phase 1b).
		newMnemosyneWorkerCommand(opts, &configPath),
	)
	return root
}

// newMnemosyneRoleCommand returns the single "serve" subcommand that runs the
// controller in personal profile. It is named "serve" rather than "controller"
// or "all" to set the right expectation: there is only one role.
func newMnemosyneRoleCommand(opts Options, configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the Mnemosyne personal-media service",
		Long: `Start the Mnemosyne controller in personal profile (ADR-0107).

This is the one role Mnemosyne offers. It mounts the personal-state plane,
vault blob upload, placement pins and blob content serving. All media beats
and surfaces are excluded.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMnemosyne(cmd.Context(), opts, *configPath)
		},
	}
}

// runMnemosyne loads Mnemosyne configuration and runs the controller in
// personal profile. It is the Mnemosyne analogue of runRoles.
func runMnemosyne(ctx context.Context, opts Options, configPath string) error {
	cfg, err := config.LoadMnemosyne(configPath)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}

	log := newLogger(cfg.Log, opts.Stderr)
	info := buildinfo.Get()
	log.Info("mnemosyne starting",
		"version", info.Version,
		"commit", info.Commit,
		"go", info.GoVersion,
		"peer", cfg.Peer.Name,
		"site", cfg.Peer.Site,
		"data_dir", cfg.DataDir,
		"profile", cfg.Profile)

	err = supervise(ctx, log, opts.ShutdownGrace, controller.New(cfg, log))
	log.Info("mnemosyne stopped")
	return err
}

func newMnemosyneVersionCommand(_ Options) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), info)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "mnemosyne %s (%s, built %s, %s)\n",
				info.Version, info.Commit, info.Date, info.GoVersion)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newMnemosyneConfigCommand(_ Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect configuration",
	}
	print := &cobra.Command{
		Use:   "print",
		Short: "Print the fully resolved configuration",
		Long: `Print configuration after defaults, the config file and MNEMOSYNE_ environment
have been layered and validated — which is what Mnemosyne will actually use.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.LoadMnemosyne(*configPath)
			if err != nil {
				return err
			}
			return emitJSON(cmd.OutOrStdout(), cfg)
		},
	}
	var redacted bool
	print.Flags().BoolVar(&redacted, "redacted", false,
		"hide secret values (no-op today — configuration holds no secrets)")
	cmd.AddCommand(print)
	return cmd
}

// newMnemosyneWorkerCommand returns the "worker" subcommand that runs the
// PersonalWorker: GC, peer convergence and blob transfer for the personal
// profile. It is kept separate from "serve" because the two roles have
// different resource requirements and are independently runnable (ADR-0002):
// a small device may run only "serve", while a node with more disk runs both.
func newMnemosyneWorkerCommand(opts Options, configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "worker",
		Short: "Run the Mnemosyne personal worker (GC, peer convergence, blob transfer)",
		Long: `Start the Mnemosyne worker role for the personal profile (ADR-0107, Phase 1b).

The worker runs three categories of background job:

  gc_blobs          — reclaims orphaned vault bytes (apply=true, every six hours).
  reconcile_peer    — drives vault-blob convergence toward the placement-pin desired
                      set on every enrolled peer (every five minutes, at startup).
  replicate_blob    — moves vault blobs to the peers that reconcile_peer named as
                      destinations (mTLS, bounded concurrency).
  chunk_blob        — produces chunk manifests for resumable large-blob transfers
                      (lazy, §16, ADR-0035).

The controller (mnemosyne serve) does not start a GC beat or a convergence beat.
A Mnemosyne deployment without this process running will never collect garbage or
converge vault blobs to remote peers.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMnemosyneWorker(cmd.Context(), opts, *configPath)
		},
	}
}

// runMnemosyneWorker loads Mnemosyne configuration and runs the personal worker
// role. It is the Phase 1b analogue of runMnemosyne.
func runMnemosyneWorker(ctx context.Context, opts Options, configPath string) error {
	cfg, err := config.LoadMnemosyne(configPath)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}

	log := newLogger(cfg.Log, opts.Stderr)
	info := buildinfo.Get()
	log.Info("mnemosyne worker starting",
		"version", info.Version,
		"commit", info.Commit,
		"go", info.GoVersion,
		"peer", cfg.Peer.Name,
		"site", cfg.Peer.Site,
		"data_dir", cfg.DataDir,
		"profile", cfg.Profile)

	err = supervise(ctx, log, opts.ShutdownGrace, worker.NewPersonalWorker(cfg, log))
	log.Info("mnemosyne worker stopped")
	return err
}

// MnemosyneMain is the process entry point for the mnemosyne binary. It wires
// signal handling to context cancellation so that SIGTERM from systemd
// produces the same clean shutdown as Ctrl-C from a terminal.
func MnemosyneMain() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := NewMnemosyneRootCommand(Options{})
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "mnemosyne: %v\n", err)
		os.Exit(exitCode(err))
	}
}
