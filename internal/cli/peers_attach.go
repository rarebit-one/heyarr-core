package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/api/peerapi"
	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/config"
)

// newPeersAttachCommand attaches this node to a controller and prints what
// that controller records it as.
//
// A Full Peer is controller-attached (ADR-0029): it runs no control plane and
// takes its scheduling, authorisation and read routing from a controller it
// now has to reach across a network it does not control. This is the command
// that proves it can — with the SAME identity it uses to reach a peer
// (ADR-0033), and no second credential anywhere.
func newPeersAttachCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "attach <name-or-id>",
		Short: "Attach to a controller over mTLS and report what it records this node as",
		Long: `Attach to a controller and print the attachment it answers with (ADR-0029,
ADR-0033).

The credential is this node's Ed25519 peer identity — the same one ` + "`peers ping`" + `
presents and the same one another peer pins. There is no per-peer token to
distribute, store or revoke separately: membership is the only trust root, and
removing the membership record is the revocation.

Two requests are made. The first asks the controller what it derived from this
node's certificate; the second sends that id back as a DECLARATION and the
controller compares it against the certificate again. A peer cannot act as
another peer by putting a different id in that body — the acting peer comes
from the certificate and never from the request — and this command sends the
declaration so that a node whose identity has drifted from its configuration
finds out here rather than after it has reported something.

A peer is not an admin. This credential reaches the peer surface only; token
management, peer enrolment and policy are the admin surface, on the
controller's client API, behind an admin-scoped bearer token.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *client.Client) error {
				return attachToController(ctx, cmd.OutOrStdout(), c, cfg, args[0], flags.asJSON)
			})
		},
	}
	flags.register(cmd)
	return cmd
}

func attachToController(ctx context.Context, out io.Writer, c *client.Client, cfg config.Config, ref string, asJSON bool) error {
	dial, err := peerDialer(ctx, c, cfg, ref)
	if err != nil {
		return err
	}

	// What the controller derives from the certificate, with nothing declared.
	var derived peerapi.Attachment
	if err := dial.call(ctx, http.MethodGet, "/attachment", nil, &derived); err != nil {
		return err
	}

	// The same fact, declared and checked. Sending the id the controller just
	// derived is not circular: the value under test is what the controller
	// does with a declaration, and a node whose key and configuration have
	// come apart is refused here rather than somewhere later and quieter.
	body, err := json.Marshal(peerapi.AttachRequest{PeerID: derived.PeerID})
	if err != nil {
		return err
	}
	var confirmed peerapi.Attachment
	if err := dial.call(ctx, http.MethodPost, "/attach", body, &confirmed); err != nil {
		return err
	}
	if confirmed.PeerID != derived.PeerID {
		return fmt.Errorf("the controller derived peer %s and then attached peer %s — "+
			"the acting identity is not stable across two requests on one certificate",
			derived.PeerID, confirmed.PeerID)
	}
	if confirmed.ControlPlane != peerapi.ControlPlaneAttached {
		return fmt.Errorf("the controller reported control plane %q, want %q: a Full Peer runs "+
			"no control plane of its own (ADR-0029)", confirmed.ControlPlane, peerapi.ControlPlaneAttached)
	}

	if asJSON {
		return emitJSON(out, confirmed)
	}
	t := newTable("CONTROLLER", "ATTACHED AS", "NAME", "CONTROL PLANE", "PRINCIPAL")
	t.add(dial.target.Name, confirmed.PeerID, confirmed.Name, confirmed.ControlPlane, confirmed.Principal)
	return t.render(out, "no attachment")
}
