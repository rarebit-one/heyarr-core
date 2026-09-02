package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
)

// deviceRevokeView is the --json shape of `device revoke`.
type deviceRevokeView struct {
	Device  apiclient.IdentityDevice `json:"device"`
	Rotated []spaceRotateView        `json:"rotated"`
	Skipped []skippedSpace           `json:"skipped"`
}

// skippedSpace is a space the revoked device could read that this command
// could NOT re-key, and why — surfaced rather than silently left readable.
type skippedSpace struct {
	SpaceID string `json:"space_id"`
	Reason  string `json:"reason"`
}

// newDeviceRevokeCommand builds `heyarr device revoke` — the admin's tombstone
// on a device at this peer (ADR-0048, ADR-0068), followed by the ADR-0049
// re-wrap for THAT ONE device: every space whose key is wrapped for the
// revoked device's encryption key is rotated away from it (`space rotate
// --revoke`), with no walk of the devices it may have admitted — a member's
// admissions stand until someone removes them (ADR-0007: no cascade).
func newDeviceRevokeCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags    clientFlags
		noRotate bool
	)
	cmd := &cobra.Command{
		Use:   "revoke <device-key>",
		Short: "Revoke a device at this peer and re-key the spaces it could read (ADR-0068, ADR-0049)",
		Long: `Revoke one of a user's devices at this peer, then rotate away from it.

The revocation is the peer's own tombstone (DELETE /identities/devices): the
device stops authenticating here at once, whatever the identity's membership
ops say, and stays refused if re-presented. It is NOT a membership op — the peer
is not a member of the identity and its word stays local. To remove the device
from the identity itself, a member device signs a remove and pushes it
(POST /membership/{usr}).

Revocation is forward-looking, not retroactive (ADR-0049): the device keeps
whatever it already decrypted, so every space whose key is wrapped for its
encryption key is re-keyed — a fresh key, re-wrapped for the remaining
recipients, the revoked copy deleted, a snapshot under the new key. THIS device
must itself be a recipient of a space to re-key it; a space it cannot read is
reported as skipped, for a device that can. Only the named device is re-keyed
away: the devices it admitted are untouched.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deviceKey := args[0]
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				revoked, err := c.RevokeIdentityDevice(ctx, deviceKey)
				if err != nil {
					return err
				}
				view := deviceRevokeView{Device: revoked, Rotated: []spaceRotateView{}, Skipped: []skippedSpace{}}
				if !noRotate && revoked.EncryptionKey != "" {
					view.Rotated, view.Skipped, err = rotateAwayFrom(ctx, c, *deviceDir, revoked.EncryptionKey)
					if err != nil {
						return err
					}
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), view)
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "revoked %s (%s)\n", revoked.DeviceKey, revoked.Name)
				switch {
				case noRotate:
					fmt.Fprintln(out, "  spaces NOT re-keyed (--no-rotate): the device keeps reading what is wrapped for it")
				case revoked.EncryptionKey == "":
					fmt.Fprintln(out, "  no encryption key bound to this device; no space was wrapped for it")
				default:
					fmt.Fprintf(out, "  re-keyed %d space(s) away from %s\n", len(view.Rotated), revoked.EncryptionKey)
					for _, r := range view.Rotated {
						fmt.Fprintf(out, "    %s: snapshot %s, %d change(s) compacted\n", r.SpaceID, r.SnapshotID, r.Dropped)
					}
					for _, sk := range view.Skipped {
						fmt.Fprintf(out, "    %s: NOT re-keyed — %s\n", sk.SpaceID, sk.Reason)
					}
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().BoolVar(&noRotate, "no-rotate", false,
		"only tombstone the device; leave every space it could read wrapped for it")
	return cmd
}

// rotateAwayFrom re-keys every space wrapped for recipient that THIS device can
// open. A space this device cannot read — or one whose rotation fails for a
// reason the operator can act on — is reported as skipped rather than aborting
// the sweep: the tombstone already stands, and each remaining space is a
// separate fact.
func rotateAwayFrom(ctx context.Context, c *apiclient.Client, deviceDir, recipient string) ([]spaceRotateView, []skippedSpace, error) {
	spaces, err := c.ListSpaces(ctx)
	if err != nil {
		return nil, nil, err
	}
	rotated, skipped := []spaceRotateView{}, []skippedSpace{}
	for _, sp := range spaces {
		keys, err := c.WrappedKeys(ctx, sp.ID)
		if err != nil {
			return nil, nil, err
		}
		if !wrappedFor(keys, recipient) {
			continue
		}
		view, err := rotateSpace(ctx, c, deviceDir, sp.ID, []string{recipient})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, nil, err
			}
			skipped = append(skipped, skippedSpace{SpaceID: sp.ID, Reason: err.Error()})
			continue
		}
		rotated = append(rotated, view)
	}
	return rotated, skipped, nil
}

// wrappedFor reports whether a space's key is wrapped for recipient.
func wrappedFor(keys []apiclient.WrappedKey, recipient string) bool {
	for _, k := range keys {
		if k.Recipient == recipient {
			return true
		}
	}
	return false
}
