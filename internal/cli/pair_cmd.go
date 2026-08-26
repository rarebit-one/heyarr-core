package cli

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/device"
	"github.com/rarebit-one/heyarr-core/internal/pairflow"
	"github.com/rarebit-one/heyarr-core/internal/pairing"
	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/useridentity"
)

// newPairCommand builds `heyarr pair` (§40, ADR-0022, ADR-0038).
//
// Pairing is how a NEW device is enrolled by an OLD one without trusting the
// server (ADR-0022): the two exchange public keys and a salt through a DUMB
// relay, each computes a short authentication string over both keys, the humans
// compare the two codes, and on a match the old device signs an enrolment cert
// for the new device's key. A man-in-the-middle that substitutes a key changes
// the code, and the commit-before-reveal ordering (internal/pairflow) stops it
// choosing its key after seeing the peer's, so the short code is the whole gate.
//
// Like `heyarr device` and `heyarr identity`, these are CLIENT commands: they
// hold the person's keys, in the person's own config directory, and reach a
// running node only for the relay, which learns nothing and grants nothing.
func newPairCommand(opts Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Authorise a new device from an already-enrolled one (§40, ADR-0022)",
		Long: `Enrol a NEW device by an OLD, already-enrolled one, over a dumb relay.

Run ` + "`heyarr pair authorise`" + ` on the OLD device (the one that holds your
user identity) and ` + "`heyarr pair enrol`" + ` on the NEW device, pointing both
at the same running Heyarr's relay and the same session id. Each prints a short
code; compare them, and if they match the old device signs a cert the new device
stores. The server only relays public values — it learns no key material and
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
	relay      string
	session    string
	confirmSAS string
	yes        bool
	poll       time.Duration
	timeout    time.Duration
}

func (f *pairCommonFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.relay, "relay", "",
		"the running Heyarr's relay: a unix socket path, unix:///path, http://host:port or host:port")
	cmd.Flags().StringVar(&f.session, "session", "",
		"the rendezvous session id both devices share (authorise generates one if empty)")
	cmd.Flags().StringVar(&f.confirmSAS, "confirm-sas", "",
		"proceed only if the derived code equals this value — the scripted stand-in for a human comparison")
	cmd.Flags().BoolVar(&f.yes, "yes", false,
		"assume the codes matched, without prompting (use only when you compared them another way)")
	cmd.Flags().DurationVar(&f.poll, "poll", pairflow.DefaultPollInterval,
		"how often to re-check the relay for the next handshake step")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 2*time.Minute,
		"how long to wait for the whole handshake before giving up")
}

func newPairAuthoriseCommand(_ Options) *cobra.Command {
	var (
		f           pairCommonFlags
		identityDir string
		lifetime    time.Duration
	)
	cmd := &cobra.Command{
		Use:   "authorise",
		Short: "Old device: authorise a new device and sign its enrolment cert",
		Long: `Run this on an already-enrolled device. It contributes your USER identity
public key to the handshake, derives the short code, and — once you confirm the
new device shows the same code — signs an enrolment cert for the new device's
key. Signing needs your user identity private key, so run it where that identity
lives.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			idStore, err := openUserIdentityStore(identityDir)
			if err != nil {
				return err
			}
			id, err := idStore.Get()
			if err != nil {
				return err
			}
			relay, err := newRelayHTTP(f.relay)
			if err != nil {
				return err
			}
			session := f.session
			if session == "" {
				session = randomSession()
			}
			salt, err := pairing.NewSalt()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pairing session: %s\n", session)
			fmt.Fprintf(cmd.OutOrStdout(), "on the new device: heyarr pair enrol --relay <relay> --session %s\n\n", session)

			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			res, err := pairflow.Initiator{
				Relay: relay, Session: session, PollInterval: f.poll,
				UserPub: id.PublicKey, Salt: salt,
				Confirm: confirmFunc(cmd, &f),
				Sign: func(devPub ed25519.PublicKey) (string, error) {
					return idStore.SignCert(devPub, lifetime)
				},
			}.Run(ctx)
			return reportPairResult(cmd, "authorise", res, err)
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&identityDir, "identity-dir", "",
		"where your user identity lives (default: your config directory; "+useridentity.EnvDir+" overrides)")
	cmd.Flags().DurationVar(&lifetime, "lifetime", 0,
		"how long the signed cert is valid (default: the 90-day enrolment lifetime)")
	return cmd
}

func newPairEnrolCommand(_ Options) *cobra.Command {
	var (
		f         pairCommonFlags
		deviceDir string
	)
	cmd := &cobra.Command{
		Use:   "enrol",
		Short: "New device: pair with an old device and store the enrolment cert",
		Long: `Run this on the NEW device. It generates (or reuses) this machine's device
key, contributes it to the handshake, derives the short code, and — once you
confirm the old device shows the same code — receives and stores the enrolment
cert the old device signs. Afterwards this device authenticates as your user.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.session == "" {
				return errors.New("pair enrol needs --session, the id shown by `heyarr pair authorise`")
			}
			devStore, err := openDeviceStore(deviceDir)
			if err != nil {
				return err
			}
			dev, err := devStore.Get("")
			if err != nil {
				dev, err = devStore.Generate("", false)
				if err != nil {
					return err
				}
			}
			relay, err := newRelayHTTP(f.relay)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), f.timeout)
			defer cancel()
			res, err := pairflow.Responder{
				Relay: relay, Session: f.session, PollInterval: f.poll,
				DevicePub: dev.PublicKey,
				Confirm:   confirmFunc(cmd, &f),
				Accept: func(cert string) error {
					_, err := devStore.Enrol(cert)
					return err
				},
			}.Run(ctx)
			return reportPairResult(cmd, "enrol", res, err)
		},
	}
	f.register(cmd)
	cmd.Flags().StringVar(&deviceDir, "device-dir", "",
		"where this machine's device key lives (default: your config directory; "+device.EnvDir+" overrides)")
	return cmd
}

func newPairSASCommand(_ Options) *cobra.Command {
	var initiator, responder, salt string
	cmd := &cobra.Command{
		Use:   "sas",
		Short: "Compute the short authentication string for two keys and a salt",
		Long: `Derive the short authentication string (SAS) that binds two public keys and
a session salt — the same primitive the handshake compares. It is a utility for
scripts and for demonstrating that SUBSTITUTING a key changes the code: run it
with an honest responder key and again with a different one, and the two codes
differ, which is exactly why a man-in-the-middle is caught.`,
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
			// Encryption keys are empty pending the commit-reveal follow-up that
			// folds them into the flow (§41) — a v1-shaped SAS from the v2 primitive.
			sas, err := pairing.Derive(pairing.Keys{Sign: initPub}, pairing.Keys{Sign: respPub}, saltBytes)
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
	_ = cmd.MarkFlagRequired("initiator")
	_ = cmd.MarkFlagRequired("responder")
	_ = cmd.MarkFlagRequired("salt")
	return cmd
}

// confirmFunc builds the SAS comparison callback from the flags. It prints the
// derived code, then decides whether to proceed: against --confirm-sas when
// given (the scripted human), silently on --yes, or by prompting otherwise.
func confirmFunc(cmd *cobra.Command, f *pairCommonFlags) func(pairing.SAS) (bool, error) {
	return func(sas pairing.SAS) (bool, error) {
		fmt.Fprintf(cmd.OutOrStdout(), "short authentication code: %s\n", sas.Grouped())
		if f.confirmSAS != "" {
			want := normaliseSAS(f.confirmSAS)
			return want == string(sas), nil
		}
		if f.yes {
			return true, nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "does the other device show the SAME code? [y/N]: ")
		reader := bufio.NewReader(cmd.InOrStdin())
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return false, fmt.Errorf("reading your confirmation: %w", err)
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes", nil
	}
}

// reportPairResult renders the outcome of a handshake and maps it to an exit code.
func reportPairResult(cmd *cobra.Command, role string, res pairflow.Result, err error) error {
	if err != nil {
		switch {
		case errors.Is(err, pairflow.ErrSASRefused):
			return fmt.Errorf("pairing refused: the codes did not match, so no device was enrolled")
		case errors.Is(err, pairflow.ErrPeerAborted):
			return fmt.Errorf("pairing aborted: the other device stopped (it may have refused the code)")
		case errors.Is(err, pairflow.ErrCommitmentMismatch):
			return fmt.Errorf("pairing refused: a device revealed a key it had not committed to — "+
				"this is what a man-in-the-middle looks like (%w)", err)
		default:
			return err
		}
	}
	if role == "authorise" {
		fmt.Fprintf(cmd.OutOrStdout(), "\npaired: signed an enrolment cert for the new device %s\n",
			identity.FormatPublicKey(res.PeerKey))
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "\nenrolled: this device now authenticates as user %s\n",
			identity.FormatPublicKey(res.PeerKey))
	}
	return nil
}

// normaliseSAS strips the cosmetic grouping space so "123 4567" and "1234567"
// compare equal against a derived, ungrouped SAS.
func normaliseSAS(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), " ", "")
}

// randomSession returns a fresh URL-safe rendezvous id. It is not a secret — the
// pairing's security is the short code — so plain hex of 16 random bytes is
// ample and cannot collide in practice.
func randomSession() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// relayHTTP is the CLI's HTTP implementation of pairflow.Relay: it PUTs and GETs
// opaque slot values against a running node's public relay (httpapi.RelayPrefix).
type relayHTTP struct {
	httpc *http.Client
	base  string
}

// newRelayHTTP builds a relay client for an address, supporting the same forms
// as the API client: a unix socket path, unix:///path, http://host:port, or a
// bare host:port.
func newRelayHTTP(addr string) (*relayHTTP, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, errors.New("pair: --relay is required (a unix socket path, unix:///path, " +
			"http://host:port or host:port)")
	}
	transport := &http.Transport{MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	base := ""
	switch {
	case strings.HasPrefix(addr, "unix://"), strings.HasPrefix(addr, "/"), strings.HasPrefix(addr, "./"):
		socket := strings.TrimPrefix(addr, "unix://")
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
		base = "http://relay.heyarr.invalid"
	case strings.HasPrefix(addr, "http://"), strings.HasPrefix(addr, "https://"):
		base = strings.TrimRight(addr, "/")
	default:
		base = "http://" + addr
	}
	return &relayHTTP{httpc: &http.Client{Transport: transport, Timeout: 30 * time.Second}, base: base}, nil
}

func (r *relayHTTP) slotURL(session, slot string) string {
	return r.base + httpapi.RelayPrefix + "/sessions/" + session + "/slots/" + slot
}

func (r *relayHTTP) Put(ctx context.Context, session, slot string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, r.slotURL(session, slot), strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := r.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("pair: reaching the relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("pair: relay refused %s (%d): %s", slot, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (r *relayHTTP) Get(ctx context.Context, session, slot string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.slotURL(session, slot), nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := r.httpc.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("pair: reaching the relay: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, false, nil
	case resp.StatusCode/100 == 2:
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, false, fmt.Errorf("pair: reading a relay slot: %w", err)
		}
		return body, true, nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, false, fmt.Errorf("pair: relay error on %s (%d): %s", slot, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}
