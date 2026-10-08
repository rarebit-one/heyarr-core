package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/rarebit-one/void-which-binds-go/useridentity"
	"github.com/spf13/cobra"

	psapi "github.com/rarebit-one/heyarr-core/internal/api/personalstate"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaceopen"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

// newSpaceCommand builds `heyarr space` — the device side of encrypted personal
// state (§38, §42, ADR-0049).
//
// It is a HYBRID: like the client commands it talks to a running controller over
// /api/v1 (so it takes --config), and like the device commands it holds this
// machine's key material (so it takes --device-dir). The split is load-bearing:
// the space key is minted and every wrap and every decrypt happens HERE, on the
// device; the controller only ever receives the opaque space, the wrapped copies
// it cannot open, and the ciphertext changes it cannot read (Invariant 6).
func newSpaceCommand(opts Options, configPath *string) *cobra.Command {
	var deviceDir string
	cmd := &cobra.Command{
		Use:   "space",
		Short: "Create and read encrypted personal-state spaces (§38, §42, ADR-0049)",
		Long: `Work with encrypted personal-state spaces.

A space's key is minted on this device and sealed ("wrapped") separately for each
authorised device's encryption key. The controller stores the wrapped copies and
the encrypted changes and can read NONE of it: it holds ciphertext and opaque
causal metadata, never a playlist name, an item, or a key (spec §38).

These commands need both a running controller (--config, like every client
command) and this machine's device key (--device-dir, like the device commands):
the controller stores the ciphertext, the device holds the only key that opens
it.`,
	}
	cmd.PersistentFlags().StringVar(&deviceDir, "device-dir", "",
		"where this machine's device key lives (default: your config directory; "+device.EnvDir+" overrides)")

	cmd.AddCommand(
		newSpaceCreateCommand(opts, configPath, &deviceDir),
		newSpaceListCommand(opts, configPath),
		newSpaceKeysCommand(opts, configPath),
		newSpaceChangesCommand(opts, configPath),
		newSpacePutCommand(opts, configPath, &deviceDir),
		newSpaceReadCommand(opts, configPath, &deviceDir),
		newSpaceSnapshotCommand(opts, configPath, &deviceDir),
		newSpaceRotateCommand(opts, configPath, &deviceDir),
		newSpaceCompactCommand(opts, configPath),
		newSpaceRecoverCommand(opts, configPath, &deviceDir),
		newSpaceExportRecoveryCommand(opts, configPath),
		newSpaceRewrapCommand(opts, configPath, &deviceDir),
	)
	return cmd
}

// spaceCreateView is the --json shape of `space create`.
type spaceCreateView struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Recipients []string `json:"recipients"`
}

func newSpaceCreateCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags           clientFlags
		kind            string
		recipients      []string
		includeSelf     bool
		includeRecovery bool
		identityDir     string
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint an encrypted space and wrap its key for the authorised devices",
		Long: `Mint a new encrypted space of a given kind (personal, family, shared, research)
and seal its key for each recipient — this device (unless --no-self), your
recovery key (unless --recovery=false), plus every --recipient encryption key you
name.

The space key is generated here and never leaves: the controller receives the
opaque space and the wrapped copies of the key, and can open none of them.

By default the space is also wrapped for your user identity's RECOVERY key, so the
space key survives the loss of every device — your recovery secret alone
regenerates it (§79, #360). This is silently skipped if you have no user identity
yet; run ` + "`heyarr identity generate`" + ` to enable it, or pass --recovery=false.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			recoveryID, err := resolveRecoveryRecipient(cmd, identityDir, includeRecovery)
			if err != nil {
				return err
			}
			var selfID string
			if includeSelf {
				cust, err := selectCustody(configPath, *deviceDir)
				if err != nil {
					return err
				}
				selfID = cust.RecipientID()
			}
			recips, err := resolveRecipients(selfID, recipients, includeSelf, recoveryID)
			if err != nil {
				return err
			}
			mgr := client.New()
			sp, wrapped, err := mgr.Create(spaces.Kind(kind), time.Now().UTC(), recips)
			if err != nil {
				return err
			}
			req := apiclient.CreateSpaceRequest{ID: sp.ID, Kind: string(sp.Kind)}
			for _, w := range wrapped {
				req.WrappedKeys = append(req.WrappedKeys, apiclient.WrappedKeyInput{
					Recipient: w.Recipient, Wrapped: w.Wrapped,
				})
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				created, err := c.CreateSpace(ctx, req)
				if err != nil {
					return err
				}
				ids := make([]string, 0, len(recips))
				for _, r := range recips {
					ids = append(ids, r.ID)
				}
				sort.Strings(ids)
				if recoveryID != "" {
					fmt.Fprintln(cmd.ErrOrStderr(), staleBlobHint)
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), spaceCreateView{ID: created.ID, Kind: created.Kind, Recipients: ids})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "space created\n\n  id:    %s\n  kind:  %s\n  wrapped for %d recipient(s):\n",
					created.ID, created.Kind, len(ids))
				for _, id := range ids {
					fmt.Fprintf(cmd.OutOrStdout(), "    %s\n", id)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&kind, "kind", string(spaces.KindPersonal), "space kind: personal, family, shared, or research")
	cmd.Flags().StringArrayVar(&recipients, "recipient", nil, "an authorised device's encryption key (x25519:<hex>); repeatable")
	cmd.Flags().BoolVar(&includeSelf, "self", true, "also wrap the key for this device, so it can read the space")
	cmd.Flags().BoolVar(&includeRecovery, "recovery", true, "also wrap the key for your user identity's recovery key, so it survives losing every device (#360)")
	cmd.Flags().StringVar(&identityDir, "identity-dir", "",
		"where your user identity lives (default: your config directory; "+useridentity.EnvDir+" overrides)")
	return cmd
}

// resolveRecoveryRecipient returns the recovery encryption key id ("x25519:<hex>")
// a new space should be wrapped for by default (#360), or "" when there is none to
// wrap for. When includeRecovery is false it returns "" without touching the
// identity store. A missing identity, or one that predates recovery-wrap, is a
// NOTE on stderr and an empty id — never a hard failure — so a device with no user
// identity can still create spaces; but if the user EXPLICITLY asked for --recovery
// and there is nothing to satisfy it, that is an error, not a silent skip.
func resolveRecoveryRecipient(cmd *cobra.Command, identityDir string, includeRecovery bool) (string, error) {
	if !includeRecovery {
		return "", nil
	}
	id, err := recoveryRecipientID(identityDir)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, useridentity.ErrNoIdentity):
		if cmd.Flags().Changed("recovery") {
			return "", fmt.Errorf("--recovery was requested but this machine has no user identity to recover to — "+
				"run `heyarr identity generate` first, or pass --recovery=false: %w", err)
		}
		fmt.Fprintln(cmd.ErrOrStderr(), "note: no user identity found — this space will NOT be wrapped for a recovery key. "+
			"Run `heyarr identity generate` (then recovery-wrap is automatic), or pass --recovery=false to silence this.")
		return "", nil
	case errors.Is(err, errNoRecoveryKey):
		fmt.Fprintln(cmd.ErrOrStderr(), "note: your user identity predates recovery-wrap and has no recovery encryption key — "+
			"this space will NOT be wrapped for recovery. Regenerate your identity to enable it, or pass --recovery=false.")
		return "", nil
	default:
		return "", err
	}
}

// errNoRecoveryKey marks a user identity that exists but carries no recovery
// encryption key — a record written before #360. It is a skip, not a failure.
var errNoRecoveryKey = errors.New("user identity has no recovery encryption key")

// recoveryRecipientID opens the user identity store and returns its recovery
// encryption key id. It returns useridentity.ErrNoIdentity if there is no identity
// here, and errNoRecoveryKey if the identity predates recovery-wrap.
func recoveryRecipientID(identityDir string) (string, error) {
	store, err := openUserIdentityStore(identityDir)
	if err != nil {
		return "", err
	}
	id, err := store.Get()
	if err != nil {
		return "", err
	}
	if id.EncryptionKey == "" {
		return "", errNoRecoveryKey
	}
	return id.EncryptionKey, nil
}

func newSpaceListCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the encrypted spaces the controller holds (metadata only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				list, err := c.ListSpaces(ctx)
				if err != nil {
					return err
				}
				if flags.asJSON {
					if list == nil {
						list = []apiclient.Space{}
					}
					return emitJSON(cmd.OutOrStdout(), list)
				}
				if len(list) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "no spaces")
					return nil
				}
				for _, sp := range list {
					fmt.Fprintf(cmd.OutOrStdout(), "%s  %-8s  %s\n", sp.ID, sp.Kind, sp.CreatedAt)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

func newSpaceKeysCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "keys <space-id>",
		Short: "List the wrapped copies of a space's key (recipients only, no key material)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				keys, err := c.WrappedKeys(ctx, args[0])
				if err != nil {
					return err
				}
				if flags.asJSON {
					if keys == nil {
						keys = []apiclient.WrappedKey{}
					}
					return emitJSON(cmd.OutOrStdout(), keys)
				}
				for _, k := range keys {
					fmt.Fprintf(cmd.OutOrStdout(), "%s  (%d bytes wrapped)  %s\n", k.Recipient, len(k.Wrapped), k.CreatedAt)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

// spaceChangeView is one stored change as the peer holds it — ciphertext and all.
// It is what proves the at-rest bytes are opaque: Ciphertext is base64 of the
// encrypted change, and the plaintext item never appears in it.
type spaceChangeView struct {
	ChangeID   string   `json:"change_id"`
	Parents    []string `json:"parents,omitempty"`
	Ciphertext []byte   `json:"ciphertext"`
}

func newSpaceChangesCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "changes <space-id>",
		Short: "List a space's stored changes AS THE PEER HOLDS THEM — ciphertext",
		Long: `List the encrypted changes the controller stores for a space.

This is the peer's-eye view: every change is ciphertext and opaque causal
metadata. Nothing here is decrypted — that is the whole point (§38). Use
'space read' on an authorised device to see the actual contents.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				changes, err := c.Changes(ctx, args[0])
				if err != nil {
					return err
				}
				views := make([]spaceChangeView, 0, len(changes))
				for _, ch := range changes {
					views = append(views, spaceChangeView{ChangeID: ch.ChangeID, Parents: ch.Parents, Ciphertext: ch.Ciphertext})
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), views)
				}
				for _, v := range views {
					fmt.Fprintf(cmd.OutOrStdout(), "%s  (%d bytes ciphertext)\n", v.ChangeID, len(v.Ciphertext))
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

func newSpacePutCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags clientFlags
		item  string
	)
	cmd := &cobra.Command{
		Use:   "put <space-id>",
		Short: "Add an item to a space's playlist (encrypted client-side, then pushed)",
		Long: `Add an item to the space's playlist CRDT.

The item is merged into the current state, encrypted under the space key on this
device, and pushed as one opaque change. The controller stores ciphertext; it
never sees the item.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spaceID := args[0]
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				cust, err := selectCustody(configPath, *deviceDir)
				if err != nil {
					return err
				}
				mgr, err := openSpace(ctx, c, cust, spaceID)
				if err != nil {
					return err
				}
				// Read-modify-write: reconstruct the CRDT (and its Lamport clock)
				// from the snapshot plus the changes the peer holds, add the item,
				// and ship the new change parented on the current heads. Using the
				// snapshot keeps this correct after the log has been compacted.
				st, existing, snap, err := materialise(ctx, c, mgr, spaceID)
				if err != nil {
					return err
				}
				change := st.Add(item)
				ec, err := statesync.Encode(mgr, spaceID, currentHeads(existing, snap), change)
				if err != nil {
					return err
				}
				id, err := c.PutChange(ctx, ec)
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), spaceChangeView{ChangeID: id, Parents: ec.Parents, Ciphertext: ec.Ciphertext})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "added %q — change %s (%d bytes ciphertext)\n", item, id, len(ec.Ciphertext))
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&item, "item", "", "the item to add to the playlist (required)")
	_ = cmd.MarkFlagRequired("item")
	return cmd
}

// spaceReadView is the --json shape of `space read`: the converged playlist.
type spaceReadView struct {
	SpaceID string   `json:"space_id"`
	Items   []string `json:"items"`
}

func newSpaceReadCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "read <space-id>",
		Short: "Read a space's playlist on an authorised device (decrypts and merges locally)",
		Long: `Read the converged playlist for a space.

Only a device the space was wrapped for can do this: it unwraps the space key
with its own encryption key, decrypts every change, and merges them into the
current playlist — all locally. A device the space was NOT wrapped for cannot,
and is refused.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spaceID := args[0]
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				cust, err := selectCustody(configPath, *deviceDir)
				if err != nil {
					return err
				}
				mgr, err := openSpace(ctx, c, cust, spaceID)
				if err != nil {
					return err
				}
				// Materialise from the snapshot (if any) plus the changes the peer
				// holds — so a device reaches the state from a bounded snapshot +
				// tail after the log has been compacted, not just from a full log.
				st, _, _, err := materialise(ctx, c, mgr, spaceID)
				if err != nil {
					return err
				}
				items := st.IDs()
				if flags.asJSON {
					if items == nil {
						items = []string{}
					}
					return emitJSON(cmd.OutOrStdout(), spaceReadView{SpaceID: spaceID, Items: items})
				}
				if len(items) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "(empty playlist)")
					return nil
				}
				for i, id := range items {
					fmt.Fprintf(cmd.OutOrStdout(), "%2d. %s\n", i+1, id)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

// deviceRecipient is this machine's device encryption key as a wrap target,
// "x25519:<hex>", read from the device record. Wrapping for the device needs
// only its public key, so this asks for no passphrase and reads no seed, and it
// serves a software and a custody device alike (void-which-binds-go ADR-0021).
func deviceRecipient(deviceDir string) (string, error) {
	ds, err := openDeviceStore(deviceDir)
	if err != nil {
		return "", err
	}
	dev, err := ds.Get("")
	if err != nil {
		return "", err
	}
	if len(dev.EncryptionKey) == 0 {
		return "", fmt.Errorf("device %s has no encryption key, so it is not a wrap target — regenerate it", dev.ID)
	}
	return dev.EncryptionKeyString(), nil
}

// resolveRecipients builds the wrap-target set for a new space: this device's own
// encryption key when includeSelf is set (the default, so the creating device can
// read what it just made), the recovery key when recoveryID is non-empty (#360),
// and the named --recipient keys. Duplicates across the three sources collapse.
// selfID is this device's wrap-target id (the selected custody backend's
// RecipientID) — so a space created on a machine configured for the YubiKey
// backend is wrapped to the card, matching what open will later unwrap with. It
// is required only when includeSelf.
func resolveRecipients(selfID string, named []string, includeSelf bool, recoveryID string) ([]client.Recipient, error) {
	seen := make(map[string]bool)
	var out []client.Recipient
	add := func(id string) error {
		if seen[id] {
			return nil
		}
		r, err := client.ParseRecipient(id)
		if err != nil {
			return err
		}
		seen[id] = true
		out = append(out, r)
		return nil
	}
	if includeSelf {
		if selfID == "" {
			return nil, fmt.Errorf("resolving this device as a recipient: no custody key (pass --no-self if this device should not read the space)")
		}
		if err := add(selfID); err != nil {
			return nil, err
		}
	}
	if recoveryID != "" {
		if err := add(recoveryID); err != nil {
			return nil, fmt.Errorf("wrapping the space for your recovery key: %w", err)
		}
	}
	for _, id := range named {
		if err := add(id); err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("a space needs at least one recipient — name one with --recipient, or keep --self")
	}
	return out, nil
}

// openSpace fetches a space's wrapped keys and key epoch, finds the copy sealed
// for THIS device (the selected custody backend's wrap-target), unwraps it
// through that backend, unrolls the key history (ADR-0103), and returns a
// manager holding the space's keyring open. A device the space was not wrapped
// for is refused here, before any change is fetched — the confidentiality gate
// of ADR-0049. The custody backend (ADR-0098) is chosen by config; opening takes
// a client.Custody, not a raw key, so a YubiKey/TPM/offloaded key drops in
// unchanged.
func openSpace(ctx context.Context, c *apiclient.Client, cust client.Custody, spaceID string) (*client.Manager, error) {
	mgr, found, err := spaceopen.Open(ctx, c, cust, spaceID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("this device cannot read space %s: no copy of its key is wrapped for %s", spaceID, cust.RecipientID())
	}
	return mgr, nil
}

// newSpaceSnapshotCommand builds `heyarr space snapshot` — materialise the current
// state on this device, encrypt it, and push it as a bounded checkpoint (§44).
func newSpaceSnapshotCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "snapshot <space-id>",
		Short: "Take an encrypted snapshot at the current causal point (§44)",
		Long: `Materialise the space's current state on this device, encrypt it under the
space key, and push it as a snapshot. A snapshot lets a fresh or long-offline
device reach the state from the snapshot plus the tail of changes after it,
rather than replaying the whole log. The server stores ciphertext; it never
materialises the state.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spaceID := args[0]
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				cust, err := selectCustody(configPath, *deviceDir)
				if err != nil {
					return err
				}
				mgr, err := openSpace(ctx, c, cust, spaceID)
				if err != nil {
					return err
				}
				st, changes, _, err := materialise(ctx, c, mgr, spaceID)
				if err != nil {
					return err
				}
				snap, err := statesync.EncodeSnapshot(mgr, spaceID, protocol.Heads(changes), st)
				if err != nil {
					return err
				}
				id, err := c.PushSnapshot(ctx, snap)
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), map[string]any{"snapshot_id": id, "frontier": snap.Frontier})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "snapshot %s taken over %d heads (%d bytes ciphertext)\n", id, len(snap.Frontier), len(snap.Ciphertext))
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

// spaceRotateView is the --json shape of `space rotate`.
type spaceRotateView struct {
	SpaceID   string   `json:"space_id"`
	Revoked   []string `json:"revoked"`
	Remaining []string `json:"remaining"`
	// KeyEpoch is the space's key epoch after the rotation (ADR-0103).
	KeyEpoch int `json:"key_epoch"`
	// SnapshotID and Dropped report the playlist carried forward for clients
	// without a keyring (see rotateSpace); absent for a pure re-key.
	SnapshotID string `json:"snapshot_id,omitempty"`
	Dropped    int    `json:"dropped,omitempty"`
}

// newSpaceRotateCommand builds `heyarr space rotate` — revoke recipients from a
// space by re-keying it (§41, ADR-0049, ADR-0103, #361).
func newSpaceRotateCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags  clientFlags
		revoke []string
	)
	cmd := &cobra.Command{
		Use:   "rotate <space-id> --revoke <recipient>",
		Short: "Revoke recipients from a space by rotating its key (§41, #361)",
		Long: `Revoke one or more recipients from an encrypted space.

Rotation mints a FRESH space key and moves the space to its next key epoch. The
new key is wrapped for every REMAINING recipient (your recovery key included),
and the previous key is sealed under the new one as an opaque history row, so a
remaining device still reads everything written before the rotation. Nothing is
re-encrypted, snapshotted or compacted. The controller drops every copy of the
old key in the same step, so a revoked recipient never receives the new key and
can read nothing written from here on. It keeps whatever it could already read —
revocation is forward-looking, not retroactive.

This device must itself be a current recipient (only a device that can read a
space may re-key it), and at least one recipient must remain. Two rotations
racing from the same epoch cannot both land: the second is refused, and is
simply run again. The same goes for a recipient added to or removed from the
space while the rotation runs: the rotation is refused rather than dropping the
new recipient or re-admitting the removed one, and is simply run again.

Only playlist spaces can be rotated for now. A vault drive, starred,
play-history or reading-position space is refused (#698): older clients do not
yet understand key epochs and would lose access to it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spaceID := args[0]
			if len(revoke) == 0 {
				return fmt.Errorf("name at least one recipient to revoke with --revoke")
			}
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				cust, err := selectCustody(configPath, *deviceDir)
				if err != nil {
					return err
				}
				view, err := rotateSpace(ctx, c, cust, spaceID, revoke)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.ErrOrStderr(), staleBlobHint)
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), view)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "space %s re-keyed to epoch %d\n\n  revoked %d recipient(s):\n", spaceID, view.KeyEpoch, len(view.Revoked))
				for _, r := range view.Revoked {
					fmt.Fprintf(cmd.OutOrStdout(), "    %s\n", r)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  re-wrapped for %d remaining recipient(s)\n", len(view.Remaining))
				if view.SnapshotID != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "  playlist carried forward as snapshot %s; %d old change(s) compacted\n", view.SnapshotID, view.Dropped)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringArrayVar(&revoke, "revoke", nil, "a recipient (x25519:<hex>) to revoke; repeatable")
	return cmd
}

// rotateSpace is the whole of `space rotate` (§41, ADR-0049, #361) as a
// function, so that revoking a device (`device revoke`, ADR-0068) can re-key
// each space that device could read without a second copy of the sequence. It
// refuses a non-playlist space (ensurePlaylistSpace) and then re-keys it
// (rekeySpace). This device must itself be a current recipient of the space.
func rotateSpace(ctx context.Context, c *apiclient.Client, cust client.Custody, spaceID string, revoke []string) (spaceRotateView, error) {
	mgr, err := openSpace(ctx, c, cust, spaceID)
	if err != nil {
		return spaceRotateView{}, err
	}
	if err := ensurePlaylistSpace(ctx, c, mgr, spaceID); err != nil {
		return spaceRotateView{}, err
	}
	st, changes, _, err := materialise(ctx, c, mgr, spaceID)
	if err != nil {
		return spaceRotateView{}, err
	}
	view, err := rekeyOpenSpace(ctx, c, mgr, spaceID, revoke)
	if err != nil {
		return spaceRotateView{}, err
	}
	// Compatibility for clients without a keyring (heyarr-kmp before its
	// ADR-0103 release): they hold only the newest key, so they read a playlist
	// only from a snapshot under that key with the old log compacted, as
	// rotation worked before ADR-0103. Carry the playlist forward that way.
	// The re-key has already landed, so a failure here leaves a correct space
	// that only keyring clients can read in full; it is reported, not undone.
	// This goes when the guard does.
	snap, err := statesync.EncodeSnapshot(mgr, spaceID, protocol.Heads(changes), st)
	if err != nil {
		return view, fmt.Errorf("space rotate: re-keyed to epoch %d, but carrying the playlist forward failed: %w", view.KeyEpoch, err)
	}
	if view.SnapshotID, err = c.PushSnapshot(ctx, snap); err != nil {
		return view, fmt.Errorf("space rotate: re-keyed to epoch %d, but pushing the playlist snapshot failed: %w", view.KeyEpoch, err)
	}
	if view.Dropped, err = c.Compact(ctx, spaceID, snap.Frontier); err != nil {
		return view, fmt.Errorf("space rotate: re-keyed to epoch %d, but compacting the old log failed: %w", view.KeyEpoch, err)
	}
	return view, nil
}

// rekeySpace is a rotation with no space-kind guard: open the space with its
// key history, then re-key it (ADR-0103). It is what rotateSpace does once the
// guard passes, and is safe for every kind of space in this binary; the guard
// stays only for clients that predate key epochs.
func rekeySpace(ctx context.Context, c *apiclient.Client, cust client.Custody, spaceID string, revoke []string) (spaceRotateView, error) {
	mgr, err := openSpace(ctx, c, cust, spaceID)
	if err != nil {
		return spaceRotateView{}, err
	}
	return rekeyOpenSpace(ctx, c, mgr, spaceID, revoke)
}

// errKeyEpochConflict is a rotation that lost a race: another rotation moved
// the space to a new epoch first.
var errKeyEpochConflict = errors.New("space rotate: the space was re-keyed by someone else since it was opened")

// errRecipientsChanged is a rotation that lost a race to a recipient change: a
// recipient was added to or removed from the space between this device reading
// the recipients and the rotation landing (#703). Rotating anyway would either
// lock the new recipient out or hand the removed one the new key, so the
// controller refuses it.
var errRecipientsChanged = errors.New("space rotate: the space's recipients changed since it was opened")

// rekeyOpenSpace is the pure re-key of ADR-0103 on a space already open on mgr:
// mint the next key, wrap it for every recipient but the revoked, seal the
// current key under it, and hand all of it to the controller in ONE
// compare-and-swap on the epoch and the recipient set. The controller drops
// every older wrap — the revoked recipients' included — in the same
// transaction, so there is no separate revoke step to fail half way, and
// nothing is materialised, snapshotted or compacted.
func rekeyOpenSpace(ctx context.Context, c *apiclient.Client, mgr *client.Manager, spaceID string, revoke []string) (spaceRotateView, error) {
	p, err := prepareRekey(ctx, c, mgr, spaceID, revoke)
	if err != nil {
		return spaceRotateView{}, err
	}
	return p.commit(ctx, c)
}

// A pendingRekey is a rotation built against the recipient set as read, not
// yet handed to the controller. The gap between the two is the race the
// controller's compare-and-swap closes (#703).
type pendingRekey struct {
	spaceID   string
	expected  int
	rot       client.Rotation
	remaining []string
	revoked   []string
}

// prepareRekey reads the space's current recipients, partitions them into the
// kept and the revoked, and mints and wraps the next key for the kept.
func prepareRekey(ctx context.Context, c *apiclient.Client, mgr *client.Manager, spaceID string, revoke []string) (pendingRekey, error) {
	expected, ok := mgr.Epoch(spaceID)
	if !ok {
		return pendingRekey{}, fmt.Errorf("%w: %s", client.ErrSpaceNotOpen, spaceID)
	}
	// The recipients are read fresh, not from when the space was opened. The
	// rotation names both the kept (its wraps) and the revoked, so if the set
	// or the epoch moves before it lands, the controller refuses it below and a
	// recipient list from the wrong moment never lands.
	keys, err := c.WrappedKeys(ctx, spaceID)
	if err != nil {
		return pendingRekey{}, err
	}
	remaining, revoked, err := partitionRecipients(keys, revoke)
	if err != nil {
		return pendingRekey{}, err
	}
	rot, err := mgr.Rotate(spaceID, remaining)
	if err != nil {
		return pendingRekey{}, err
	}
	return pendingRekey{
		spaceID: spaceID, expected: expected, rot: rot,
		remaining: recipientIDs(remaining), revoked: revoked,
	}, nil
}

// commit hands the prepared rotation to the controller.
func (p pendingRekey) commit(ctx context.Context, c *apiclient.Client) (spaceRotateView, error) {
	inputs := make([]apiclient.WrappedKeyInput, 0, len(p.rot.Wrapped))
	for _, w := range p.rot.Wrapped {
		inputs = append(inputs, apiclient.WrappedKeyInput{Recipient: w.Recipient, Wrapped: w.Wrapped})
	}
	epoch, err := c.RotateKey(ctx, p.spaceID, p.expected, p.rot.SealedPrev, inputs, p.revoked)
	if err != nil {
		var apiErr *apiclient.Error
		if errors.As(err, &apiErr) && apiErr.Problem != nil {
			switch apiErr.Problem.Code {
			case psapi.CodeKeyEpochConflict:
				return spaceRotateView{}, fmt.Errorf("%w (it was at epoch %d): %w — run the rotation again", errKeyEpochConflict, p.expected, err)
			case psapi.CodeRotationRecipientsChanged:
				return spaceRotateView{}, fmt.Errorf("%w: %w — run the rotation again", errRecipientsChanged, err)
			}
		}
		return spaceRotateView{}, err
	}
	if epoch != p.rot.Epoch {
		return spaceRotateView{}, fmt.Errorf("space rotate: the controller moved space %s to epoch %d, not %d", p.spaceID, epoch, p.rot.Epoch)
	}
	return spaceRotateView{
		SpaceID: p.spaceID, Revoked: p.revoked, Remaining: p.remaining, KeyEpoch: epoch,
	}, nil
}

// errRotateNotPlaylist refuses to re-key a space that does not hold a playlist.
var errRotateNotPlaylist = errors.New("space rotate: this space holds state rotation cannot preserve")

// ensurePlaylistSpace refuses rotation for any space that is not a playlist
// space (#698). This binary's rotation is a pure re-key with a key history
// (ADR-0103), so it no longer loses anything itself. The guard stays for client
// COMPATIBILITY: a client that predates key epochs opens a rotated space with
// only its newest key, and so cannot read a vault file, a starred, play-history
// or reading-position change sealed before the rotation. A playlist survives
// that, because the older clients' rotation left a snapshot under the new key.
// The guard lifts once every client (the mobile one included) unrolls the
// history. Until then only a space whose every change, and whose snapshot if it
// has one, strictly decodes as a playlist may be rotated. An empty space has
// nothing to lose.
func ensurePlaylistSpace(ctx context.Context, c *apiclient.Client, mgr *client.Manager, spaceID string) error {
	refuse := func(what string) error {
		return fmt.Errorf("%w: space %s has a %s that is not a playlist (a vault drive, starred, "+
			"play-history or reading-position space); clients that predate key epochs would lose its contents (#698)",
			errRotateNotPlaylist, spaceID, what)
	}
	changes, err := c.Changes(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, ec := range changes {
		if err := ec.Validate(); err != nil {
			return fmt.Errorf("space rotate: refusing a change: %w", err)
		}
		raw, err := mgr.Decrypt(spaceID, ec.Ciphertext)
		if err != nil {
			return err
		}
		if !crdt.IsPlaylistChange(raw) {
			return refuse("change")
		}
	}
	snap, ok, err := c.Snapshot(ctx, spaceID)
	if err != nil {
		return err
	}
	if ok {
		raw, _, err := statesync.OpenSnapshot(mgr, snap)
		if err != nil {
			return err
		}
		if !crdt.IsPlaylistSnapshot(raw) {
			return refuse("snapshot")
		}
	}
	return nil
}

// partitionRecipients splits a space's current wrapped-key recipients into those
// that REMAIN (parsed, for re-wrapping under the new key) and those to revoke.
// Every --revoke target must currently be a recipient (a typo that would revoke
// nobody is an error, not a silent no-op), and at least one recipient must remain.
func partitionRecipients(keys []apiclient.WrappedKey, revoke []string) (remaining []client.Recipient, revoked []string, err error) {
	revokeSet := make(map[string]bool, len(revoke))
	for _, r := range revoke {
		revokeSet[r] = true
	}
	current := make(map[string]bool, len(keys))
	for _, k := range keys {
		current[k.Recipient] = true
		if revokeSet[k.Recipient] {
			continue
		}
		r, perr := client.ParseRecipient(k.Recipient)
		if perr != nil {
			return nil, nil, fmt.Errorf("parsing remaining recipient %q: %w", k.Recipient, perr)
		}
		remaining = append(remaining, r)
	}
	for r := range revokeSet {
		if !current[r] {
			return nil, nil, fmt.Errorf("%q is not a current recipient of this space — nothing to revoke", r)
		}
		revoked = append(revoked, r)
	}
	if len(remaining) == 0 {
		return nil, nil, fmt.Errorf("refusing to revoke every recipient — the space would be readable by no one")
	}
	sort.Strings(revoked)
	return remaining, revoked, nil
}

// recipientIDs renders a recipient slice as sorted ids for stable output.
func recipientIDs(rs []client.Recipient) []string {
	ids := make([]string, 0, len(rs))
	for _, r := range rs {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// newSpaceCompactCommand builds `heyarr space compact` — drop the changes the
// latest snapshot subsumes and every replica holds, bounding the log (§44).
func newSpaceCompactCommand(_ Options, configPath *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "compact <space-id>",
		Short: "Drop the changes the latest snapshot subsumes (§44)",
		Long: `Compact the space's change log: drop the changes the latest snapshot subsumes
and that every replica already holds, so a long-lived space is not an unbounded
log. The snapshot is what makes it safe — the dropped changes are recoverable
from it — and only changes within the acknowledged frontier are ever dropped, so
a change a partitioned peer still needs survives.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spaceID := args[0]
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				snap, ok, err := c.Snapshot(ctx, spaceID)
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("space %s has no snapshot to compact against — take one first with `space snapshot`", spaceID)
				}
				dropped, err := c.Compact(ctx, spaceID, snap.Frontier)
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), map[string]any{"dropped": dropped})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "compacted %d change(s) the snapshot subsumes\n", dropped)
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

// materialise reconstructs a space's CRDT state from its latest snapshot (if any)
// plus every change the peer still holds. It is correct whether or not the log has
// been compacted: a snapshot folds the changes it drops, and applying a change
// already folded in is a no-op (the CRDT is idempotent). It returns the state, the
// changes the peer holds, and the latest snapshot (nil if none).
func materialise(ctx context.Context, c *apiclient.Client, mgr *client.Manager, spaceID string) (*crdt.State, []protocol.EncryptedChange, *protocol.EncryptedSnapshot, error) {
	st := crdt.New()
	var snapPtr *protocol.EncryptedSnapshot
	if snap, ok, err := c.Snapshot(ctx, spaceID); err != nil {
		return nil, nil, nil, err
	} else if ok {
		base, err := statesync.DecodeSnapshot(mgr, snap)
		if err != nil {
			return nil, nil, nil, err
		}
		st = base
		snapPtr = &snap
	}
	changes, err := c.Changes(ctx, spaceID)
	if err != nil {
		return nil, nil, nil, err
	}
	decoded, err := statesync.DecodeAll(mgr, changes)
	if err != nil {
		return nil, nil, nil, err
	}
	st.Apply(decoded...)
	return st, changes, snapPtr, nil
}

// currentHeads is the space's causal heads for parenting a new change: the heads
// of the changes the peer holds, or the snapshot's frontier when the log has been
// compacted to nothing.
func currentHeads(changes []protocol.EncryptedChange, snap *protocol.EncryptedSnapshot) []string {
	if h := protocol.Heads(changes); len(h) > 0 {
		return h
	}
	if snap != nil {
		return snap.Frontier
	}
	return nil
}
