package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/device/personalmcp"
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
func newDeviceCommand(opts Options) *cobra.Command {
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
		newDeviceMCPCommand(opts, &dir),
		newDeviceGatewayCommand(opts, &dir),
	)
	return cmd
}

// openDeviceStore resolves the device directory and opens the store.
func openDeviceStore(dir string) (*device.Store, error) {
	if dir == "" {
		resolved, err := device.DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = resolved
	}
	return device.NewStore(device.StoreOptions{Dir: dir})
}

func newDeviceGenerateCommand(_ Options, dir *string) *cobra.Command {
	var (
		name   string
		force  bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate this machine's device key",
		Long: `Generate the Ed25519 keypair that identifies this machine.

The private key is written with mode 0600 and is never printed, logged or
returned by any command here — only its public half, as ed25519:<64 hex>.

Regenerating replaces the key, which is unrecoverable: Milestone 8 wraps space
keys for a public key (§41), and a key that has been replaced cannot unwrap
what the old one could. So a second generate refuses unless you pass --force.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openDeviceStore(*dir)
			if err != nil {
				return err
			}
			dev, err := store.Generate(name, force)
			if err != nil {
				return err
			}
			if asJSON {
				return encodeJSON(cmd.OutOrStdout(), device.NewView(dev, device.CommandHint))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "device key generated\n\n")
			printDevice(cmd.OutOrStdout(), dev)
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", caveat(dev))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "what to call this device (default: this machine's hostname)")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing key — unrecoverable")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
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
				return encodeJSON(w, device.NewViews(devices, device.CommandHint))
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
				return encodeJSON(cmd.OutOrStdout(), device.NewView(dev, device.CommandHint))
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
		Long: `Delete a device key and its record from this machine.

There is no escrow and no copy: once removed, the key is gone. The id is
required and is matched exactly, because an unrecoverable command that accepts
"whatever is there" eventually runs against the wrong thing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := openDeviceStore(*dir)
			if err != nil {
				return err
			}
			dev, err := store.Remove(args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return encodeJSON(cmd.OutOrStdout(), device.NewView(dev, device.CommandHint))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %s (%s)\n", dev.ID, dev.Name)
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
messages goes to stdout.`,
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
				opts.PersonalState = personalStateReader{ctx: cmd.Context(), c: c, deviceDir: *dir}
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
	ctx       context.Context
	c         *apiclient.Client
	deviceDir string
}

func (r personalStateReader) Playlist(spaceID string) ([]string, error) {
	mgr, err := openSpace(r.ctx, r.c, r.deviceDir, spaceID)
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
	mgr, err := openSpace(r.ctx, r.c, r.deviceDir, spaceID)
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
	fmt.Fprintf(w, "  private key  %s (mode %#o, never printed)\n", d.KeyPath, device.KeyFileMode)
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
		return "enrolled: " + d.AuthorisationNote(device.CommandHint) + "."
	}
	return "unproven: " + d.AuthorisationNote(device.CommandHint) + "."
}

func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
