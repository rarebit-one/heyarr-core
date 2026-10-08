package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	psapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/servicerecipient"
)

// spaceGrantView is the --json shape of `space grant`.
type spaceGrantView struct {
	SpaceID     string `json:"space_id"`
	PrincipalID string `json:"principal_id"`
	Caps        string `json:"caps"`
	Recipient   string `json:"recipient"`
	Fingerprint string `json:"fingerprint"`
	KeyEpoch    int    `json:"key_epoch"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

// errGrantDeclined is the operator answering no at the confirmation.
var errGrantDeclined = errors.New("space grant: declined; nothing was recorded")

// newSpaceGrantCommand builds `heyarr space grant` — the consent act of
// ADR-0104: wrap the space's current key for an executor's registered service
// recipient AND grant the executor the space, in one request the controller
// records in one transaction.
func newSpaceGrantCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags               deviceClientFlags
		executor, recipient string
		write, yes          bool
		expires             string
	)
	cmd := &cobra.Command{
		Use:   "grant <space-id> --executor <principal> --recipient x25519:<hex>",
		Short: "Let an executor fetch and decrypt a space (ADR-0104)",
		Long: `Grant an executor access to an encrypted space.

Two gates open together. The executor may FETCH the space's ciphertext (a
grant, read or --write), and it can DECRYPT it, because this device wraps the
space's current key for the executor's registered service recipient (register
it first with ` + "`heyarr recipient add`" + `). With the current key the executor
also reads everything written before, through the space's key history.

This device must be one the space is wrapped for, enrolled, and authorised for
management; if the space has a recorded owner, it must be that owner's device.

The grant and the wrap are one request, recorded in one transaction: either
both land or neither does. If the space is re-keyed while this runs, the
controller refuses the stale wrap and nothing is recorded; run the command
again. If the reply is lost, running it again is safe: the grant is renewed and
the copy replaced.

Undo with ` + "`heyarr space revoke-executor`" + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spaceID := args[0]
			fp, err := servicerecipient.Fingerprint(recipient)
			if err != nil {
				return err
			}
			r, err := client.ParseRecipient(recipient)
			if err != nil {
				return err
			}
			var expiresAt *time.Time
			if expires != "" {
				t, err := parseExpiry(expires, time.Now().UTC())
				if err != nil {
					return err
				}
				expiresAt = &t
			}
			caps := "read"
			if write {
				caps = "read,write"
			}
			return flags.withDeviceClient(cmd, configPath, *deviceDir, func(ctx context.Context, c *apiclient.Client) error {
				label, err := registeredLabel(ctx, c, recipient)
				if err != nil {
					return err
				}
				if !yes {
					if err := confirmGrant(cmd, label, fp, executor, spaceID, write); err != nil {
						return err
					}
				}
				cust, err := selectCustody(configPath, *deviceDir)
				if err != nil {
					return err
				}
				mgr, err := openSpace(ctx, c, cust, spaceID)
				if err != nil {
					return err
				}
				wrapped, epoch, err := mgr.WrapCurrent(spaceID, r)
				if err != nil {
					return err
				}
				g, err := c.GrantSpace(ctx, spaceID, apiclient.GrantRequest{
					Principal: executor, Caps: caps, ExpiresAt: expiresAt,
					WrappedKeys: []apiclient.WrappedKeyInput{{Recipient: wrapped.Recipient, Wrapped: wrapped.Wrapped, Epoch: epoch}},
				})
				if err != nil {
					return grantError(err)
				}
				view := spaceGrantView{
					SpaceID: spaceID, PrincipalID: g.PrincipalID, Caps: g.Caps,
					Recipient: recipient, Fingerprint: fp, KeyEpoch: epoch, ExpiresAt: g.ExpiresAt,
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), view)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "executor %s granted %s on space %s\n\n  key wrapped for:  %s\n  fingerprint:      %s\n  at key epoch:     %d\n",
					g.PrincipalID, g.Caps, spaceID, recipient, fp, epoch)
				if g.ExpiresAt != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  fetch expires:    %s\n", g.ExpiresAt)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&executor, "executor", "", "the executor principal, by name or id")
	cmd.Flags().StringVar(&recipient, "recipient", "", "the executor's registered public key, x25519:<hex>")
	cmd.Flags().BoolVar(&write, "write", false, "also let the executor push changes and snapshots (read,write)")
	cmd.Flags().StringVar(&expires, "expires", "",
		"end the FETCH grant after a duration (720h) or at an RFC 3339 time; the wrapped key itself has no expiry")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation")
	_ = cmd.MarkFlagRequired("executor")
	_ = cmd.MarkFlagRequired("recipient")
	return cmd
}

// parseExpiry reads --expires as a duration from now or an RFC 3339 time, and
// refuses one already past.
func parseExpiry(s string, now time.Time) (time.Time, error) {
	var t time.Time
	if d, err := time.ParseDuration(s); err == nil {
		t = now.Add(d)
	} else if t, err = time.Parse(time.RFC3339, s); err != nil {
		return time.Time{}, fmt.Errorf("--expires %q is neither a duration (720h) nor an RFC 3339 time", s)
	}
	if !t.After(now) {
		return time.Time{}, fmt.Errorf("--expires %q is not in the future", s)
	}
	return t.UTC(), nil
}

// registeredLabel finds the recipient among the registered service recipients,
// so the confirmation names what the operator registered. An unregistered key
// is refused here, before anything is unwrapped; the controller refuses it too.
func registeredLabel(ctx context.Context, c *apiclient.Client, recipient string) (string, error) {
	list, err := c.ServiceRecipients(ctx)
	if err != nil {
		return "", err
	}
	for _, sr := range list {
		if sr.Recipient == recipient {
			if sr.Label == "" {
				return "executor key " + sr.Fingerprint, nil
			}
			return sr.Label, nil
		}
	}
	return "", fmt.Errorf("%s is not a registered service recipient — register it first with "+
		"`heyarr recipient add --executor <principal> --pub %s`, comparing the fingerprint its host displays", recipient, recipient)
}

func confirmGrant(cmd *cobra.Command, label, fp, executor, spaceID string, write bool) error {
	what := "read"
	if write {
		what = "read and write"
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Allow %q (executor %s, key %s) to %s space %s?\n"+
			"It will hold the space's current key and can decrypt everything in the space, including its history. [y/N]: ",
		label, executor, fp, what, spaceID)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading your confirmation: %w", err)
	}
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errGrantDeclined
	}
	return nil
}

// grantError explains the refusals an operator can act on.
func grantError(err error) error {
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) || apiErr.Problem == nil {
		return err
	}
	switch apiErr.Problem.Code {
	case psapi.CodeKeyEpochStale, psapi.CodeKeyEpochAhead:
		return fmt.Errorf("space grant: the space was re-keyed while granting, so nothing was recorded — run it again: %w", err)
	case psapi.CodeWrapRecipientNotAllowed:
		return fmt.Errorf("space grant: that key is not registered for this executor, so nothing was recorded: %w", err)
	}
	return err
}

// newSpaceRevokeExecutorCommand builds `heyarr space revoke-executor` — the
// undo of `space grant`.
func newSpaceRevokeExecutorCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags    deviceClientFlags
		executor string
	)
	cmd := &cobra.Command{
		Use:   "revoke-executor <space-id> --executor <principal>",
		Short: "Withdraw an executor's grant on a space and delete its copy of the key",
		Long: `Withdraw an executor's access to a space. In one transaction the controller
revokes the grant, so the executor's next fetch of the space is refused, and
deletes every copy of the space key wrapped for the executor's service
recipients, so it can unwrap nothing more from this node.

This is NOT forward secrecy. An executor that already unwrapped the key keeps
it, and could read anything sealed under it that reaches it by other means. A
peer that replicated its copy keeps that copy too. Only a rotation that leaves
the executor out ends that:

  heyarr space rotate <space-id> --revoke <executor-key>    # first
  heyarr space revoke-executor <space-id> --executor <name> # then

Rotate first: once this command has deleted the copy, the key is no longer a
current recipient to revoke. Rotation is refused for every space that is not a
playlist (a vault drive among them) until older clients understand key epochs
(#698, #706). Until then, revoking an executor from a vault space closes the
fetch gate and deletes the wrap, and the space keeps its key.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spaceID := args[0]
			return flags.withDeviceClient(cmd, configPath, *deviceDir, func(ctx context.Context, c *apiclient.Client) error {
				if err := c.RevokeSpaceGrant(ctx, spaceID, executor); err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), map[string]any{"space_id": spaceID, "executor": executor, "revoked": true})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "executor %s revoked from space %s; its copy of the key deleted\n", executor, spaceID)
				fmt.Fprintln(cmd.ErrOrStderr(), "note: this is not forward secrecy — the space keeps its key. "+
					"See `heyarr space revoke-executor --help` for when a rotation is needed and possible.")
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&executor, "executor", "", "the executor principal, by name or id")
	_ = cmd.MarkFlagRequired("executor")
	return cmd
}
