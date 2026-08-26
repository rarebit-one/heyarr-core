package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/useridentity"
)

// newIdentityCommand builds `heyarr identity`.
//
// Like `heyarr device`, these are a CLIENT concern: they manage the key of the
// person at the keyboard, in that person's own config directory, and take no
// --config, open no database and call no controller. The user identity is the
// root of authority (§40, ADR-0048) — its private key signs the enrolment certs
// that vouch for this person's device keys, and it never enters the server's
// data directory (ADR-0032).
//
// The pin at a peer is a separate, admin-mediated act: an operator posts the
// public key printed here to `POST /api/v1/identities/users` at each peer. That
// out-of-band step is the ADR-0032 gate — a key the device could assert about
// itself would be "issued and immediately honoured", which must stay unspellable.
func newIdentityCommand(opts Options) *cobra.Command {
	var (
		identityDir string
		deviceDir   string
	)
	cmd := &cobra.Command{
		Use:   "identity",
		Short: "Manage your user identity and enrol this machine's device (§40, ADR-0048)",
		Long: `Manage the Ed25519 user identity that is the root of your authority (spec §40).

Your user identity signs the enrolment certificate that says "this device key is
mine". A device authenticates as you by presenting that cert; the cert authorises
nothing on its own (ADR-0048). The private key is generated locally, stored 0600
in your own configuration directory, and never sent anywhere — an operator pins
only its PUBLIC half at each peer, out of band, which is what makes enrolment a
deliberate human act rather than something a device can claim about itself
(ADR-0032).`,
	}
	cmd.PersistentFlags().StringVar(&identityDir, "identity-dir", "",
		"where your user identity lives (default: your config directory; "+useridentity.EnvDir+" overrides)")
	cmd.PersistentFlags().StringVar(&deviceDir, "device-dir", "",
		"where this machine's device key lives (default: your config directory; "+device.EnvDir+" overrides)")

	cmd.AddCommand(
		newIdentityGenerateCommand(opts, &identityDir),
		newIdentityShowCommand(opts, &identityDir),
		newIdentityEnrolCommand(opts, &identityDir, &deviceDir),
		newIdentityCredentialCommand(opts, &deviceDir),
	)
	return cmd
}

// openUserIdentityStore resolves the identity directory and opens the store.
func openUserIdentityStore(dir string) (*useridentity.Store, error) {
	if dir == "" {
		resolved, err := useridentity.DefaultDir()
		if err != nil {
			return nil, err
		}
		dir = resolved
	}
	return useridentity.NewStore(useridentity.StoreOptions{Dir: dir})
}

func newIdentityGenerateCommand(_ Options, dir *string) *cobra.Command {
	var (
		name   string
		force  bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Generate your user identity keypair",
		Long: `Generate the Ed25519 keypair that is the root of your authority.

The private key is written with mode 0600 and is never printed, logged or
returned by any command here — only its public half, as ed25519:<64 hex>, which
is what an operator pins at a peer.

Regenerating replaces the identity, which is unrecoverable: every device this
identity enrolled verifies against its public key, so a replaced key invalidates
them all. A second generate refuses unless you pass --force.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openUserIdentityStore(*dir)
			if err != nil {
				return err
			}
			id, err := store.Generate(name, force)
			if err != nil {
				return err
			}
			if asJSON {
				return encodeJSON(cmd.OutOrStdout(), useridentity.NewView(id))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "user identity generated\n\n")
			printIdentity(cmd.OutOrStdout(), id)
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", identityPinHint(id))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "what to call this identity (default: derived from this machine's hostname)")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing identity — unrecoverable")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newIdentityShowCommand(_ Options, dir *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show your user identity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openUserIdentityStore(*dir)
			if err != nil {
				return err
			}
			id, err := store.Get()
			if err != nil {
				return err
			}
			if asJSON {
				return encodeJSON(cmd.OutOrStdout(), useridentity.NewView(id))
			}
			printIdentity(cmd.OutOrStdout(), id)
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", identityPinHint(id))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newIdentityEnrolCommand(_ Options, identityDir, deviceDir *string) *cobra.Command {
	var (
		lifetime time.Duration
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "enrol",
		Short: "Sign an enrolment cert binding this machine's device to your identity",
		Long: `Sign a user-signed enrolment certificate for this machine's device key and
store it beside the device key.

This is the client half of enrolment: your user identity vouches for this
device. The device then authenticates as you by presenting the cert. It stays
LOCAL — nothing is sent to a server here. An operator still pins your user
public key at each peer (out of band) before any peer will honour the cert; a
cert signed by a key no peer has pinned is refused there (ADR-0032).

Signing takes the private key of your user identity, so run it where that
identity lives. It reads the local device key (generate one with
` + "`heyarr device generate`" + ` first).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			idStore, err := openUserIdentityStore(*identityDir)
			if err != nil {
				return err
			}
			devStore, err := openDeviceStore(*deviceDir)
			if err != nil {
				return err
			}
			dev, err := devStore.Get("")
			if err != nil {
				return err
			}
			// Bind the device's encryption key (§41) as well as its signing key, so
			// the cert authenticates the device AND names what space keys are
			// wrapped for. A pre-Milestone-9 device with no encryption key passes
			// "", minting the v1-shaped binding.
			cert, err := idStore.SignCert(dev.PublicKey, dev.EncryptionKeyString(), lifetime)
			if err != nil {
				return err
			}
			enrolled, err := devStore.Enrol(cert)
			if err != nil {
				return err
			}
			if asJSON {
				return encodeJSON(cmd.OutOrStdout(), device.NewView(enrolled))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "device enrolled\n\n")
			printDevice(cmd.OutOrStdout(), enrolled)
			fmt.Fprintf(cmd.OutOrStdout(), "\n%s\n", caveat(enrolled))
			return nil
		},
	}
	cmd.Flags().DurationVar(&lifetime, "lifetime", 0,
		"how long the cert is valid (default: the 90-day enrolment lifetime)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func newIdentityCredentialCommand(_ Options, deviceDir *string) *cobra.Command {
	var header bool
	cmd := &cobra.Command{
		Use:   "credential",
		Short: "Print an Authorization credential for this enrolled device",
		Long: `Print the value this device presents to authenticate as your user.

It is the held enrolment cert joined to a FRESH possession proof, signed here
with the device private key — so it proves both that a user vouched for this
device AND that this caller holds the device key (ADR-0048). The proof is
short-lived, so mint one per request rather than caching it.

With --header it prints the whole ` + "`Authorization: Device …`" + ` header,
ready to pass to a client. Without it, just the credential value.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := openDeviceStore(*deviceDir)
			if err != nil {
				return err
			}
			cred, err := store.Credential(time.Now(), 0)
			if err != nil {
				return err
			}
			if header {
				fmt.Fprintf(cmd.OutOrStdout(), "Authorization: Device %s\n", cred)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), cred)
			return nil
		},
	}
	cmd.Flags().BoolVar(&header, "header", false, "print the whole Authorization header line, not just the value")
	return cmd
}

// printIdentity renders one user identity for a person. The private key is
// represented by its path and mode, never its contents.
func printIdentity(w io.Writer, id useridentity.Identity) {
	fmt.Fprintf(w, "  id           %s\n", id.ID)
	fmt.Fprintf(w, "  name         %s\n", id.Name)
	fmt.Fprintf(w, "  algorithm    %s\n", id.Algorithm)
	fmt.Fprintf(w, "  public key   %s\n", id.PublicKeyString())
	fmt.Fprintf(w, "  created      %s\n", id.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "  private key  %s (mode %#o, never printed)\n", id.KeyPath, useridentity.KeyFileMode)
}

// identityPinHint tells the operator the one out-of-band step enrolment needs:
// pin this public key at each peer. It is the ADR-0032 gate spelled out at the
// edge, where the person who has to do it will see it.
func identityPinHint(id useridentity.Identity) string {
	return "to trust this identity, an operator pins its public key at each peer:\n" +
		"  heyarr token create … (an admin token), then\n" +
		"  POST /api/v1/identities/users {\"public_key\":\"" + id.PublicKeyString() + "\"}\n" +
		"until then, a cert this identity signs is refused at the peer (ADR-0032)."
}
