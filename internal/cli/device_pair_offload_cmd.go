package cli

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rarebit-one/voidbind-go/device"
	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/peer/identity"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/client/cruciform"
	vbrelay "github.com/rarebit-one/voidbind-go/relay"
)

// newDevicePairOffloadCommand builds `heyarr device pair-offload`: the one-time
// ceremony that pairs THIS desktop with your phone (one.rarebit.cruciform) for the
// cruciform-offload custody backend (ADR-0098). The desktop holds no device key;
// after pairing, each space unwrap wakes the phone, which hardware-gates and
// returns the space key. This command establishes that trust once.
//
// It pins the phone's device + encryption keys here, and pins THIS desktop's
// transport key on the phone, authenticated by a short number-compare (SAS). The
// transport key is a pairing identity, not a custody key: stolen alone it opens
// nothing without the phone and its biometric.
func newDevicePairOffloadCommand(_ Options, dir *string) *cobra.Command {
	var (
		relayAddr  string
		confirmSAS string
		yes        bool
		poll       time.Duration
		timeout    time.Duration
		outPath    string
	)
	cmd := &cobra.Command{
		Use:   "pair-offload",
		Short: "Pair this desktop with your phone for cruciform-offload custody (ADR-0098)",
		Long: `Pair this desktop with your phone (one.rarebit.cruciform) so the vault's
` + "`cruciform`" + ` custody backend can open spaces without any device key on this
machine: each unwrap wakes the phone, which hardware-gates and returns the key.

Run this once. It creates a rendezvous on the node's voidbind relay and prints a
` + "`voidbind:offload-pair?…`" + ` invite — the payload the phone scans as a QR (a
desktop GUI renders it; the CLI prints the text). Both screens then show a short
code; compare them, and on a match this desktop pins the phone's keys and the
phone pins this desktop's transport key.

The transport key it pins is NOT a device encryption key (this backend holds
none) — it only authenticates unwrap requests as coming from this paired
terminal. Re-running reuses the existing transport key if one is already paired.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			deviceDir, err := resolveDeviceDir(*dir)
			if err != nil {
				return err
			}
			out := outPath
			if out == "" {
				out = filepath.Join(deviceDir, cruciform.PairConfigFileName)
			}

			transportKey, reused, err := loadOrMintTransportKey(out)
			if err != nil {
				return err
			}

			base, hc, err := newVoidbindRelayBase(relayAddr)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			session, err := vbrelay.CreateSession(ctx, hc, base)
			if err != nil {
				return fmt.Errorf("creating the pairing rendezvous: %w", err)
			}
			salt, err := newPairSalt()
			if err != nil {
				return err
			}
			in, err := cruciform.NewInitiator(transportKey, base, session, salt)
			if err != nil {
				return err
			}
			invite, err := cruciform.EncodeInvite(base, session, salt)
			if err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			if reused {
				fmt.Fprintf(w, "re-pairing with the existing transport key %s\n", in.TransportID())
			}
			fmt.Fprintf(w, "scan this on your phone (one.rarebit.cruciform):\n\n  %s\n\n", invite)

			transport := &vbrelay.Client{Base: base, Session: session, Role: "initiator", HTTP: hc, PollInterval: poll}
			sas, err := in.Handshake(ctx, transport)
			if err != nil {
				return fmt.Errorf("pairing handshake: %w", err)
			}
			ok, err := confirmOffloadSAS(cmd, sas.Grouped(), string(sas), confirmSAS, yes)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("pairing refused: the codes did not match, so nothing was paired")
			}
			cfg, err := in.Confirm(ctx, transport)
			if err != nil {
				return fmt.Errorf("confirming the pairing: %w", err)
			}
			if err := cruciform.SavePairConfig(out, cfg); err != nil {
				return err
			}
			fmt.Fprintf(w, "\npaired: this desktop now offloads unwraps to phone %s.\n",
				identity.FormatPublicKey(cfg.PhonePub))
			fmt.Fprintf(w, "  pairing config: %s\n  relay:          %s\n\n", out, cfg.RelayBase)
			fmt.Fprintf(w, "Select it with `vault.unwrapper: cruciform` to open spaces via the phone.\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&relayAddr, "relay", "",
		"the node's voidbind relay base the phone also reaches: unix:///path, http://host:port/pair, or host:port/pair")
	cmd.Flags().StringVar(&confirmSAS, "confirm-sas", "",
		"proceed only if the derived code equals this value — the scripted stand-in for a human comparison")
	cmd.Flags().BoolVar(&yes, "yes", false,
		"assume the codes matched, without prompting (use only when you compared them another way)")
	cmd.Flags().DurationVar(&poll, "poll", vbrelay.DefaultPollInterval,
		"how often to re-check the relay for the phone's next step")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute,
		"how long to wait for the whole pairing before giving up")
	cmd.Flags().StringVar(&outPath, "out", "",
		"where to write the pairing config (default: cruciform-pairing.json in the device directory)")
	return cmd
}

// resolveDeviceDir returns the device directory, defaulting to the platform
// default when empty — the same resolution openDeviceStore performs.
func resolveDeviceDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	return device.DefaultDir()
}

// loadOrMintTransportKey reuses the transport key already pinned at path (so
// re-pairing keeps the same terminal identity), or mints a fresh one for a first
// pairing. It reports whether it reused an existing key.
func loadOrMintTransportKey(path string) (ed25519.PrivateKey, bool, error) {
	if existing, err := cruciform.LoadPairConfig(path); err == nil {
		return existing.TransportKey, true, nil
	} else if !os.IsNotExist(err) {
		return nil, false, fmt.Errorf("reading the existing pairing config %s: %w", path, err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, false, fmt.Errorf("minting a transport key: %w", err)
	}
	return key, false, nil
}

// newPairSalt returns a fresh pairing salt.
func newPairSalt() ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generating a pairing salt: %w", err)
	}
	return salt, nil
}

// newVoidbindRelayBase resolves a relay address into the base string voidbind's
// relay.Client dials (<base>/v1/sessions/…) and an HTTP client for it. It accepts
// the same address forms as `heyarr pair`: a unix socket, an http(s) origin, or a
// bare host:port — for a unix socket the base is a placeholder host the custom
// dialer ignores.
func newVoidbindRelayBase(addr string) (string, *http.Client, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", nil, errors.New("pair-offload: --relay is required (the node's voidbind relay base, e.g. http://host:port/pair)")
	}
	transport := &http.Transport{MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	switch {
	case strings.HasPrefix(addr, "unix://"), strings.HasPrefix(addr, "/"), strings.HasPrefix(addr, "./"):
		socket := strings.TrimPrefix(addr, "unix://")
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		}
		return "http://relay.heyarr.invalid/pair", &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
	case strings.HasPrefix(addr, "http://"), strings.HasPrefix(addr, "https://"):
		return strings.TrimRight(addr, "/"), &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
	default:
		return "http://" + strings.TrimRight(addr, "/"), &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
	}
}

// confirmOffloadSAS prints the derived code and decides whether to proceed:
// against --confirm-sas when given (the scripted human), silently on --yes, or by
// prompting. It mirrors pair_cmd's confirmFunc.
func confirmOffloadSAS(cmd *cobra.Command, grouped, raw, want string, yes bool) (bool, error) {
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "short authentication code: %s\n", grouped)
	if want != "" {
		return normaliseSAS(want) == raw, nil
	}
	if yes {
		return true, nil
	}
	fmt.Fprintf(w, "does your phone show the SAME code? [y/N]: ")
	reader := bufio.NewReader(cmd.InOrStdin())
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, fmt.Errorf("reading your confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
