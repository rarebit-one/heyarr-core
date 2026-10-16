package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	psapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
)

// newSpaceMembersCommand builds `heyarr space members` — who an encrypted
// space is shared with (ADR-0108, §47). The same tree is mounted under the
// mnemosyne binary, where the personal-state plane lives.
func newSpaceMembersCommand(opts Options, configPath, deviceDir *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "members",
		Short: "List, add and remove the members of a shared space (ADR-0108)",
		Long: `Manage who an encrypted space is shared with.

The space is the unit of sharing (ADR-0108): a member's enrolled devices may
each hold a wrapped copy of the space key, and a user who is not a member may
not be given one. Only the space owner's device changes membership (any
management-authorised device on a family space, or on a space with no recorded
owner); members may not re-share.

Adding a member is two steps, both done here: the controller records the
membership and names the member's enrolled device keys, then THIS device wraps
the space's current key for each of them and pushes the copies. The controller
never wraps — it holds no key to wrap with (Invariant 6).

Removing a member records the removal, after which no new copy of the key is
admitted for their devices. It does not itself re-key the space: the devices
keep the copies they hold until a rotation leaves them out. The command prints
that rotation; see 'space members remove --help' for why it may be refused.`,
	}
	cmd.AddCommand(
		newSpaceMembersListCommand(opts, configPath),
		newSpaceMembersAddCommand(opts, configPath, deviceDir),
		newSpaceMembersRemoveCommand(opts, configPath, deviceDir),
	)
	return cmd
}

func newSpaceMembersListCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "list <space-id>",
		Short: "List a space's members, current and removed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				members, err := c.SpaceMembers(ctx, args[0])
				if err != nil {
					return err
				}
				if flags.asJSON {
					if members == nil {
						members = []apiclient.SpaceMember{}
					}
					return emitJSON(cmd.OutOrStdout(), members)
				}
				if len(members) == 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "space %s has no membership record: its key is gated by device enrolment alone\n", args[0])
					return nil
				}
				for _, m := range members {
					state := "member"
					if m.Role == "owner" {
						state = "owner"
					}
					if !m.Current() {
						state = "removed " + m.RemovedAt
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  (added %s)\n", m.PrincipalID, state, m.AddedAt)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

// spaceMemberAddedView is the --json shape of `space members add`.
type spaceMemberAddedView struct {
	SpaceID     string `json:"space_id"`
	PrincipalID string `json:"principal_id"`
	Role        string `json:"role"`
	// KeyEpoch is the epoch the copies were sealed at (ADR-0103).
	KeyEpoch int `json:"key_epoch"`
	// WrappedFor are the member's device keys this device wrapped the space
	// key for.
	WrappedFor []string `json:"wrapped_for"`
}

func newSpaceMembersAddCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags deviceClientFlags
		user  string
	)
	cmd := &cobra.Command{
		Use:   "add <space-id> --user <principal>",
		Short: "Share a space with a user: record the membership and wrap the key for their devices",
		Long: `Add a user as a member of an encrypted space.

Two steps, one command. The controller records the membership and names the
user's enrolled device encryption keys; then this device opens the space with
its own copy of the key, wraps the current key for each of those devices and
pushes the copies. From then on the member's devices read the space, and
everything written before it, through the key history.

This device must be one the space is wrapped for, enrolled, and authorised for
management; if the space has a recorded owner, it must be that owner's device
(on a family space, any such device will do). A user with no enrolled device is
refused: they enrol first.

If the space is re-keyed while this runs, the controller refuses the stale
wraps and the membership stands without them; run the command again to wrap
at the new epoch. Running it again is always safe.

Undo with 'space members remove'.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spaceID := args[0]
			return flags.withDeviceClient(cmd, configPath, *deviceDir, func(ctx context.Context, c *apiclient.Client) error {
				added, err := c.AddSpaceMember(ctx, spaceID, user)
				if err != nil {
					return memberError(err)
				}
				cust, err := selectCustody(ctx, configPath, *deviceDir, cmd.ErrOrStderr())
				if err != nil {
					return err
				}
				mgr, err := openSpace(ctx, c, cust, spaceID)
				if err != nil {
					return fmt.Errorf("%s is now a member of space %s, but this device could not open the space to wrap the key for their devices: %w",
						added.PrincipalID, spaceID, err)
				}
				wraps := make([]apiclient.WrappedKeyInput, 0, len(added.WrapFor))
				var epoch int
				for _, id := range added.WrapFor {
					r, err := client.ParseRecipient(id)
					if err != nil {
						return err
					}
					wrapped, e, err := mgr.WrapCurrent(spaceID, r)
					if err != nil {
						return err
					}
					epoch = e
					wraps = append(wraps, apiclient.WrappedKeyInput{Recipient: wrapped.Recipient, Wrapped: wrapped.Wrapped, Epoch: e})
				}
				if len(wraps) > 0 {
					if err := c.RewrapKeys(ctx, spaceID, wraps); err != nil {
						return memberError(fmt.Errorf("%s is now a member of space %s, but wrapping the key for their %d device(s) failed: %w",
							added.PrincipalID, spaceID, len(wraps), err))
					}
				}
				view := spaceMemberAddedView{
					SpaceID: spaceID, PrincipalID: added.PrincipalID, Role: added.Role,
					KeyEpoch: epoch, WrappedFor: added.WrapFor,
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), view)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s added to space %s as %s\n\n  key wrapped for %d device(s) at epoch %d:\n",
					added.PrincipalID, spaceID, added.Role, len(added.WrapFor), epoch)
				for _, id := range added.WrapFor {
					fmt.Fprintf(cmd.OutOrStdout(), "    %s\n", id)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&user, "user", "", "the user to add, by principal name or id")
	_ = cmd.MarkFlagRequired("user")
	return cmd
}

// spaceMemberRemovedView is the --json shape of `space members remove`.
type spaceMemberRemovedView struct {
	SpaceID     string `json:"space_id"`
	PrincipalID string `json:"principal_id"`
	RemovedAt   string `json:"removed_at"`
	// Revoke are the removed member's device keys still holding a copy of the
	// current key: the --revoke list of the rotation that completes the
	// removal. Empty when they hold none and no rotation is needed.
	Revoke []string `json:"revoke"`
	// Rotation is that command, or "" when none is needed.
	Rotation string `json:"rotation,omitempty"`
}

func newSpaceMembersRemoveCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags deviceClientFlags
		user  string
	)
	cmd := &cobra.Command{
		Use:   "remove <space-id> --user <principal>",
		Short: "Stop sharing a space with a user: record the removal and name the rotation that completes it",
		Long: `Remove a user from an encrypted space.

The controller records the removal. From then on no new copy of the space key
is admitted for the user's devices — not by a re-wrap, and not by a rotation,
which must name them in --revoke instead. The same caller rule as 'add'.

This is NOT yet forward secrecy. The user's devices keep every copy they hold,
and read everything sealed under that key, until the space is re-keyed without
them (ADR-0103). The command prints that rotation, with the recipients to
revoke, for an admin token to run:

  heyarr space rotate <space-id> --revoke <device-key> [--revoke ...]

Rotation is refused for every space that is not a playlist (a vault drive
among them) until older clients understand key epochs (#698, #706). Until that
guard lifts, removing a member from such a space closes the gate on new copies
and leaves the current key with the devices that already hold it — say so to
the owner rather than letting the record suggest otherwise.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spaceID := args[0]
			return flags.withDeviceClient(cmd, configPath, *deviceDir, func(ctx context.Context, c *apiclient.Client) error {
				removed, err := c.RemoveSpaceMember(ctx, spaceID, user)
				if err != nil {
					return memberError(err)
				}
				view := spaceMemberRemovedView{
					SpaceID: spaceID, PrincipalID: removed.PrincipalID, RemovedAt: removed.RemovedAt,
					Revoke: removed.Revoke,
				}
				if view.Revoke == nil {
					view.Revoke = []string{}
				}
				if len(view.Revoke) > 0 {
					view.Rotation = rotationCommand(spaceID, view.Revoke)
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), view)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s removed from space %s\n", removed.PrincipalID, spaceID)
				if len(view.Revoke) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "  none of their devices holds the current key; no rotation is needed")
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %d of their device(s) still hold the current key (epoch %d). To take it back, re-key the space without them:\n\n    %s\n",
					len(view.Revoke), removed.KeyEpoch, view.Rotation)
				fmt.Fprintln(cmd.ErrOrStderr(), "note: until that rotation lands the removed member reads the space as before. "+
					"Rotation is refused for a space that is not a playlist until #698 is resolved (see --help).")
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&user, "user", "", "the user to remove, by principal name or id")
	_ = cmd.MarkFlagRequired("user")
	return cmd
}

// rotationCommand renders the `space rotate` that completes a removal.
func rotationCommand(spaceID string, revoke []string) string {
	var b strings.Builder
	b.WriteString("space rotate " + spaceID)
	for _, r := range revoke {
		b.WriteString(" --revoke " + r)
	}
	return b.String()
}

// memberError explains the refusals an operator can act on.
func memberError(err error) error {
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) || apiErr.Problem == nil {
		return err
	}
	switch apiErr.Problem.Code {
	case psapi.CodeMemberHasNoDevice:
		return fmt.Errorf("space members: that user has no enrolled device to wrap the key for; they enrol one first: %w", err)
	case psapi.CodeMemberIsOwner:
		return fmt.Errorf("space members: the owner cannot be removed from their own space: %w", err)
	case psapi.CodeKeyEpochStale, psapi.CodeKeyEpochAhead:
		return fmt.Errorf("space members: the space was re-keyed while wrapping, so no copy was recorded — run the command again: %w", err)
	case psapi.CodeWrapRecipientNotMember:
		return fmt.Errorf("space members: the controller refused to wrap for a device whose user is not a member: %w", err)
	}
	return err
}
