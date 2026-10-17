package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/void-which-binds-go/enrolment"
	"github.com/rarebit-one/void-which-binds-go/rp"
	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/client"
)

// newDeviceEnrolCommand builds `heyarr device enrol`: this machine's paired
// device enrolling itself at a peer through POST /enrol (ADR-0067), the route
// the phone apps call after pairing. Before it, a device paired on the command
// line (void-which-binds pair-join) had no way to enrol at a controller that
// does not mount the admin identities API — the Mnemosyne personal profile — so
// nothing could be wrapped for it there (enrol-before-wrap, ADR-0049).
//
// Like the rest of `device` it reads no server configuration: the peer is named
// with --peer. It sends no bearer token either; the admitting op and a fresh
// possession proof are the credential.
func newDeviceEnrolCommand(_ Options, dir *string) *cobra.Command {
	var (
		peer    string
		name    string
		timeout time.Duration
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "enrol",
		Short: "Enrol this machine's paired device at a peer (POST /enrol, ADR-0067)",
		Long: `Enrol this machine's device at a peer on the strength of its own credential:
the op that admitted it (from pairing), a fresh possession proof signed with the
device key, and the membership ops it knows. This is what the phone apps do
after pairing; it needs no admin token.

The peer must already pin the device's user (` + "`admin user pin`" + `), and the device
must be enrolled locally (paired with ` + "`void-which-binds pair-join`" + ` or
` + "`heyarr pair enrol`" + `). Any refusal — an unpinned user, an expired or revoked
admission, a bad proof — is the peer's deliberately opaque 401; its log has the
reason.

The device authenticates at the read floor once enrolled. Write remains an
admin's authorisation of the device key (ADR-0065). Enrolment is per peer: run
this against each peer the device talks to.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(peer) == "" {
				return errors.New("device enrol: --peer is required (the peer's http(s):// URL)")
			}
			store, err := openDeviceStore(*dir)
			if err != nil {
				return err
			}
			dev, err := store.Get("")
			if err != nil {
				return err
			}
			if name == "" {
				name = dev.Name
			}
			// One mint: a sealed device asks for its passphrase here, once.
			cred, ops, err := store.CredentialWithOps(time.Now().UTC(), 0, rp.MaxPresentedOps)
			if err != nil {
				return fmt.Errorf("device enrol: this device cannot present a credential: %w", err)
			}
			op, proof, ok := strings.Cut(cred, enrolment.CredentialSeparator)
			if !ok || op == "" || proof == "" {
				return errors.New("device enrol: the device store minted a malformed credential")
			}
			c, err := client.New(client.Options{
				Addr:      peer,
				Timeout:   timeout,
				UserAgent: "heyarr-cli/" + buildinfo.Get().Version,
			})
			if err != nil {
				return err
			}
			enrolled, err := c.Enrol(cmd.Context(), client.EnrolRequest{Op: op, Proof: proof, Name: name, Ops: ops})
			if err != nil {
				var apiErr *client.Error
				if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
					return fmt.Errorf("device enrol: %s refused the enrolment (%w). The peer does not say why: "+
						"check that it pins this device's user (`admin user pin`), and that the admission has not "+
						"expired and the device is not revoked — the peer's log names the reason", c.Target(), err)
				}
				return fmt.Errorf("device enrol: %s: %w", c.Target(), err)
			}
			return printDeviceEnrolled(cmd.OutOrStdout(), c.Target(), enrolled, asJSON)
		},
	}
	cmd.Flags().StringVar(&peer, "peer", "", "the peer to enrol at: its http(s):// URL (required)")
	cmd.Flags().StringVar(&name, "name", "", "the name the peer records for this device (default: the device's own name)")
	cmd.Flags().DurationVar(&timeout, "timeout", client.DefaultTimeout, "how long the request may take")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func printDeviceEnrolled(w io.Writer, target string, d client.EnrolledDevice, asJSON bool) error {
	if asJSON {
		return emitJSON(w, d)
	}
	recovery := d.RecoveryEncryptionKey
	if recovery == "" {
		recovery = "(none — the user was pinned without one)"
	}
	fmt.Fprintf(w, "enrolled at %s\n\n", target)
	fmt.Fprintf(w, "  device        %s (%s)\n", d.DeviceKey, d.Name)
	fmt.Fprintf(w, "  encryption    %s\n", d.EncryptionKey)
	fmt.Fprintf(w, "  user          %s\n", d.User)
	fmt.Fprintf(w, "  admitted by   %s\n", d.AdmittedBy)
	fmt.Fprintf(w, "  recovery key  %s\n", recovery)
	fmt.Fprintf(w, "  expires       %s\n", d.ExpiresAt.UTC().Format(time.RFC3339))
	return nil
}
