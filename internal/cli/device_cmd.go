package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/rarebit-one/void-which-binds-go/custody/sealedfile"
	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	heyarrdevice "github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/device/devicekeys"
	"github.com/rarebit-one/heyarr-core/internal/device/personalmcp"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/crdt"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/statesync"
)

// newDeviceCommand builds `heyarr device`.
//
// These commands are a CLIENT concern and share nothing with the rest of the
// tree. They take no --config, open no database and call no controller: the
// device key belongs to the person at the keyboard, and the server's data
// directory belongs to the service account. Reading the server's configuration
// here would be the first step towards putting the key in it (§38, §40,
// ADR-0032).
func newDeviceCommand(opts Options, configPath *string) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "device",
		Short: "Manage this machine's device key (§40, ADR-0032)",
		Long: `Manage the Ed25519 device key that identifies this machine as one of your
devices (spec §40).

The key is generated locally, stored 0600 in your own configuration directory,
and never sent anywhere. It is not the peer identity: that belongs to the
server and lives in its data directory.

It also does not authorise anything yet. Nothing is enrolled, nothing is
wrapped for it, and every grant against a Heyarr controller is still a bearer
token scope (ADR-0011) until Milestone 8. The key exists now so that Milestone
8 populates a shape rather than retrofitting one — see ADR-0032.`,
	}
	cmd.PersistentFlags().StringVar(&dir, "device-dir", "",
		"where this machine's device key lives (default: your config directory; "+device.EnvDir+" overrides)")

	cmd.AddCommand(
		newDeviceGenerateCommand(opts, &dir),
		newDeviceListCommand(opts, &dir),
		newDeviceShowCommand(opts, &dir),
		newDeviceRemoveCommand(opts, &dir),
		newDeviceRevokeCommand(opts, configPath, &dir),
		newDeviceSealTPMCommand(opts, configPath, &dir),
		newDevicePairOffloadCommand(opts, &dir),
		newDeviceMCPCommand(opts, &dir),
		newDeviceGatewayCommand(opts, &dir),
	)
	return cmd
}

// openDeviceStore resolves the device directory and opens the store with its
// keys wherever they are held (devicekeys.Open): seed files for a software
// device, the sealed file for a custody device (void-which-binds-go ADR-0021).
// It never asks for a passphrase; the first private operation does.
func openDeviceStore(dir string) (*device.Store, error) {
	return devicekeys.Open(devicekeys.Options{Dir: dir})
}

func newDeviceGenerateCommand(_ Options, dir *string) *cobra.Command {
	var (
		name           string
		force          bool
		asJSON         bool
		custodyKind    string
		passphraseFile string
	)
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate this machine's device key",
		Long: `Generate the Ed25519 signing key and X25519 encryption key that identify this
machine.

--custody decides where the private keys are held:

  software    (the default) two seed files in the device directory, mode 0600.
  sealedfile  one passphrase-sealed file, ` + devicekeys.SealedFileName + ` in the device directory
              (void-which-binds-go ADR-0021). The seeds are drawn in memory and
              sealed straight into it, so no seed is ever written in the clear.
              The passphrase is asked for twice on the terminal, without echo,
              or read from --passphrase-file (or ` + devicekeys.PassphraseFileEnvVar + `).
              It must be at least ` + strconv.Itoa(devicekeys.MinPassphraseLen) + ` characters.
              Every later command that signs or unwraps with this device asks
              for it again, once per command.

A sealed file protects the keys at rest — a stolen disk, a lost laptop, a backup
— as strongly as the passphrase does. It is not hardware: anyone with the file
can guess offline, slowed only by its Argon2id cost, and code running as you can
read the keys while they are unlocked.

No private key is ever printed, logged or returned by any command here — only
the public halves, as ed25519:<64 hex> and x25519:<64 hex>.

Regenerating replaces the keys, which is unrecoverable: space keys are wrapped
for a public key (§41), and a key that has been replaced cannot unwrap what the
old one could. So a second generate refuses unless you pass --force.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := devicekeys.ResolveDir(*dir)
			if err != nil {
				return err
			}
			// A plain store: whether a device already exists, and replacing one,
			// does not depend on unlocking it.
			store, err := device.NewStore(device.StoreOptions{Dir: resolved})
			if err != nil {
				return err
			}
			var dev device.Device
			switch custodyKind {
			case devicekeys.Software:
				if passphraseFile != "" {
					return errors.New("--passphrase-file is for --custody sealedfile; a software device has no passphrase")
				}
				if dev, err = store.Generate(name, force); err != nil {
					return err
				}
				// A custody device replaced with --force leaves no sealed
				// file behind. Without --force there was no device to
				// replace, and a sealed file with no record is left alone.
				if force {
					if err := devicekeys.RemoveSealed(resolved); err != nil {
						return err
					}
				}
			case devicekeys.SealedFile:
				dev, err = store.GenerateInto(name, force, &sealedfile.Provisioner{
					Path:      devicekeys.SealedPath(resolved),
					PIN:       devicekeys.NewPassphrase(passphraseFile, cmd.InOrStdin()),
					Overwrite: force,
				})
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("--custody must be %s or %s, not %q", devicekeys.Software, devicekeys.SealedFile, custodyKind)
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), device.NewView(dev, heyarrdevice.CommandHint))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "device key generated\n\n")
			printDevice(cmd.OutOrStdout(), dev)
			if dev.KeyCustody == device.KeyCustodyExternal {
				fmt.Fprintf(cmd.OutOrStdout(), "  sealed file  %s (mode %#o)\n", devicekeys.SealedPath(resolved), sealedfile.FileMode)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", caveat(dev))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "what to call this device (default: this machine's hostname)")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing key — unrecoverable")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	cmd.Flags().StringVar(&custodyKind, "custody", devicekeys.Software,
		"where the private keys are held: software (seed files) or sealedfile (a passphrase-sealed file)")
	cmd.Flags().StringVar(&passphraseFile, "passphrase-file", "",
		"with --custody sealedfile, read the passphrase from this file's first line (- for stdin) instead of the terminal")
	return cmd
}

func newDeviceListCommand(_ Options, dir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List this machine's device keys",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openDeviceStore(*dir)
			if err != nil {
				return err
			}
			devices, err := store.List()
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if asJSON {
				return emitJSON(w, device.NewViews(devices, heyarrdevice.CommandHint))
			}
			if len(devices) == 0 {
				fmt.Fprintln(w, "no device key on this machine — create one with `heyarr device generate`")
				return nil
			}
			fmt.Fprintf(w, "%-36s  %-20s  %-14s  %-10s  %s\n",
				"ID", "NAME", "ENROLMENT", "PROVEN", "PUBLIC KEY")
			for _, d := range devices {
				fmt.Fprintf(w, "%-36s  %-20s  %-14s  %-10s  %s\n",
					d.ID, d.Name, d.EnrolmentStatus(), provenWord(d), d.PublicKeyString())
			}
			fmt.Fprintf(w, "\n%s\n", caveat(devices[0]))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newDeviceShowCommand(_ Options, dir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show [id]",
		Short: "Show one device key",
		Long:  "Show one device record. With no id, the device on this machine.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openDeviceStore(*dir)
			if err != nil {
				return err
			}
			var id string
			if len(args) == 1 {
				id = args[0]
			}
			dev, err := store.Get(id)
			if err != nil {
				return err
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), device.NewView(dev, heyarrdevice.CommandHint))
			}
			printDevice(cmd.OutOrStdout(), dev)
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", caveat(dev))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newDeviceRemoveCommand(_ Options, dir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a device key",
		Long: `Delete a device key and its record from this machine — its seed files, or
the sealed file of a custody device.

There is no escrow and no copy: once removed, the key is gone. The id is
required and is matched exactly, because an unrecoverable command that accepts
"whatever is there" eventually runs against the wrong thing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A plain store, so a custody device whose sealed file is already
			// gone can still be removed.
			resolved, err := devicekeys.ResolveDir(*dir)
			if err != nil {
				return err
			}
			store, err := device.NewStore(device.StoreOptions{Dir: resolved})
			if err != nil {
				return err
			}
			dev, err := store.Remove(args[0])
			if err != nil {
				return err
			}
			// The device library leaves a custody device's sealed file to its
			// caller (ADR-0021); removing the device removes its keys too.
			if dev.KeyCustody == device.KeyCustodyExternal {
				if err := devicekeys.RemoveSealed(store.Dir()); err != nil {
					return err
				}
			}
			if asJSON {
				return emitJSON(cmd.OutOrStdout(), device.NewView(dev, heyarrdevice.CommandHint))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s (%s)\n", dev.ID, dev.Name)
			if dev.KeyCustody == device.KeyCustodyExternal {
				fmt.Fprintf(cmd.OutOrStdout(), "its sealed keys are gone from %s\n", devicekeys.SealedPath(store.Dir()))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "its private key is gone from %s\n", dev.KeyPath)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

// newDeviceMCPCommand runs the Personal MCP (§73) on this machine.
//
// Local stdio, and deliberately not a tool on the controller's MCP: §72 says
// controller-side MCP cannot decrypt user artifacts, and a controller tool that
// managed device keys would put the private key on the server. See ADR-0032.
func newDeviceMCPCommand(_ Options, dir *string) *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the Personal MCP for this machine's device key and personal state (§73)",
		Long: `Serve the Personal MCP over stdio, for an agent running on THIS machine.

This is not the Heyarr MCP. The Heyarr MCP is served by the controller and
covers the library, acquisition, peers and playback; it cannot see private
state and never will (§72). This one runs here and exposes the key-management
verbs this device can perform.

With --config it also exposes the READ tools over your encrypted personal state
(§73) — your playlists, starred items, listening history and reading positions:
it fetches the ciphertext from the controller, unwraps the space key with THIS
device's key, and decrypts and merges the matching CRDT locally — the controller
sees only ciphertext and can read none of it. Without --config it serves the
device-key tools alone.

It speaks newline-delimited JSON-RPC 2.0 on stdin and stdout, so configure your
agent to launch it as a command rather than to dial a URL. Nothing but protocol
messages goes to stdout.

On a device whose keys are held in a sealed file, the first read that unwraps
asks for the passphrase on the terminal (never on stdin, which carries the
protocol), or reads ` + devicekeys.PassphraseFileEnvVar + ` when it is set — which
an agent-launched server with no terminal needs.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openDeviceStore(*dir)
			if err != nil {
				return err
			}
			opts := personalmcp.Options{
				Store:   store,
				Version: buildinfo.Get().Version,
				Stdin:   cmd.InOrStdin(),
				Stdout:  cmd.OutOrStdout(),
			}
			// With a controller configured, wire the read-over-real-state tools.
			// The decrypt happens here, on the device; the controller only ever
			// serves ciphertext (§72, §73).
			if configPath != "" {
				var flags clientFlags
				c, err := flags.newClient(configPath)
				if err != nil {
					return err
				}
				cust, err := selectCustody(&configPath, *dir)
				if err != nil {
					return err
				}
				opts.PersonalState = personalStateReader{ctx: cmd.Context(), c: c, cust: cust}
			}
			srv, err := personalmcp.New(opts)
			if err != nil {
				return err
			}
			// Readiness on stderr, never stdout: on this transport a stray
			// line of prose on stdout is a protocol error.
			fmt.Fprintf(cmd.ErrOrStderr(), "heyarr personal mcp: serving %s over stdio (%d tools)\n",
				store.Dir(), len(srv.Names()))
			return srv.Serve(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "",
		"connect to this controller to expose the read tools over your encrypted personal state (§73)")
	return cmd
}

// personalStateReader is the device-side decrypt path behind the Personal MCP's
// read tools (§73): it opens a space by unwrapping the space key with this
// device's key, decrypts the changes the controller holds, and merges them into
// the playlist — all locally, so the controller only ever serves ciphertext.
type personalStateReader struct {
	ctx  context.Context
	c    *apiclient.Client
	cust client.Custody
}

func (r personalStateReader) Playlist(spaceID string) ([]string, error) {
	mgr, err := openSpace(r.ctx, r.c, r.cust, spaceID)
	if err != nil {
		return nil, err
	}
	changes, err := r.c.Changes(r.ctx, spaceID)
	if err != nil {
		return nil, err
	}
	decoded, err := statesync.DecodeAll(mgr, changes)
	if err != nil {
		return nil, err
	}
	st := crdt.New()
	st.Apply(decoded...)
	return st.IDs(), nil
}

// Starred opens the space, decrypts its changes as star/unstar operations, and
// folds them into the add-wins OR-Set — all on this device (§46, §72).
func (r personalStateReader) Starred(spaceID string) ([]string, error) {
	changes, err := decodeSpaceChanges[crdt.StarChange](r, spaceID)
	if err != nil {
		return nil, err
	}
	s := crdt.NewStarSet()
	s.Apply(changes...)
	return s.StarredIDs(), nil
}

// History opens the space, decrypts its changes as play events, and folds them
// into the grow-only play log, returning the recent/frequent/now-playing views
// a stock client asks for — all decrypted on this device (§46, §72).
func (r personalStateReader) History(spaceID string) (personalmcp.PlayHistory, error) {
	changes, err := decodeSpaceChanges[crdt.PlayChange](r, spaceID)
	if err != nil {
		return personalmcp.PlayHistory{}, err
	}
	log := crdt.NewPlayLog()
	log.Apply(changes...)

	var recent []string
	for _, e := range log.Recent() {
		recent = append(recent, e.ID)
	}
	var frequent []personalmcp.ItemCount
	for _, e := range log.Frequent() {
		frequent = append(frequent, personalmcp.ItemCount{ID: e.ID, Count: e.Count})
	}
	now, _ := log.NowPlaying()
	return personalmcp.PlayHistory{Recent: recent, Frequent: frequent, NowPlaying: now}, nil
}

// ReadingPositions opens the space, decrypts its writes as position updates, and
// folds them into the per-publication LWW register — all on this device (§45,
// §72).
func (r personalStateReader) ReadingPositions(spaceID string) ([]personalmcp.ReadingPosition, error) {
	changes, err := decodeSpaceChanges[crdt.PositionChange](r, spaceID)
	if err != nil {
		return nil, err
	}
	positions := crdt.NewReadingPositions()
	positions.Apply(changes...)
	var out []personalmcp.ReadingPosition
	for _, e := range positions.All() {
		out = append(out, personalmcp.ReadingPosition{PubID: e.PubID, Position: e.Position})
	}
	return out, nil
}

// decodeSpaceChanges is the shared device-side decrypt path for a typed CRDT
// read: open the space by unwrapping its key with this device's key, fetch the
// opaque changes the controller holds, and decrypt+decode them into CRDT changes
// of type T ready to fold. The controller only ever serves ciphertext.
func decodeSpaceChanges[T any](r personalStateReader, spaceID string) ([]T, error) {
	mgr, err := openSpace(r.ctx, r.c, r.cust, spaceID)
	if err != nil {
		return nil, err
	}
	changes, err := r.c.Changes(r.ctx, spaceID)
	if err != nil {
		return nil, err
	}
	return statesync.DecodeAllChanges[T](mgr, changes)
}

// printDevice renders one device for a person. The private key is represented
// by its path and its mode — the two facts an operator needs — and never by its
// contents.
func printDevice(w io.Writer, d device.Device) {
	fmt.Fprintf(w, "  id           %s\n", d.ID)
	fmt.Fprintf(w, "  name         %s\n", d.Name)
	fmt.Fprintf(w, "  algorithm    %s\n", d.Algorithm)
	fmt.Fprintf(w, "  public key   %s\n", d.PublicKeyString())
	fmt.Fprintf(w, "  created      %s\n", d.CreatedAt.UTC().Format(time.RFC3339))
	if d.KeyCustody == device.KeyCustodyExternal {
		fmt.Fprintf(w, "  private key  held in custody: the passphrase-sealed %s beside the record (never printed)\n",
			devicekeys.SealedFileName)
	} else {
		fmt.Fprintf(w, "  private key  %s (mode %#o, never printed)\n", d.KeyPath, device.KeyFileMode)
	}
	fmt.Fprintf(w, "  enrolment    %s\n", d.EnrolmentStatus())
	if u := d.EnrolledUser(); u != "" {
		fmt.Fprintf(w, "  enrolled as  %s\n", u)
	}
	fmt.Fprintf(w, "  proven       %s\n", provenWord(d))
}

// provenWord is the one-word form of Unproven, so the table column and the
// JSON field cannot drift apart.
func provenWord(d device.Device) string {
	if d.Unproven() {
		return "unproven"
	}
	return "proven"
}

// caveat is the honesty line, printed by every human-readable device command.
//
// It is here, at the edge, and not only in the domain, for the reason placement
// made `unproven` a required response field: a caveat that lives only in the
// domain is one the edge forgets. It reads the device so it reflects enrolment
// — the un-enrolled prefix stops being printed once the label comes off, which
// is the whole point of ADR-0032's revisit clause.
func caveat(d device.Device) string {
	if _, enrolled := d.EnrolmentCert(); enrolled {
		return "enrolled: " + d.AuthorisationNote(heyarrdevice.CommandHint) + "."
	}
	return "unproven: " + d.AuthorisationNote(heyarrdevice.CommandHint) + "."
}
