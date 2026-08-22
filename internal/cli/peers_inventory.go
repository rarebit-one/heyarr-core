package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/peer/inventory"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// newPeersReportCommand reports this node's inventory to a controller.
//
// A Full Peer runs no control plane and cannot write control-plane rows
// directly (ADR-0029). What it can do is tell the controller what is on its
// disk, and the controller's single writer records that — which is the whole
// of M4-07 and the reason `replicas` stops being a table that only ever grows.
//
// It reports the STORE, not this node's catalog. A command that read the
// catalog would report the controller's own beliefs back to it and confirm
// nothing.
func newPeersReportCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	var incremental bool
	cmd := &cobra.Command{
		Use:   "report-inventory <name-or-id>",
		Short: "Tell a controller what this node's content store actually holds",
		Long: `Report this node's inventory to a controller (§19, §20, ADR-0029, ADR-0033).

` + "`replicas`" + ` on the controller is what the CONTROLLER believes. This node's
inventory is what is on its DISK. This command is where the two are compared,
and the controller's table is corrected to match the disk — including
downwards: a blob this node no longer holds becomes a ` + "`missing`" + ` replica rather
than staying present or quietly vanishing.

The inventory is derived from the content store, never from this node's
catalog. Quarantined blobs are reported as ` + "`corrupt`" + ` — the bytes are here and
cannot be served, and both "present" and "gone" are lies about that.

The credential is this node's Ed25519 peer identity, the same one ` + "`peers attach`" +
			` presents. The report declares which peer this node believes it is, and the
controller compares that against the certificate: a peer reports its own
inventory and only its own.

By default the report is FULL — everything this node holds, so a blob it does
not mention is asserted absent. ` + "`--incremental`" + ` sends only what changed since the
last report in this process, which for a scheduled reporter is the ordinary
cycle; run without it to correct drift.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				return reportInventory(ctx, cmd.OutOrStdout(), c, cfg, args[0], incremental, flags.asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&incremental, "incremental", false,
		"send only what changed since this process's previous report, rather than the whole set")
	flags.register(cmd)
	return cmd
}

func reportInventory(
	ctx context.Context, out io.Writer, c *client.Client, cfg config.Config,
	ref string, incremental, asJSON bool,
) error {
	dial, err := peerDialer(ctx, c, cfg, ref)
	if err != nil {
		return err
	}
	store, err := cas.OpenFS(cfg.CAS.Root)
	if err != nil {
		return fmt.Errorf("opening the content store at %s: %w", cfg.CAS.Root, err)
	}
	// Which peer the CONTROLLER thinks this node is.
	//
	// Not this node's own self-peer id: peer ids are rows in one instance's
	// catalog, and two instances that enrolled each other hold different ids
	// for the same machine. The identity that matters is the one the
	// controller derived from this node's certificate, and asking for it is
	// the only way to declare it — which is the point of GET /attachment
	// existing at all (ADR-0033).
	var derived peerapi.Attachment
	if err := dial.call(ctx, http.MethodGet, "/attachment", nil, &derived); err != nil {
		return err
	}

	snapshot, err := inventory.Collect(ctx, inventory.Options{Store: store, Quarantine: store})
	if err != nil {
		return err
	}

	// A single invocation has no previous observation to diff against, so
	// --incremental sends a diff against an EMPTY one: everything this node
	// holds, labelled as an incremental report. That is honest rather than
	// clever — an incremental report asserts nothing about what it omits, and
	// this one omits nothing — and it keeps the flag meaningful for the
	// scheduled reporter that does hold a baseline.
	empty, err := inventory.NewSnapshot(snapshot.ObservedAt, nil)
	if err != nil {
		return err
	}
	report := snapshot.Full(derived.PeerID)
	if incremental {
		report = snapshot.Since(empty, derived.PeerID)
	}

	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	var outcome inventory.Outcome
	if err := dial.call(ctx, http.MethodPost, "/inventory", body, &outcome); err != nil {
		return err
	}
	if outcome.PeerID != derived.PeerID {
		return fmt.Errorf("the controller derived peer %s from this node's certificate and then "+
			"recorded the report against %s — the acting identity is not stable across two "+
			"requests on one certificate (ADR-0033)", derived.PeerID, outcome.PeerID)
	}

	if asJSON {
		return emitJSON(out, outcome)
	}
	t := newTable("CONTROLLER", "REPORTED AS", "MODE", "ENTRIES", "ADDED", "CHANGED", "REMOVED", "UNKNOWN")
	t.add(dial.target.Name, outcome.PeerID, string(outcome.Mode),
		itoaCLI(outcome.Entries), itoaCLI(outcome.Added), itoaCLI(outcome.Changed),
		itoaCLI(outcome.Removed), itoaCLI(outcome.Unknown))
	if err := t.render(out, "no outcome"); err != nil {
		return err
	}
	// Freshness, stated rather than implied: the point of a report is that the
	// controller's rows are now dated, and an operator running this by hand
	// wants to see the date they just set.
	_, err = fmt.Fprintf(out, "observed at %s, recorded at %s\n",
		outcome.ObservedAt.Format(time.RFC3339), outcome.ReceivedAt.Format(time.RFC3339))
	return err
}

// itoaCLI renders a count for the table. strconv would do, and this file
// already avoids importing it for one call in every other command here.
func itoaCLI(n int) string { return fmt.Sprintf("%d", n) }
