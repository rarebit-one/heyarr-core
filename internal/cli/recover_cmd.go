package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/peer/backupsync"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	recover "github.com/rarebit-one/heyarr-core/internal/peer/recover"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

type recoverFlags struct {
	fromEndpoint string
	fromKey      string
	identityKey  string
	generation   int64
	confirm      string
	asJSON       bool
}

// recoverJSON is the --json shape of a recovery, for a dry run and for a
// completed restore alike.
type recoverJSON struct {
	Applied bool            `json:"applied"`
	Plan    recover.Plan    `json:"plan"`
	Message string          `json:"message"`
	Inputs  []recover.Input `json:"inputs,omitempty"`
}

func newRecoverCommand(_ Options, configPath *string) *cobra.Command {
	var f recoverFlags
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Rebuild this peer's control plane from a surviving peer (§51, §82, M7-04)",
		Long: `Rebuild THIS peer's control plane and identity from a backup a surviving
peer holds.

This does not "recover the system" — under the peer-repo model each peer is
authoritative for its own site, so what is rebuilt is one peer's control plane.
Two of the recovery inputs have no fetch path and are what this restores: the
control database, and this node's Ed25519 identity. The content store, the
encrypted personal state and the catalog snapshot re-fill themselves from the
fabric once the control plane is back — this command does NOT copy the CAS.

It needs this node's own identity key (kept aside from the lost data
directory), and the surviving peer's endpoint and public key (as ` + "`heyarr peers`" + `
shows them). The backup was signed by this node, so its own key is what
verifies it — a recovery trusts a signature over its identity, not the peer that
serves the file.

By default this is a DRY RUN: it fetches and verifies the backup and reports
what a restore would do, touching nothing. To actually restore, pass
--confirm with this node's data directory, matched exactly — an unrecoverable
command that accepts a partial match is one that runs on a typo, and this one
overwrites a data directory. It refuses outright if the data directory is still
live.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRecover(cmd.Context(), *configPath, cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.fromEndpoint, "from-endpoint", "", "the surviving peer's https peer-surface endpoint")
	cmd.Flags().StringVar(&f.fromKey, "from-key", "", "the surviving peer's ed25519 public key (as `heyarr peers` shows it)")
	cmd.Flags().StringVar(&f.identityKey, "identity-key", "", "this node's identity key file, kept aside from the lost data directory")
	cmd.Flags().Int64Var(&f.generation, "generation", 0, "which backup generation to restore (0 = the latest the peer holds)")
	cmd.Flags().StringVar(&f.confirm, "confirm", "", "the data directory to overwrite, matched exactly, to perform the restore (omit for a dry run)")
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func runRecover(ctx context.Context, configPath string, out io.Writer, f recoverFlags) error {
	if f.fromEndpoint == "" || f.fromKey == "" || f.identityKey == "" {
		return fmt.Errorf("--from-endpoint, --from-key and --identity-key are all required")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	// When --confirm is given, refuse a mismatched confirmation or a LIVE data
	// directory BEFORE fetching anything: fail fast, and so these refusals are
	// reachable without a surviving peer to fetch from. A dry run (no --confirm)
	// falls through to fetch-and-report.
	if f.confirm != "" {
		if f.confirm != cfg.DataDir {
			return fmt.Errorf("--confirm %q does not match the data directory %q exactly; "+
				"nothing was restored", f.confirm, cfg.DataDir)
		}
		if live, detail := dataDirIsLive(cfg.HTTP.UnixSocket); live {
			return fmt.Errorf("the data directory %q is live (%s); stop the node before restoring over it",
				cfg.DataDir, detail)
		}
	}

	seed, err := identity.ReadSeed(f.identityKey)
	if err != nil {
		return fmt.Errorf("reading the identity key: %w", err)
	}
	peerKey, err := parsePeerKey(f.fromKey)
	if err != nil {
		return err
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{
		PrivateKey: ed25519.NewKeyFromSeed(seed), PeerID: "recovering-node",
	})
	if err != nil {
		return fmt.Errorf("building this node's certificate material: %w", err)
	}

	target := backupsync.Target{
		Peer:     mtls.Peer{PeerID: "recovery-source", Name: "recovery-source", PublicKey: peerKey},
		Endpoint: f.fromEndpoint,
	}
	fetcher := backupsync.NewPusher(material, slog.New(slog.DiscardHandler))

	workDir, err := os.MkdirTemp("", "heyarr-recover-*")
	if err != nil {
		return fmt.Errorf("preparing a work directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	plan, err := recover.Fetch(ctx, recover.FetchOptions{
		Fetcher: fetcher, From: target, Generation: f.generation,
		IdentitySeed: seed, WorkDir: workDir, Now: time.Now,
	})
	if err != nil {
		return err
	}

	// A dry run stops here, having verified the backup and touched nothing.
	if f.confirm == "" {
		return printRecover(out, recoverJSON{
			Applied: false, Plan: plan, Inputs: plan.Inputs,
			Message: "dry run: nothing was restored; re-run with --confirm " + cfg.DataDir + " to restore",
		}, f.asJSON)
	}

	store, err := cas.OpenFS(cfg.CAS.Root)
	if err != nil {
		return fmt.Errorf("opening the content store: %w", err)
	}
	if err := recover.Apply(ctx, plan, seed, recover.ApplyOptions{
		DataDir: cfg.DataDir, Store: store, Now: time.Now,
	}); err != nil {
		return err
	}
	return printRecover(out, recoverJSON{
		Applied: true, Plan: plan, Inputs: plan.Inputs,
		Message: fmt.Sprintf("restored %s at generation %d into %s; the fabric refills the rest by convergence",
			plan.SourcePeerID, plan.Generation, cfg.DataDir),
	}, f.asJSON)
}

// parsePeerKey accepts an ed25519 public key as bare hex or with the "ed25519:"
// prefix `heyarr peers` prints.
func parsePeerKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "ed25519:")
	raw, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("--from-key is not valid hex: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("--from-key is %d bytes, an ed25519 public key is %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// dataDirIsLive reports whether a controller is listening on the data dir's
// socket. A successful dial means something is accepting; a refused dial (a
// stale socket, or none) means the directory is not live.
func dataDirIsLive(socket string) (bool, string) {
	if socket == "" {
		return false, ""
	}
	conn, err := net.DialTimeout("unix", socket, 2*time.Second)
	if err != nil {
		return false, ""
	}
	_ = conn.Close()
	return true, "a controller is answering on " + socket
}

func printRecover(out io.Writer, r recoverJSON, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	verb := "would restore"
	if r.Applied {
		verb = "restored"
	}
	if _, err := fmt.Fprintf(out, "%s %s at generation %d (taken %s ago, schema %d, %s)\n",
		verb, r.Plan.SourcePeerID, r.Plan.Generation, r.Plan.Age, r.Plan.SchemaVersion, signedWord(r.Plan.Signed)); err != nil {
		return err
	}
	for _, in := range r.Plan.Inputs {
		if _, err := fmt.Fprintf(out, "  %-26s %s (%s)\n", in.Name, in.Status, in.Detail); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(out, r.Message)
	return err
}

func signedWord(signed bool) string {
	if signed {
		return "signed"
	}
	return "unsigned"
}
