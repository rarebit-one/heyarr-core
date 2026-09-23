package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	"github.com/rarebit-one/voidbind-go/hashing"
	"github.com/spf13/cobra"

	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/protocol"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultframe"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/vaultread"
)

// newVaultCommand builds `heyarr vault` — the media-vault client (ADR-0021,
// ADR-0095, ADR-0096, ADR-0097). It is the reference client for the vault: an
// admin-blind, cross-site personal-media store whose bytes the peer holds only as
// ciphertext it cannot open.
//
// Like `space` it is a HYBRID (see newSpaceCommand): it talks to a running
// controller over /api/v1 (--config) AND holds this machine's device key
// (--device-dir). The split is the whole point — the peer stores the opaque
// ciphertext blobs, the encrypted drive changes and the opaque placement pins, and
// this device alone holds the space key that seals a file into frames and opens
// them back. A file is pushed as: seal the plaintext into fixed ciphertext frames
// under the space key (vaultframe), content-address and upload the content blob and
// the sealed manifest (both self-pin on upload), then write the manifest's blob id
// at a path in the space's DRIVE CRDT (crdt.Drive) as one encrypted change. Pulling
// reverses it: materialise the drive, resolve the path to its manifest blob, and
// range-read + decrypt the content frames (vaultread).
func newVaultCommand(opts Options, configPath *string) *cobra.Command {
	var deviceDir string
	cmd := &cobra.Command{
		Use:   "vault",
		Short: "Push, pull and list files in an encrypted media vault (ADR-0021, ADR-0095)",
		Long: `Work with an encrypted media vault — a person's files as a path-addressed
drive whose bytes the peer holds only as ciphertext it cannot open.

A file is sealed into fixed ciphertext frames under the space key on THIS device
(never on the peer), content-addressed, and uploaded as opaque blobs; its manifest
blob id is then recorded at a vault path in the space's drive CRDT. Reading
reverses it entirely on the device. The peer stores ciphertext blobs, encrypted
drive changes and opaque placement pins, and can open none of it (Invariant 6).

Like the space commands these need both a running controller (--config) and this
machine's device key (--device-dir): the controller stores the ciphertext, the
device holds the only key that opens it.`,
	}
	cmd.PersistentFlags().StringVar(&deviceDir, "device-dir", "",
		"where this machine's device key lives (default: your config directory; "+device.EnvDir+" overrides)")

	cmd.AddCommand(
		newVaultPushCommand(opts, configPath, &deviceDir),
		newVaultPullCommand(opts, configPath, &deviceDir),
		newVaultLsCommand(opts, configPath, &deviceDir),
	)
	return cmd
}

// loadDrive materialises a space's vault DRIVE CRDT on this device: list the
// encrypted changes the peer holds, decrypt each under the space key into a
// crdt.DriveChange, and fold them into a fresh drive. It returns the drive and the
// changes (whose heads parent a new write). Unlike the playlist's `materialise`
// this takes no snapshot path — the drive's changes are replayed directly (the
// CRDT is idempotent and order-independent, so a full replay converges).
func loadDrive(ctx context.Context, c *apiclient.Client, mgr *client.Manager, spaceID string) (*crdt.Drive, []protocol.EncryptedChange, error) {
	changes, err := c.Changes(ctx, spaceID)
	if err != nil {
		return nil, nil, err
	}
	decoded, err := statesync.DecodeAllChanges[crdt.DriveChange](mgr, changes)
	if err != nil {
		return nil, nil, err
	}
	d := crdt.NewDrive()
	d.Apply(decoded...)
	return d, changes, nil
}

// blobFetcher adapts the API client to vaultread.BlobFetcher: Fetch reads a whole
// blob, FetchRange reads exactly [start, end). It backs both with the shared,
// honest GET /blobs/{hash}/content byte-range contract (blobs.go) — no vault-
// specific read route exists or is needed.
type blobFetcher struct{ c *apiclient.Client }

// Fetch returns a blob's whole bytes.
func (f blobFetcher) Fetch(ctx context.Context, blobID string) ([]byte, error) {
	bc, err := f.c.OpenBlobContent(ctx, blobID, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = bc.Body.Close() }()
	return io.ReadAll(bc.Body)
}

// FetchRange returns a blob's bytes in the half-open range [start, end), the
// convention vaultframe/vaultread use. It asks the read route to resume from
// start; if the peer ignored the range (a 200, not a 206) the body begins at
// bc.Offset, so it discards the gap before reading exactly end-start bytes rather
// than returning the wrong window.
func (f blobFetcher) FetchRange(ctx context.Context, blobID string, start, end int64) ([]byte, error) {
	if end < start {
		return nil, fmt.Errorf("vault: range end %d is before start %d", end, start)
	}
	bc, err := f.c.OpenBlobContent(ctx, blobID, start)
	if err != nil {
		return nil, err
	}
	defer func() { _ = bc.Body.Close() }()
	if skip := start - bc.Offset; skip > 0 {
		if _, err := io.CopyN(io.Discard, bc.Body, skip); err != nil {
			return nil, fmt.Errorf("vault: skipping to offset %d of %s: %w", start, blobID, err)
		}
	}
	buf := make([]byte, end-start)
	if _, err := io.ReadFull(bc.Body, buf); err != nil {
		return nil, fmt.Errorf("vault: reading [%d,%d) of %s: %w", start, end, blobID, err)
	}
	return buf, nil
}

// vaultPushView is the --json shape of `vault push`.
type vaultPushView struct {
	SpaceID      string `json:"space_id"`
	Path         string `json:"path"`
	ManifestBlob string `json:"manifest_blob"`
	ContentBlob  string `json:"content_blob"`
	Size         int64  `json:"size"`
	ChangeID     string `json:"change_id"`
}

func newVaultPushCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags     clientFlags
		spaceID   string
		vaultPath string
	)
	cmd := &cobra.Command{
		Use:   "push <file>",
		Short: "Seal a local file into the vault and record it at a vault path",
		Long: `Seal a local file into fixed ciphertext frames under the space key on THIS
device, upload the content blob and the sealed manifest (both self-pin on upload,
so they are retained), and record the manifest's blob id at --path in the space's
drive CRDT as one encrypted change.

The plaintext is never uploaded — the frames and the manifest are sealed here; the
peer stores ciphertext it cannot open.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				cust, err := selectCustody(configPath, *deviceDir)
				if err != nil {
					return err
				}
				mgr, err := openSpace(ctx, c, cust, spaceID)
				if err != nil {
					return err
				}
				sk, ok := mgr.SpaceKey(spaceID)
				if !ok {
					return fmt.Errorf("this device does not hold the key for space %s", spaceID)
				}

				f, err := os.Open(filePath) //nolint:gosec // a user-named file to seal is the whole point
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				info, err := f.Stat()
				if err != nil {
					return err
				}

				// Seal the plaintext into ciphertext frames, holding the content
				// blob in a buffer so it can be content-addressed and uploaded. The
				// manifest carries the content blob's id (its BLAKE3, over the
				// ciphertext) so a reader can fetch it.
				var content bytes.Buffer
				m, err := vaultframe.Seal(sk, f, &content)
				if err != nil {
					return err
				}
				if err := c.PutVaultBlob(ctx, m.Content, bytes.NewReader(content.Bytes())); err != nil {
					return err
				}

				// Seal the manifest and content-address it the same way, so a drive
				// entry can reference it as a canonical blake3 blob id.
				sealed, err := vaultframe.SealManifest(sk, m)
				if err != nil {
					return err
				}
				mh := hashing.New()
				_, _ = mh.Write(sealed)
				manifestID := mh.Sum().String()
				if err := c.PutVaultBlob(ctx, manifestID, bytes.NewReader(sealed)); err != nil {
					return err
				}

				// Record the manifest blob at the vault path in the drive CRDT, then
				// ship the write as one encrypted change parented on the current heads.
				drive, changes, err := loadDrive(ctx, c, mgr, spaceID)
				if err != nil {
					return err
				}
				change, err := drive.Put(vaultPath, manifestID, m.PlaintextSize, info.ModTime().Unix())
				if err != nil {
					return err
				}
				ec, err := statesync.EncodeChange(mgr, spaceID, protocol.Heads(changes), change)
				if err != nil {
					return err
				}
				id, err := c.PutChange(ctx, ec)
				if err != nil {
					return err
				}

				view := vaultPushView{
					SpaceID: spaceID, Path: vaultPath, ManifestBlob: manifestID,
					ContentBlob: m.Content, Size: m.PlaintextSize, ChangeID: id,
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), view)
				}
				fmt.Fprintf(cmd.OutOrStdout(),
					"pushed %s (%d bytes) to %s\n\n  manifest: %s\n  content:  %s\n  change:   %s\n",
					filePath, view.Size, view.Path, view.ManifestBlob, view.ContentBlob, view.ChangeID)
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&spaceID, "space", "", "the vault's space id (required)")
	cmd.Flags().StringVar(&vaultPath, "path", "", "the vault path to write the file at (required)")
	_ = cmd.MarkFlagRequired("space")
	_ = cmd.MarkFlagRequired("path")
	return cmd
}

func newVaultPullCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags   clientFlags
		outPath string
	)
	cmd := &cobra.Command{
		Use:   "pull <space-id> <vault-path>",
		Short: "Read a file from the vault, decrypting it on this device",
		Long: `Materialise the space's drive, resolve the vault path to its manifest blob,
and read the file back by range-fetching and decrypting only that manifest's
content frames — all on this device. Write it to -o, or to stdout.

A path that is absent, or one that currently has more than one live version (a
conflict), is refused rather than guessing which bytes were meant.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			spaceID, vaultPath := args[0], args[1]
			return flags.withClient(cmd, configPath, func(ctx context.Context, c *apiclient.Client) error {
				cust, err := selectCustody(configPath, *deviceDir)
				if err != nil {
					return err
				}
				mgr, err := openSpace(ctx, c, cust, spaceID)
				if err != nil {
					return err
				}
				sk, ok := mgr.SpaceKey(spaceID)
				if !ok {
					return fmt.Errorf("this device does not hold the key for space %s", spaceID)
				}
				drive, _, err := loadDrive(ctx, c, mgr, spaceID)
				if err != nil {
					return err
				}
				entry, ok := drive.Get(vaultPath)
				if !ok {
					return fmt.Errorf("no file at vault path %q in space %s", vaultPath, spaceID)
				}
				if entry.Conflicted {
					return fmt.Errorf("vault path %q has conflicting versions on this space — "+
						"resolve the conflict before pulling", vaultPath)
				}
				data, err := vaultread.ReadAll(ctx, blobFetcher{c: c}, sk, entry.Blob)
				if err != nil {
					return err
				}
				if outPath == "" || outPath == "-" {
					_, err := cmd.OutOrStdout().Write(data)
					return err
				}
				if err := os.WriteFile(outPath, data, 0o600); err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes to %s\n", len(data), outPath)
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVarP(&outPath, "out", "o", "", "write to this file instead of stdout")
	return cmd
}

// vaultEntryView is one live vault file in the --json shape of `vault ls`.
type vaultEntryView struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Conflicted bool   `json:"conflicted"`
}

func newVaultLsCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var flags clientFlags
	cmd := &cobra.Command{
		Use:   "ls <space-id>",
		Short: "List the live files in a vault",
		Long: `Materialise the space's drive on this device and list its live files — each
file's vault path, plaintext size, and whether the path is currently conflicted
(more than one live version).`,
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
				drive, _, err := loadDrive(ctx, c, mgr, spaceID)
				if err != nil {
					return err
				}
				entries := drive.List()
				if flags.asJSON {
					views := make([]vaultEntryView, 0, len(entries))
					for _, e := range entries {
						views = append(views, vaultEntryView{Path: e.Path, Size: e.Size, Conflicted: e.Conflicted})
					}
					return emitJSON(cmd.OutOrStdout(), views)
				}
				if len(entries) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "(empty vault)")
					return nil
				}
				t := newTable("PATH", "SIZE", "CONFLICTED")
				for _, e := range entries {
					conflicted := ""
					if e.Conflicted {
						conflicted = "conflicted"
					}
					t.add(e.Path, fmt.Sprintf("%d", e.Size), conflicted)
				}
				return t.render(cmd.OutOrStdout(), "(empty vault)")
			})
		},
	}
	flags.register(cmd)
	return cmd
}
