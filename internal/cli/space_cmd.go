package cli

import (
	"context"
	"crypto/ecdh"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/encryption"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
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
		newSpaceCompactCommand(opts, configPath),
	)
	return cmd
}

// spaceCreateView is the --json shape of `space create`.
type spaceCreateView struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Recipients []string `json:"recipients"`
}

func newSpaceCreateCommand(opts Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags       clientFlags
		kind        string
		recipients  []string
		includeSelf bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint an encrypted space and wrap its key for the authorised devices",
		Long: `Mint a new encrypted space of a given kind (personal, family, shared, research)
and seal its key for each recipient — this device (unless --no-self) plus every
--recipient encryption key you name.

The space key is generated here and never leaves: the controller receives the
opaque space and the wrapped copies of the key, and can open none of them.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			recips, err := resolveRecipients(*deviceDir, recipients, includeSelf)
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
	return cmd
}

func newSpaceListCommand(opts Options, configPath *string) *cobra.Command {
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

func newSpaceKeysCommand(opts Options, configPath *string) *cobra.Command {
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

func newSpaceChangesCommand(opts Options, configPath *string) *cobra.Command {
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

func newSpacePutCommand(opts Options, configPath, deviceDir *string) *cobra.Command {
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
				mgr, err := openSpace(ctx, c, *deviceDir, spaceID)
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

func newSpaceReadCommand(opts Options, configPath, deviceDir *string) *cobra.Command {
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
				mgr, err := openSpace(ctx, c, *deviceDir, spaceID)
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

// loadDeviceEncKey opens this machine's device store and loads its X25519
// encryption private key — the key that unwraps a space key sealed for it.
func loadDeviceEncKey(deviceDir string) (*ecdh.PrivateKey, error) {
	ds, err := openDeviceStore(deviceDir)
	if err != nil {
		return nil, err
	}
	return ds.LoadEncryptionKey()
}

// resolveRecipients builds the wrap-target set for a new space: the named
// --recipient keys, plus this device's own encryption key when includeSelf is
// set (the default, so the creating device can read what it just made).
func resolveRecipients(deviceDir string, named []string, includeSelf bool) ([]client.Recipient, error) {
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
		priv, err := loadDeviceEncKey(deviceDir)
		if err != nil {
			return nil, fmt.Errorf("resolving this device as a recipient (pass --no-self if this device should not read the space): %w", err)
		}
		if err := add(encryption.FormatPublicKey(priv.PublicKey().Bytes())); err != nil {
			return nil, err
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

// openSpace fetches a space's wrapped keys, finds the one sealed for THIS device,
// unwraps it with the device's encryption key, and returns a manager holding the
// space key open. A device the space was not wrapped for is refused here, before
// any change is fetched — the confidentiality gate of ADR-0049.
func openSpace(ctx context.Context, c *apiclient.Client, deviceDir, spaceID string) (*client.Manager, error) {
	priv, err := loadDeviceEncKey(deviceDir)
	if err != nil {
		return nil, err
	}
	mine := encryption.FormatPublicKey(priv.PublicKey().Bytes())
	keys, err := c.WrappedKeys(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	var wrapped []byte
	for _, k := range keys {
		if k.Recipient == mine {
			wrapped = k.Wrapped
			break
		}
	}
	if wrapped == nil {
		return nil, fmt.Errorf("this device cannot read space %s: no copy of its key is wrapped for %s", spaceID, mine)
	}
	mgr := client.New()
	if err := mgr.Open(spaceID, wrapped, client.NewKeyUnwrapper(priv)); err != nil {
		return nil, err
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
				mgr, err := openSpace(ctx, c, *deviceDir, spaceID)
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
