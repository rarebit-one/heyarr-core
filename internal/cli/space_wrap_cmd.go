package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	psapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
)

// spaceWrapView is the --json shape of `space wrap`.
type spaceWrapView struct {
	SpaceID string `json:"space_id"`
	// KeyEpoch is the epoch the copies were (or, on a dry run, would be)
	// sealed at (ADR-0103).
	KeyEpoch int  `json:"key_epoch"`
	DryRun   bool `json:"dry_run"`
	// Wrapped are the devices this device wrapped the key for — on a dry run,
	// the ones it would wrap for.
	Wrapped []spaceWrapDeviceView `json:"wrapped"`
	// AlreadyHeld are the named devices that already hold a copy of the
	// current key, so nothing was wrapped for them.
	AlreadyHeld []spaceWrapDeviceView `json:"already_held"`
}

type spaceWrapDeviceView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Recipient string `json:"recipient"`
}

func newSpaceWrapCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags   deviceClientFlags
		devices []string
		all     bool
		dryRun  bool
	)
	cmd := &cobra.Command{
		Use:   "wrap <space-id> (--device <id> ... | --all-devices)",
		Short: "Wrap a space's key for another of your own enrolled devices (ADR-0049)",
		Long: `Give another of your own devices the key of a space this device holds.

Pairing or enrolling a new device admits it to your identity but wraps it no
space key: until a device that holds the key wraps it for the new one, the new
device shows the space but cannot open it. The controller cannot do this for
you — it holds no key to wrap with (Invariant 6). This command is that step.

It asks the controller which of your user's enrolled devices lack a copy of the
space's current key, opens the space with this device's own copy, wraps the
current key for each device you name (or, with --all-devices, every active one
lacking a copy) and pushes the copies, which replicate with the space like any
other wrapped key. Only your own devices are targets: a device of another user
is shared with through 'space members add'. A target must be enrolled, not
revoked, and carry an encryption key (enrol-before-wrap); the controller checks
that again when the copies arrive, along with the space's membership gate.

--device takes the device identity id that pairing reports, its device key
(ed25519:...) or its encryption key (x25519:...), and may be repeated. A device
that already holds the current key is reported and skipped, so running the
command again is always safe. --dry-run lists what would be wrapped and opens
nothing.

Run it on a device that holds the space, once per space:

  mnemosyne space wrap <space-id> --all-devices`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all == (len(devices) > 0) {
				return errors.New("name the devices to wrap for with --device, or pass --all-devices (not both)")
			}
			spaceID := args[0]
			return flags.withDeviceClient(cmd, configPath, *deviceDir, func(ctx context.Context, c *apiclient.Client) error {
				own, err := c.SpaceOwnDevices(ctx, spaceID)
				if err != nil {
					return wrapError(err)
				}
				targets, held, err := wrapTargets(own, devices, all)
				if err != nil {
					return err
				}
				view := spaceWrapView{
					SpaceID: spaceID, KeyEpoch: own.KeyEpoch, DryRun: dryRun,
					Wrapped: wrapDeviceViews(targets), AlreadyHeld: wrapDeviceViews(held),
				}
				if len(targets) > 0 && !dryRun {
					epoch, err := wrapForDevices(ctx, c, configPath, *deviceDir, cmd, spaceID, targets)
					if err != nil {
						return err
					}
					view.KeyEpoch = epoch
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), view)
				}
				printSpaceWrap(cmd, view)
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringArrayVar(&devices, "device", nil,
		"one of your devices to wrap the key for: its device identity id, device key or encryption key (repeatable)")
	cmd.Flags().BoolVar(&all, "all-devices", false, "wrap for every one of your active devices that lacks a copy of the current key")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list the devices that would be wrapped for, and wrap nothing")
	return cmd
}

// wrapTargets picks the devices to wrap for from the caller's own devices:
// every active one lacking a copy (all), or the named ones, each of which must
// be an own device that can be a wrap target. It also returns the named (or,
// with all, the other active) devices that already hold the current key.
func wrapTargets(own apiclient.OwnDevices, refs []string, all bool) (targets, held []apiclient.OwnDevice, err error) {
	if all {
		for _, d := range own.Devices {
			switch {
			case d.Status != apiclient.OwnDeviceActive:
			case d.HoldsCurrentKey:
				if !d.ThisDevice {
					held = append(held, d)
				}
			default:
				targets = append(targets, d)
			}
		}
		return targets, held, nil
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		d, ok := findOwnDevice(own.Devices, ref)
		if !ok {
			return nil, nil, fmt.Errorf("%s is not one of your devices enrolled at this controller; "+
				"'space wrap' wraps only for your own devices (share with another user through 'space members add')", ref)
		}
		if seen[d.ID] {
			continue
		}
		seen[d.ID] = true
		switch {
		case d.Status == apiclient.OwnDeviceRevoked:
			return nil, nil, fmt.Errorf("device %s (%s) is revoked; a space key is never wrapped for a revoked device", d.ID, d.Name)
		case d.Status == apiclient.OwnDeviceNoEncryptionKey:
			return nil, nil, fmt.Errorf("device %s (%s) was enrolled with no encryption key, so there is nothing to wrap the key for; "+
				"re-enrol it", d.ID, d.Name)
		case d.Status != apiclient.OwnDeviceActive:
			return nil, nil, fmt.Errorf("device %s (%s) is %s, not a wrap target", d.ID, d.Name, d.Status)
		case d.HoldsCurrentKey:
			held = append(held, d)
		default:
			targets = append(targets, d)
		}
	}
	return targets, held, nil
}

func findOwnDevice(devices []apiclient.OwnDevice, ref string) (apiclient.OwnDevice, bool) {
	for _, d := range devices {
		if ref == d.ID || ref == d.DeviceKey || (d.Recipient != "" && ref == d.Recipient) {
			return d, true
		}
	}
	return apiclient.OwnDevice{}, false
}

// wrapForDevices opens the space with this device's copy of the key, wraps the
// current key for each target and pushes the copies in one request. It
// returns the epoch they seal.
func wrapForDevices(ctx context.Context, c *apiclient.Client, configPath *string, deviceDir string, cmd *cobra.Command,
	spaceID string, targets []apiclient.OwnDevice,
) (int, error) {
	cust, err := selectCustody(ctx, configPath, deviceDir, cmd.ErrOrStderr())
	if err != nil {
		return 0, err
	}
	mgr, err := openSpace(ctx, c, cust, spaceID)
	if err != nil {
		return 0, err
	}
	wraps := make([]apiclient.WrappedKeyInput, 0, len(targets))
	var epoch int
	for _, d := range targets {
		r, err := client.ParseRecipient(d.Recipient)
		if err != nil {
			return 0, fmt.Errorf("device %s: %w", d.ID, err)
		}
		wrapped, e, err := mgr.WrapCurrent(spaceID, r)
		if err != nil {
			return 0, err
		}
		epoch = e
		wraps = append(wraps, apiclient.WrappedKeyInput{Recipient: wrapped.Recipient, Wrapped: wrapped.Wrapped, Epoch: e})
	}
	if err := c.RewrapKeys(ctx, spaceID, wraps); err != nil {
		return 0, wrapError(err)
	}
	return epoch, nil
}

func wrapDeviceViews(devices []apiclient.OwnDevice) []spaceWrapDeviceView {
	out := make([]spaceWrapDeviceView, 0, len(devices))
	for _, d := range devices {
		out = append(out, spaceWrapDeviceView{ID: d.ID, Name: d.Name, Recipient: d.Recipient})
	}
	return out
}

func printSpaceWrap(cmd *cobra.Command, v spaceWrapView) {
	w := cmd.OutOrStdout()
	verb := "wrapped"
	if v.DryRun {
		verb = "would wrap"
	}
	if len(v.Wrapped) == 0 {
		fmt.Fprintf(w, "space %s: nothing to wrap; no device of yours that can take a copy lacks the current key (epoch %d)\n", v.SpaceID, v.KeyEpoch)
	} else {
		fmt.Fprintf(w, "space %s: %s the key (epoch %d) for %d device(s):\n", v.SpaceID, verb, v.KeyEpoch, len(v.Wrapped))
		for _, d := range v.Wrapped {
			fmt.Fprintf(w, "    %s  %s\n", d.ID, d.Name)
		}
	}
	if len(v.AlreadyHeld) > 0 {
		fmt.Fprintln(w, "  already holding it:")
		for _, d := range v.AlreadyHeld {
			fmt.Fprintf(w, "    %s  %s\n", d.ID, d.Name)
		}
	}
}

// wrapError explains the refusals an operator can act on.
func wrapError(err error) error {
	var apiErr *apiclient.Error
	if !errors.As(err, &apiErr) || apiErr.Problem == nil {
		return err
	}
	switch apiErr.Problem.Code {
	case psapi.CodeKeyEpochStale, psapi.CodeKeyEpochAhead:
		return fmt.Errorf("space wrap: the space was re-keyed while wrapping, so no copy was recorded — run the command again: %w", err)
	case psapi.CodeWrapRecipientNotMember:
		return fmt.Errorf("space wrap: the controller refused the copy because the space's membership does not admit the device: %w", err)
	case psapi.CodeWrapRecipientNotAllowed:
		return fmt.Errorf("space wrap: the controller does not count the device as enrolled (enrol-before-wrap): %w", err)
	}
	return err
}
