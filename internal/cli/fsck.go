package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
	"github.com/rarebit-one/heyarr-core/internal/peer/durability"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/peer/mtls"
	"github.com/rarebit-one/heyarr-core/internal/peer/repairsource"
	"github.com/rarebit-one/heyarr-core/internal/peer/transfer"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/integrity"
)

// withIntegrity opens the database and the content store and hands both to fn.
//
// Like `heyarr token`, these commands talk to the database directly rather than
// to the API, and for the same reason: they are host administration, not a role
// (ADR-0002). fsck in particular has to work when the controller will not start,
// which is precisely when someone reaches for it.
//
// It migrates, because a schema one version behind would otherwise present as
// "no such column: unreferenced_since" — a puzzle rather than an error.
func withIntegrity(ctx context.Context, configPath string,
	fn func(context.Context, integrity.Options, *cas.FS, *catalog.Catalog, string) error,
) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	db, err := sqlite.Open(ctx, sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		return fmt.Errorf("opening the controller database: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := sqlite.Migrate(ctx, db); err != nil {
		return fmt.Errorf("migrating the controller database: %w", err)
	}

	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		return err
	}
	cat, err := catalog.New(catalog.Options{
		DB: db, Events: eventLog, PeerName: cfg.Peer.Name, PeerSite: cfg.Peer.Site,
	})
	if err != nil {
		return err
	}
	store, err := cas.OpenFS(cfg.CAS.Root)
	if err != nil {
		return fmt.Errorf("opening the content store: %w", err)
	}
	opts := integrity.Options{Store: store, Catalog: cat}
	if v := durabilityFor(ctx, cfg.DataDir, cat, db); v != nil {
		opts.Durability = v
	}
	return fn(ctx, opts, store, cat, cfg.DataDir)
}

// durabilityFor builds the placement precondition's remote half, or nothing.
//
// Nothing is the safe answer and it is not the convenient one: a collector with
// no Durability REFUSES to unlink anything in a deployment that has another
// peer (ADR-0018, M4-12), and reclaims normally in one that does not. So a
// single-node install with no peer key on disk — the ordinary Milestone 1
// through 3 shape — still collects, and a two-site install whose key is missing
// or unreadable declines to delete and says why, rather than deleting because a
// dependency was absent.
//
// That asymmetry is the whole reason this returns nil instead of an error: an
// unavailable check must never be indistinguishable from a passed one.
func durabilityFor(ctx context.Context, dataDir string, cat *catalog.Catalog, db *sqlite.DB) integrity.Durability {
	self, err := cat.SelfPeer(ctx)
	if err != nil {
		return nil
	}
	priv, err := identity.Signer(dataDir)
	if err != nil {
		return nil
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: self})
	if err != nil {
		return nil
	}
	v, err := durability.New(durability.Options{
		Material:   material,
		Controller: durability.LocalControlPlane(db.Writer()),
	})
	if err != nil {
		return nil
	}
	return v
}

// ErrDamage is returned when fsck finds content that is missing or corrupt.
//
// It exists so `heyarr fsck` exits non-zero on damage. A checker that reports
// corruption and then exits 0 is worse than no checker: it will be wired into
// a cron job, its output will stop being read, and its silence will be trusted.
var ErrDamage = fmt.Errorf("fsck: integrity damage found")

func newFsckCommand(_ Options, configPath *string) *cobra.Command {
	var (
		deep   bool
		asJSON bool
		repair bool
	)
	cmd := &cobra.Command{
		Use:   "fsck",
		Short: "Check stored bytes against the catalog (§57, ADR-0018)",
		Long: `Reconcile expected hashes against the bytes actually on disk.

A shallow check confirms every blob the catalog knows about exists and is the
right length. It is fast and it catches a deleted or truncated file, but it
cannot catch a file that was rewritten in place at the same length.

--deep re-hashes everything. That is the check that matters on a hardlink-
ingested library, where a blob shares its inode with the file it was adopted
from and an external tool writing to that file rewrites the blob. Any blob whose
bytes no longer hash to their own name is moved to quarantine and recorded —
never deleted, because on such a library the "corruption" may be the operator's
original (ADR-0018).

Bytes with no catalog row and partial writes are reported too, but they are
waste rather than damage. fsck exits non-zero only for damage.

--repair attempts to rebuild each damaged blob from the chunks that are still
intact plus replacements fetched from a peer. Nothing is written in place: the
replacement is assembled in the store's private staging area, verified against
the blob's own digest, and published only after the damaged original has been
moved to quarantine (ADR-0036). A repair that cannot complete — no manifest, no
reachable peer, a peer whose copy is damaged too — changes nothing at all and
says which of those it was.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withIntegrity(cmd.Context(), *configPath, func(
				ctx context.Context, opts integrity.Options, store *cas.FS, cat *catalog.Catalog,
				dataDir string,
			) error {
				checker, err := integrity.NewChecker(opts)
				if err != nil {
					return err
				}
				report, err := checker.Check(ctx, integrity.CheckOptions{Deep: deep})
				if err != nil {
					return err
				}
				if !repair {
					if err := printReport(cmd.OutOrStdout(), report, asJSON); err != nil {
						return err
					}
					if report.Damage() > 0 {
						return fmt.Errorf("%w: %d of %d blobs checked are missing or corrupt",
							ErrDamage, report.Damage(), report.BlobsChecked)
					}
					return nil
				}

				results, err := repairDamage(ctx, opts, store, cat, dataDir, report)
				if err != nil {
					return err
				}
				if err := printRepairedReport(cmd.OutOrStdout(), report, results, asJSON); err != nil {
					return err
				}
				remaining := report.Damage() - repairedCount(results)
				if remaining > 0 {
					return fmt.Errorf("%w: %d of %d damaged blobs are still damaged after the "+
						"repair pass", ErrDamage, remaining, report.Damage())
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&deep, "deep", false, "re-hash every blob instead of checking existence and length")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	cmd.Flags().BoolVar(&repair, "repair", false,
		"rebuild damaged blobs from intact chunks plus replacements from a peer (ADR-0036)")
	return cmd
}

// repairDamage runs a repair for every blob the check found damaged.
//
// Only for damage. Untracked bytes and partial writes are waste, and garbage
// collection reclaims them; running a repair over them would fetch from a peer
// to rebuild something nothing references.
func repairDamage(
	ctx context.Context, opts integrity.Options, store *cas.FS, cat *catalog.Catalog,
	dataDir string, report integrity.Report,
) ([]integrity.RepairResult, error) {
	repairer, err := integrity.NewRepairer(integrity.RepairOptions{
		Store:     store,
		Manifests: cat,
		Catalog:   cat,
		// May still be nil, and a nil one REFUSES rather than permits: every
		// damaged blob is reported as unreachable, with the reason, instead of
		// the command appearing to have tried. That is now the answer for a
		// node with no peer identity on disk rather than the answer for every
		// node.
		Source: chunkSourceFor(ctx, dataDir, store, cat, opts.Logger),
		Clock:  opts.Clock,
		Logger: opts.Logger,
	})
	if err != nil {
		return nil, err
	}
	var results []integrity.RepairResult
	for _, f := range report.Findings {
		if !f.Kind.Damage() || f.Hash == "" {
			continue
		}
		h, err := hashing.Parse(f.Hash)
		if err != nil {
			return nil, err
		}
		result, err := repairer.Repair(ctx, h)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

// chunkSourceFor builds the peer-backed replacement-chunk source, or nothing.
//
// The same shape as durabilityFor, and for the same reason: NOTHING is the
// safe answer and it is not the convenient one. A repairer with no source
// reports every damaged blob as unreachable and says why, which is honest on a
// single-node install that has no peers to ask; a repairer that silently got a
// half-built source would report a fault that is really a missing key.
//
// It is built from the same three things replication uses — this node's
// identity, its mTLS material and the catalog's view of which peers report
// holding the bytes — so a repair fetches over exactly the path a transfer
// does, from exactly the peers a transfer would use. There is no second
// notion of "where bytes come from" (ADR-0030).
//
// The chunk index is deliberately NOT wired in. It is what lets a transfer
// reuse chunks this node already holds, and a repair has no use for it: the
// chunks this node holds for THIS blob are the ones it just verified locally,
// and repair has already read them. Handing it an index would invite it to
// source a replacement from another blob, which is a storage-level decision
// nobody has taken (#194's stance, and ADR-0034 does not reach it).
func chunkSourceFor(
	ctx context.Context, dataDir string, store *cas.FS, cat *catalog.Catalog, log *slog.Logger,
) integrity.ChunkSource {
	self, err := cat.SelfPeer(ctx)
	if err != nil {
		return nil
	}
	priv, err := identity.Signer(dataDir)
	if err != nil {
		return nil
	}
	material, err := mtls.NewMaterial(mtls.MaterialOptions{PrivateKey: priv, PeerID: self})
	if err != nil {
		return nil
	}
	puller, err := transfer.New(transfer.Options{
		Material: material,
		Store:    store,
		Logger:   log,
	})
	if err != nil {
		return nil
	}
	src, err := repairsource.New(repairsource.Options{
		Sources: cat,
		Fetcher: puller,
		Logger:  log,
	})
	if err != nil {
		return nil
	}
	return src
}

func repairedCount(results []integrity.RepairResult) int {
	var n int
	for _, r := range results {
		if r.Outcome.Repaired() {
			n++
		}
	}
	return n
}

// printRepairedReport prints the check and then what the repair pass did.
//
// What was repaired, how many chunks moved, and — for every blob that was not
// repaired — WHY. A refusal nobody can read is an outage nobody can diagnose
// (M4-12).
func printRepairedReport(
	w io.Writer, report integrity.Report, results []integrity.RepairResult, asJSON bool,
) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if results == nil {
			results = []integrity.RepairResult{}
		}
		return enc.Encode(struct {
			Check   integrity.Report         `json:"check"`
			Repairs []integrity.RepairResult `json:"repairs"`
		}{Check: report, Repairs: results})
	}
	if err := printReport(w, report, false); err != nil {
		return err
	}
	return printRepairs(w, results)
}

func printRepairs(w io.Writer, results []integrity.RepairResult) error {
	fmt.Fprintf(w, "\nrepair\n\n")
	if len(results) == 0 {
		fmt.Fprintln(w, "  nothing damaged to repair")
		return nil
	}
	fmt.Fprintf(w, "  blobs attempted   %d\n", len(results))
	fmt.Fprintf(w, "  repaired          %d\n\n", repairedCount(results))
	for _, r := range results {
		verdict := "NOT REPAIRED"
		if r.Outcome.Repaired() {
			verdict = "REPAIRED"
		}
		fmt.Fprintf(w, "%-13s %-18s %s\n", verdict, r.Outcome, r.Hash)
		fmt.Fprintf(w, "              %d of %d chunks damaged, %d fetched from a peer (%d bytes "+
			"of a %d byte blob)\n",
			r.ChunksDamaged, r.ChunksTotal, r.ChunksFetched, r.BytesFetched, r.BlobSize)
		if r.Detail != "" {
			fmt.Fprintf(w, "              %s\n", r.Detail)
		}
		if r.QuarantinePath != "" {
			fmt.Fprintf(w, "              the damaged original is at %s\n", r.QuarantinePath)
		}
	}
	return nil
}

func printReport(w io.Writer, report integrity.Report, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	mode := "shallow"
	if report.Deep {
		mode = "deep"
	}
	fmt.Fprintf(w, "fsck (%s)\n\n", mode)
	fmt.Fprintf(w, "  blobs in catalog  %d\n", report.BlobsInCatalog)
	fmt.Fprintf(w, "  blobs checked     %d\n", report.BlobsChecked)
	fmt.Fprintf(w, "  files in store    %d\n", report.FilesInStore)
	if report.Deep {
		fmt.Fprintf(w, "  bytes re-hashed   %d\n", report.BytesRead)
	}
	fmt.Fprintf(w, "  damage            %d\n", report.Damage())
	fmt.Fprintf(w, "  reclaimable       %d\n\n", report.Reclaimable())

	if len(report.Findings) == 0 {
		fmt.Fprintln(w, "no problems found")
		return nil
	}
	for _, f := range report.Findings {
		subject := f.Hash
		if subject == "" {
			subject = f.Path
		}
		severity := "waste"
		if f.Kind.Damage() {
			severity = "DAMAGE"
		}
		fmt.Fprintf(w, "%-7s %-14s %s\n", severity, f.Kind, subject)
		if f.Detail != "" {
			fmt.Fprintf(w, "        %s\n", f.Detail)
		}
		if f.ActualHash != "" {
			fmt.Fprintf(w, "        hashes to %s\n", f.ActualHash)
		}
		if f.ExpectedSize != 0 && f.ActualSize != f.ExpectedSize {
			fmt.Fprintf(w, "        expected %d bytes, found %d\n", f.ExpectedSize, f.ActualSize)
		}
		if f.Quarantined {
			fmt.Fprintf(w, "        quarantined at %s\n", f.Path)
		}
	}
	if report.Damage() > 0 {
		fmt.Fprintf(w, "\nQuarantined bytes are kept, never deleted. On a hardlink-ingested\n"+
			"library a mismatch may be the original file changing under Heyarr rather\n"+
			"than storage failing — compare before you conclude (ADR-0018).\n")
	}
	return nil
}

func newGCCommand(_ Options, configPath *string) *cobra.Command {
	var (
		apply     bool
		dryRun    bool
		grace     time.Duration
		tempGrace time.Duration
		asJSON    bool
	)
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Reclaim bytes nothing references (ADR-0018)",
		Long: `Reclaim blobs that no asset references, plus orphaned partial writes and
bytes in the store with no catalog row.

This command changes nothing unless you pass --apply. That is the default
because a garbage collector is the one piece of Heyarr whose bugs are not
recoverable by re-running it.

Reclamation is two-pass. The first sweep that sees a blob with no references
records when it noticed and reclaims nothing; a later sweep, once the grace
window has passed, frees the bytes. So a mistaken delete stays reversible for
the length of the window, and a blob that regains a reference gets a fresh
window rather than a partly spent one.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apply && cmd.Flags().Changed("dry-run") && dryRun {
				return fmt.Errorf("gc: --apply and --dry-run contradict each other; " +
					"pass --apply alone to reclaim, or neither to report")
			}
			// Zero means "use the default" everywhere below this point, which
			// is the safe reading of an unset field but the wrong reading of a
			// flag somebody typed. Say so rather than silently applying a week.
			if cmd.Flags().Changed("grace") && grace == 0 {
				return fmt.Errorf("gc: --grace 0 is ambiguous — it reads as "+
					"\"use the default\" (%s). Pass a small non-zero duration such as 1ns "+
					"to reclaim without waiting", integrity.DefaultGrace)
			}
			return withIntegrity(cmd.Context(), *configPath, func(
				ctx context.Context, opts integrity.Options, _ *cas.FS, _ *catalog.Catalog, _ string,
			) error {
				collector, err := integrity.NewCollector(opts)
				if err != nil {
					return err
				}
				result, err := collector.Collect(ctx, integrity.CollectOptions{
					Apply:     apply,
					Grace:     grace,
					TempGrace: tempGrace,
				})
				if err != nil {
					return err
				}
				return printCollection(cmd.OutOrStdout(), result, asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "actually reclaim; without it nothing is changed")
	cmd.Flags().BoolVar(&dryRun, "dry-run", true, "report what would be reclaimed without doing it (the default)")
	cmd.Flags().DurationVar(&grace, "grace", integrity.DefaultGrace,
		"how long a blob must have been unreferenced before its bytes may be freed")
	cmd.Flags().DurationVar(&tempGrace, "temp-grace", integrity.DefaultTempGrace,
		"how old a partial write must be before it is swept")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func printCollection(w io.Writer, result integrity.Collection, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	mode := "dry run — nothing was changed"
	if !result.DryRun {
		mode = "applied"
	}
	fmt.Fprintf(w, "gc (%s)\n\n", mode)
	fmt.Fprintf(w, "  grace window        %s\n", result.Grace)
	fmt.Fprintf(w, "  blobs considered    %d\n", result.Considered)
	fmt.Fprintf(w, "  still referenced    %d\n", result.Referenced)
	fmt.Fprintf(w, "  window started      %d\n", len(result.Marked))
	fmt.Fprintf(w, "  waiting out window  %d\n", len(result.Waiting))
	fmt.Fprintf(w, "  reclaimed           %d\n", len(result.Reclaimed))
	fmt.Fprintf(w, "  untracked files     %d\n", len(result.Untracked))
	fmt.Fprintf(w, "  partial writes      %d\n", len(result.TempRemoved))
	fmt.Fprintf(w, "  spared              %d\n", len(result.Spared)+len(result.UntrackedSpared))
	fmt.Fprintf(w, "  bytes reclaimed     %d\n", result.BytesReclaimed)

	// Why, not only how many. A sweep that reports "reclaimed 0" and stops
	// there is indistinguishable from a healthy library, a peer that has been
	// down since Tuesday and a collector that silently failed — and an
	// operator who cannot tell those apart stops reading the output
	// (ADR-0018, M4-12).
	// Each explanation is printed ONCE, and after that the blobs it covers are
	// listed by hash and reason alone. A sweep-wide refusal applies the same
	// sentence to every file in the store, and repeating a paragraph ten
	// thousand times turns the one thing an operator needed to read into the
	// thing they scroll past.
	said := map[string]bool{}
	explain := func(prefix, detail string) {
		if detail == "" || said[detail] {
			return
		}
		said[detail] = true
		fmt.Fprintf(w, "%s%s\n", prefix, detail)
	}
	if len(result.Refusals) > 0 {
		fmt.Fprintln(w)
		for _, r := range result.Refusals {
			fmt.Fprintf(w, "  REFUSED  %s\n", r.Reason)
			explain("           ", r.Detail)
		}
	}
	if len(result.Spared) > 0 || len(result.UntrackedSpared) > 0 {
		fmt.Fprintln(w)
		for _, sp := range append(append([]integrity.Sparing{}, result.Spared...), result.UntrackedSpared...) {
			fmt.Fprintf(w, "  spared        %s  %s\n", sp.Hash, sp.Reason)
			explain("                ", sp.Detail)
		}
	}

	if len(result.Reclaimed) > 0 || len(result.Untracked) > 0 {
		fmt.Fprintln(w)
		for _, c := range result.Reclaimed {
			fmt.Fprintf(w, "  unreferenced  %s  %d bytes\n", c.Hash, c.Size)
		}
		for _, c := range result.Untracked {
			fmt.Fprintf(w, "  untracked     %s  %d bytes\n", c.Hash, c.Size)
		}
	}
	if len(result.Waiting) > 0 {
		fmt.Fprintln(w)
		for _, c := range result.Waiting {
			fmt.Fprintf(w, "  waiting       %s  eligible %s\n", c.Hash, c.EligibleAt.Format(time.RFC3339))
		}
	}
	if result.DryRun && (len(result.Reclaimed) > 0 || len(result.Untracked) > 0 || len(result.TempRemoved) > 0) {
		fmt.Fprintf(w, "\nNothing was removed. Re-run with --apply to reclaim.\n")
	}
	return nil
}
