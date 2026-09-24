package cli

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/rarebit-one/voidbind-go/device"
	"github.com/rarebit-one/voidbind-go/encryption"
	"github.com/rarebit-one/voidbind-go/enrolment"
	"github.com/rarebit-one/voidbind-go/pairflow"
	"github.com/rarebit-one/voidbind-go/pairing"
	vbrelay "github.com/rarebit-one/voidbind-go/relay"
	"github.com/rarebit-one/voidbind-go/useridentity"
	"github.com/spf13/cobra"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
)

// newPairCommand builds `heyarr pair` (§40, ADR-0022, ADR-0038, ADR-0066).
//
// Pairing is how a NEW device is admitted by one that can already vouch for the
// identity, without trusting the server (ADR-0022): the two exchange public keys
// through a DUMB relay, each computes a short authentication string over both
// keys and the invite's salt, the humans compare the two codes, and on a match
// the initiator signs a membership `add` op for the new device (ADR-0068). A
// man-in-the-middle that substitutes a key changes the code, and the
// commit-before-reveal ordering stops it choosing its key after seeing the
// peer's, so the short code is the whole gate.
//
// The handshake is voidbind-go's pairflow over voidbind-go's relay protocol, the
// same one the Voidbind apps speak, served by the node at httpapi.RelayV1Prefix.
//
// Like `heyarr device` and `heyarr identity`, these are CLIENT commands: they
// hold the person's keys, in the person's own config directory, and reach a
// running node only for the relay, which learns nothing and grants nothing.
func newPairCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Admit a new device from one that can already vouch for you (§40, ADR-0022)",
		Long: `Admit a NEW device from one that can already vouch for your identity, over a
dumb relay.

Run ` + "`heyarr pair authorise`" + ` where your user identity lives, or on a
device that is already a member. It opens a session on a running Heyarr's relay
and prints an invite. Run ` + "`heyarr pair enrol --invite <invite>`" + ` on the
NEW device. Each side prints a short code. Compare them, and if they match the
authorising side signs a membership op that admits the new device. The server
only relays public values and one sealed message. It learns no key material and
vouches for nothing (ADR-0038).`,
	}
	cmd.AddCommand(
		newPairAuthoriseCommand(opts),
		newPairEnrolCommand(opts),
		newPairSASCommand(opts),
	)
	return cmd
}

// pairCommonFlags are shared by authorise and enrol.
type pairCommonFlags struct {
	confirmSAS string
	yes        bool
	poll       time.Duration
	timeout    time.Duration
}

func (f *pairCommonFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.confirmSAS, "confirm-sas", "",
		"proceed only if the derived code equals this value — the scripted stand-in for a human comparison")
	cmd.Flags().BoolVar(&f.yes, "yes", false,
		"assume the codes matched, without prompting (use only when you compared them another way)")
	cmd.Flags().DurationVar(&f.poll, "poll", vbrelay.DefaultPollInterval,
		"how often to re-check the relay for the next handshake step")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 2*time.Minute,
		"how long to wait for the whole handshake before giving up")
}

// The --as values of `pair authorise`.
const (
	pairAsAuto     = "auto"
	pairAsIdentity = "identity"
	pairAsDevice   = "device"
)

func newPairAuthoriseCommand(_ Options) *cobra.Command {
	var (
		f           pairCommonFlags
		relayAddr   string
		as          string
		identityDir string
		deviceDir   string
		lifetime    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "authorise",
		Short: "Existing side: admit a new device by signing its membership op",
		Long: `Run this where your user identity lives, or on a device that is already a
member of your identity. It opens a session on the relay, prints an invite for
the new device, derives the short code, and, once you confirm the new device
shows the same code, signs a membership add op for the new device's keys and
hands it over sealed to the new device's encryption key (ADR-0068).

--as picks what signs:
  identity  your user identity (the genesis key), read from --identity-dir;
            the private key is used through a signer and never leaves its store
  device    this machine's device, which must already be a member
  auto      identity when one is present here, otherwise device (the default)

Either way, a local device enrolled under the same identity contributes the
membership ops it knows and records the new add afterwards.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ep, err := relayForNode(relayAddr)
			if err != nil {
				return err
			}
			salt, err := pairing.NewSalt()
			if err != nil {
				return err
			}
			in, record, err := buildInitiator(as, identityDir, deviceDir, salt, lifetime)
			if err != nil {
				return err
			}
			known := in.Ops()

			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			session, err := vbrelay.CreateSession(ctx, ep.httpc, ep.base)
			if err != nil {
				return fmt.Errorf("pair: opening a relay session: %w", err)
			}
			invite, err := pairflow.EncodeInvite(ep.invite, session, salt, in.UserID())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if in.Genesis() {
				fmt.Fprintf(out, "authorising as: user identity %s\n", in.UserID())
			} else {
				fmt.Fprintf(out, "authorising as: member device %s of user %s\n", in.DeviceID(), in.UserID())
			}
			fmt.Fprintf(out, "invite: %s\n", invite)
			fmt.Fprintf(out, "on the new device: heyarr pair enrol --invite '%s'\n\n", invite)

			t := ep.transport(session, pairflow.RoleInitiator, f.poll)
			sas, err := in.Handshake(ctx, t)
			if err != nil {
				return fmt.Errorf("pair: handshake: %w", err)
			}
			if err := confirmSAS(cmd, &f, sas); err != nil {
				return err
			}
			if err := in.Authorise(ctx, t); err != nil {
				return fmt.Errorf("pair: delivering the admission: %w", err)
			}
			if err := record(in.Ops()); err != nil {
				return fmt.Errorf("pair: the new device is admitted, but recording its op here failed: %w", err)
			}
			fmt.Fprintf(out, "\npaired: signed a membership op admitting the new device %s\n",
				admittedDevice(known, in.Ops()))
			return nil
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&relayAddr, "relay", "",
		"the running Heyarr's relay: a unix socket path, unix:///path, http://host:port or host:port")
	cmd.Flags().StringVar(&as, "as", pairAsAuto,
		"what signs the admission: identity, device, or auto (identity when present here)")
	cmd.Flags().StringVar(&identityDir, "identity-dir", "",
		"where your user identity lives (default: your config directory; "+useridentity.EnvDir+" overrides)")
	cmd.Flags().StringVar(&deviceDir, "device-dir", "",
		"where this machine's device key lives (default: your config directory; "+device.EnvDir+" overrides)")
	cmd.Flags().DurationVar(&lifetime, "lifetime", 0,
		"how long an admission signed as the identity is valid (default: the enrolment lifetime)")
	return cmd
}

// buildInitiator picks and builds the pairflow initiator for `pair authorise`,
// and returns how to record the op set afterwards.
//
// The identity initiator signs through useridentity.Store.Signer, so the genesis
// seed is read per signature and never held here. The device initiator is a
// member device: it signs with its own key and cites its own admitting op.
func buildInitiator(as, identityDir, deviceDir string, salt []byte, lifetime time.Duration,
) (*pairflow.Initiator, func([]string) error, error) {
	now := time.Now().UTC()
	devStore, err := openDeviceStore(deviceDir)
	if err != nil {
		return nil, nil, err
	}
	idStore, err := openUserIdentityStore(identityDir)
	if err != nil {
		return nil, nil, err
	}
	switch as {
	case pairAsAuto:
		as = pairAsDevice
		if _, err := idStore.Get(); err == nil {
			as = pairAsIdentity
		}
	case pairAsIdentity, pairAsDevice:
	default:
		return nil, nil, fmt.Errorf("pair: --as must be identity, device or auto, not %q", as)
	}

	if as == pairAsIdentity {
		id, err := idStore.Get()
		if err != nil {
			return nil, nil, err
		}
		signer, err := idStore.Signer()
		if err != nil {
			return nil, nil, err
		}
		// A local device enrolled under THIS identity is the replica of what the
		// identity knows: its ops let the genesis admission cite the current heads
		// (and re-admit a removed device), and it records the new add. A device of
		// another identity, or none, contributes nothing.
		var known []string
		record := func([]string) error { return nil }
		if dev, err := devStore.Get(""); err == nil && dev.EnrolledUser() == identity.FormatPublicKey(id.PublicKey) {
			if known, err = devStore.Ops(); err != nil {
				return nil, nil, err
			}
			record = devStore.RecordOps
		}
		in, err := pairflow.NewGenesisInitiatorWithSigner(signer, known, salt, now, lifetime)
		if err != nil {
			return nil, nil, err
		}
		return in, record, nil
	}

	dev, err := devStore.Get("")
	if err != nil {
		return nil, nil, fmt.Errorf("pair: this machine has no device to authorise from (%w); "+
			"run it where your user identity lives, or pass --as identity", err)
	}
	admitting, ok := dev.EnrolmentCert()
	if !ok {
		return nil, nil, fmt.Errorf("pair: device %s is not a member of any identity (%s), so it cannot admit another",
			dev.PublicKeyString(), dev.AuthorisationNote())
	}
	signer, err := devStore.LoadSigningKey()
	if err != nil {
		return nil, nil, err
	}
	known, err := devStore.Ops()
	if err != nil {
		return nil, nil, err
	}
	in, err := pairflow.NewDeviceInitiator(signer, dev.EncryptionKey, admitting, known, salt, now)
	if err != nil {
		return nil, nil, err
	}
	return in, devStore.RecordOps, nil
}

// admittedDevice names the device an Authorise just admitted: the add op that is
// in the initiator's op set now and was not before.
func admittedDevice(before, after []string) string {
	seen := make(map[string]bool, len(before))
	for _, tok := range before {
		seen[tok] = true
	}
	for _, tok := range after {
		if seen[tok] {
			continue
		}
		if op, err := enrolment.VerifyOp(tok); err == nil && op.Kind == enrolment.OpAdd {
			return op.Device
		}
	}
	return "(unknown)"
}

func newPairEnrolCommand(_ Options) *cobra.Command {
	var (
		f         pairCommonFlags
		invite    string
		relayAddr string
		deviceDir string
	)
	cmd := &cobra.Command{
		Use:   "enrol",
		Short: "New device: join through an invite and store the membership op",
		Long: `Run this on the NEW device with the invite ` + "`heyarr pair authorise`" + ` printed.
It generates (or reuses) this machine's device keys, contributes them to the
handshake, derives the short code, and, once you confirm the other side shows the
same code, receives the membership add op that admits this device, with the ops
that authorise it. It checks that the op admits THIS device into the invite's
identity, signed by the side it compared codes with, and stores both
(ADR-0068). Afterwards this device authenticates as your user.

--relay overrides the relay the invite names, for when this device reaches the
node by a different address. It takes the same forms as authorise's --relay.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(invite) == "" {
				return errors.New("pair enrol needs --invite, the voidbind:pair?... invite `heyarr pair authorise` printed")
			}
			inv, err := pairflow.DecodeInvite(strings.TrimSpace(invite))
			if err != nil {
				return err
			}
			var ep relayEndpoint
			if relayAddr != "" {
				ep, err = relayForNode(relayAddr)
			} else {
				ep, err = relayFromInvite(inv.RelayBase)
			}
			if err != nil {
				return err
			}
			devStore, err := openDeviceStore(deviceDir)
			if err != nil {
				return err
			}
			dev, err := devStore.Get("")
			if err != nil {
				if dev, err = devStore.Generate("", false); err != nil {
					return err
				}
			}
			if dev.EnrolmentStatus() == device.EnrolmentEnrolled && dev.EnrolledUser() != inv.User {
				return fmt.Errorf("pair: this device is already a member of %s; the invite is for %s",
					dev.EnrolledUser(), inv.User)
			}
			signPriv, err := devStore.LoadSigningKey()
			if err != nil {
				return err
			}
			encPriv, err := devStore.LoadEncryptionKey()
			if err != nil {
				return err
			}
			resp, err := pairflow.NewResponderWithKeys(inv.User, signPriv, encPriv, inv.Salt, time.Now().UTC())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "joining user: %s\nthis device:  %s\n\n", inv.User, resp.DeviceID())

			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			t := ep.transport(inv.Session, pairflow.RoleResponder, f.poll)
			sas, err := resp.Handshake(ctx, t)
			if err != nil {
				return fmt.Errorf("pair: handshake: %w", err)
			}
			if err := confirmSAS(cmd, &f, sas); err != nil {
				return err
			}
			enr, err := resp.Receive(ctx, t)
			if err != nil {
				return fmt.Errorf("pair: waiting for the admission (the other side may have refused the code): %w", err)
			}
			if _, err := devStore.EnrolWithOps(enr.Op, enr.Ops); err != nil {
				return err
			}
			fmt.Fprintf(out, "\nenrolled: this device is now a member of user %s (%d membership ops recorded)\n",
				inv.User, len(enr.Ops))
			return nil
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&invite, "invite", "",
		"the voidbind:pair?... invite printed by `heyarr pair authorise`")
	cmd.Flags().StringVar(&relayAddr, "relay", "",
		"reach the relay at this address instead of the invite's (a unix socket path, unix:///path, http://host:port or host:port)")
	cmd.Flags().StringVar(&deviceDir, "device-dir", "",
		"where this machine's device key lives (default: your config directory; "+device.EnvDir+" overrides)")
	return cmd
}

func newPairSASCommand(_ Options) *cobra.Command {
	var initiator, responder, salt, initiatorEnc, responderEnc string
	cmd := &cobra.Command{
		Use:   "sas",
		Short: "Compute the short authentication string for two keys and a salt",
		Long: `Derive the short authentication string (SAS) that binds two public keys and
a session salt — the same primitive the handshake compares. It is a utility for
scripts and for demonstrating that SUBSTITUTING a key changes the code: run it
with an honest responder key and again with a different one, and the two codes
differ, which is exactly why a man-in-the-middle is caught.

The v2 SAS also binds each device's X25519 ENCRYPTION key (§41, ADR-0049): pass
--responder-enc (and --initiator-enc) and substituting only the encryption key
changes the code too, so a relay that swaps the wrap-target key is caught.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			initPub, err := identity.ParsePublicKey(initiator)
			if err != nil {
				return fmt.Errorf("--initiator: %w", err)
			}
			respPub, err := identity.ParsePublicKey(responder)
			if err != nil {
				return fmt.Errorf("--responder: %w", err)
			}
			saltBytes, err := hex.DecodeString(strings.TrimSpace(salt))
			if err != nil {
				return fmt.Errorf("--salt is not hex: %w", err)
			}
			initEnc, err := parseOptionalEnc(initiatorEnc)
			if err != nil {
				return fmt.Errorf("--initiator-enc: %w", err)
			}
			respEnc, err := parseOptionalEnc(responderEnc)
			if err != nil {
				return fmt.Errorf("--responder-enc: %w", err)
			}
			sas, err := pairing.Derive(
				pairing.Keys{Sign: initPub, Enc: initEnc},
				pairing.Keys{Sign: respPub, Enc: respEnc},
				saltBytes)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), sas.String())
			return nil
		},
	}
	cmd.Flags().StringVar(&initiator, "initiator", "", "the initiator (user identity) public key, ed25519:<hex>")
	cmd.Flags().StringVar(&responder, "responder", "", "the responder (device) public key, ed25519:<hex>")
	cmd.Flags().StringVar(&salt, "salt", "", "the session salt, hex-encoded")
	cmd.Flags().StringVar(&initiatorEnc, "initiator-enc", "", "the initiator's X25519 encryption key, x25519:<hex> (optional)")
	cmd.Flags().StringVar(&responderEnc, "responder-enc", "", "the responder's X25519 encryption key, x25519:<hex> (optional)")
	_ = cmd.MarkFlagRequired("initiator")
	_ = cmd.MarkFlagRequired("responder")
	_ = cmd.MarkFlagRequired("salt")
	return cmd
}

// parseOptionalEnc parses an optional x25519:<hex> encryption key into its raw
// bytes, returning nil for an empty flag — a device that has no encryption key
// (or a caller demonstrating only the signing key) derives a v1-shaped SAS.
func parseOptionalEnc(s string) ([]byte, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	pub, err := encryption.ParsePublicKey(s)
	if err != nil {
		return nil, err
	}
	return pub.Bytes(), nil
}

// errSASRefused is a pairing whose codes were not confirmed: nothing was signed
// on the authorising side, and nothing is stored on the new one.
var errSASRefused = errors.New("pairing refused: the codes did not match, so no device was admitted")

// confirmSAS prints the derived code, then decides whether to proceed: against
// --confirm-sas when given (the scripted human), silently on --yes, or by
// prompting otherwise. A refusal is errSASRefused.
func confirmSAS(cmd *cobra.Command, f *pairCommonFlags, sas pairing.SAS) error {
	fmt.Fprintf(cmd.OutOrStdout(), "short authentication code: %s\n", sas.Grouped())
	ok := false
	switch {
	case f.confirmSAS != "":
		ok = normaliseSAS(f.confirmSAS) == sas.String()
	case f.yes:
		ok = true
	default:
		fmt.Fprintf(cmd.OutOrStdout(), "does the other device show the SAME code? [y/N]: ")
		line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if err != nil && err != io.EOF {
			return fmt.Errorf("reading your confirmation: %w", err)
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		ok = answer == "y" || answer == "yes"
	}
	if !ok {
		return errSASRefused
	}
	return nil
}

// normaliseSAS strips the cosmetic grouping space so "123 4567" and "1234567"
// compare equal against a derived, ungrouped SAS.
func normaliseSAS(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), " ", "")
}

// relayEndpoint is how this CLI reaches a node's Voidbind relay: the HTTP client
// (a unix-socket dialer, or plain TCP), the relay BASE voidbind-go's client
// appends "/v1/..." to, and the form of that address the invite carries.
type relayEndpoint struct {
	httpc  *http.Client
	base   string
	invite string
}

// unixRelayHost stands in for the host of a request that is dialled over a unix
// socket; the dialer ignores it.
const unixRelayHost = "http://relay.heyarr.invalid"

// relayForNode resolves a node address — a unix socket path, unix:///path,
// http(s)://host:port or a bare host:port — to the node's relay. The node serves
// the Voidbind relay under httpapi.RelayPrefix, so that is the base.
//
// A unix-socket relay is carried in the invite as unix:///abs/path, which only
// another `heyarr pair enrol` on the same machine can use; an HTTP relay is
// carried as its base URL, which any Voidbind client can.
func relayForNode(addr string) (relayEndpoint, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return relayEndpoint{}, errors.New("pair: --relay is required (a unix socket path, unix:///path, " +
			"http://host:port or host:port)")
	}
	transport := &http.Transport{MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	httpc := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	switch {
	case strings.HasPrefix(addr, "unix://"), strings.HasPrefix(addr, "/"), strings.HasPrefix(addr, "./"):
		socket, err := filepath.Abs(strings.TrimPrefix(addr, "unix://"))
		if err != nil {
			return relayEndpoint{}, fmt.Errorf("pair: resolving the relay socket: %w", err)
		}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
		return relayEndpoint{httpc: httpc, base: unixRelayHost + httpapi.RelayPrefix, invite: "unix://" + socket}, nil
	case strings.HasPrefix(addr, "http://"), strings.HasPrefix(addr, "https://"):
		addr = strings.TrimRight(addr, "/")
	default:
		addr = "http://" + strings.TrimRight(addr, "/")
	}
	base := strings.TrimSuffix(addr, httpapi.RelayPrefix) + httpapi.RelayPrefix
	return relayEndpoint{httpc: httpc, base: base, invite: base}, nil
}

// relayFromInvite resolves the relay an invite names: a unix:// socket (from a
// `heyarr pair authorise` on this machine) or an HTTP relay base, used as-is.
func relayFromInvite(relayBase string) (relayEndpoint, error) {
	if strings.HasPrefix(relayBase, "unix://") {
		return relayForNode(relayBase)
	}
	if !strings.HasPrefix(relayBase, "http://") && !strings.HasPrefix(relayBase, "https://") {
		return relayEndpoint{}, fmt.Errorf("pair: the invite's relay %q is not an http(s) or unix:// address", relayBase)
	}
	base := strings.TrimRight(relayBase, "/")
	return relayEndpoint{
		httpc:  &http.Client{Timeout: 30 * time.Second},
		base:   base,
		invite: base,
	}, nil
}

// transport binds the endpoint to one session and one role — the pairflow
// Transport voidbind-go's relay client implements.
func (e relayEndpoint) transport(session string, role pairflow.Role, poll time.Duration) *vbrelay.Client {
	return &vbrelay.Client{Base: e.base, Session: session, Role: string(role), HTTP: e.httpc, PollInterval: poll}
}
