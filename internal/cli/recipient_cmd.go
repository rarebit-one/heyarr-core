package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rarebit-one/void-which-binds-go/device"
	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/servicerecipient"
)

// newRecipientCommand builds `heyarr recipient`: the service recipients an
// owner's device registers for executors (ADR-0104). An executor is never a
// member device; registering its X25519 public key here is the only way a space
// key may be wrapped for it, and then only on a space it is granted.
func newRecipientCommand(opts Options, configPath *string) *cobra.Command {
	var deviceDir string
	cmd := &cobra.Command{
		Use:   "recipient",
		Short: "Register executors' public keys as space-key recipients (ADR-0104)",
		Long: `Manage service recipients: executors' X25519 public keys that a space key may
be wrapped for.

An executor runs work for you against a few encrypted spaces. It is not one of
your devices and is never enrolled as one, so enrol-before-wrap refuses to wrap
a space key for it. Registering its public key here, from your own
management-authorised device, is the explicit act that lets you then grant it
spaces with ` + "`heyarr space grant`" + `.

These commands authenticate as this machine's enrolled device (--device-dir),
never with a bearer token: an admin token cannot register a recipient.`,
	}
	cmd.PersistentFlags().StringVar(&deviceDir, "device-dir", "",
		"where this machine's device key lives (default: your config directory; "+device.EnvDir+" overrides)")
	cmd.AddCommand(
		newRecipientAddCommand(opts, configPath, &deviceDir),
		newRecipientListCommand(opts, configPath, &deviceDir),
		newRecipientRemoveCommand(opts, configPath, &deviceDir),
	)
	return cmd
}

// deviceClientFlags are the flags of a command that calls the controller as
// this machine's enrolled device rather than with a bearer token: the consent
// acts of ADR-0104, which a token may never perform.
type deviceClientFlags struct {
	addr    string
	timeout time.Duration
	asJSON  bool
}

func (f *deviceClientFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.addr, "addr", "",
		"where the API is: a unix socket path, unix:///path, http://host:port or host:port "+
			"(default: the unix socket in the data directory)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", client.DefaultTimeout, "how long one request may take")
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "emit machine-readable JSON")
}

// withDeviceClient runs fn with a client that presents a fresh Device
// credential for this machine on every request (ADR-0048). It fails up front,
// with the enrolment hint, when this machine has no enrolled device key.
func (f *deviceClientFlags) withDeviceClient(cmd *cobra.Command, configPath *string, deviceDir string,
	fn func(context.Context, *client.Client) error,
) error {
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	store, err := openDeviceStore(deviceDir)
	if err != nil {
		return err
	}
	if _, err := store.Credential(time.Now().UTC(), 0); err != nil {
		return fmt.Errorf("this device cannot authenticate to the controller "+
			"(it must be enrolled, and authorised for management by an admin): %w", err)
	}
	addr, err := (&clientFlags{addr: f.addr}).apiAddr(cfg)
	if err != nil {
		return err
	}
	c, err := client.New(client.Options{
		Addr: addr, UnixSocket: cfg.HTTP.UnixSocket, Device: store, Timeout: f.timeout,
		UserAgent: "heyarr-cli/" + buildinfo.Get().Version,
	})
	if err != nil {
		return err
	}
	return fn(cmd.Context(), c)
}

// errFingerprintMismatch is a typed fingerprint that is not the key's.
var errFingerprintMismatch = errors.New("the fingerprint does not match this key — it is not the key the executor's host displayed; nothing was registered")

// confirmFingerprint shows the key's fingerprint and requires the operator to
// type the one the executor's host displayed (or pass it as --fingerprint).
// The controller is not trusted to relay keys, and neither is anything between
// the host and this terminal: the fingerprint is compared by a person.
func confirmFingerprint(cmd *cobra.Command, computed, given string) (string, error) {
	fmt.Fprintf(cmd.ErrOrStderr(), "key fingerprint: %s\n", computed)
	typed := given
	if typed == "" {
		fmt.Fprint(cmd.ErrOrStderr(), "type the fingerprint the executor's host displayed: ")
		line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("reading the fingerprint: %w", err)
		}
		typed = strings.TrimSpace(line)
	}
	if !servicerecipient.SameFingerprint(typed, computed) {
		return "", errFingerprintMismatch
	}
	return typed, nil
}

func newRecipientAddCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		flags                         deviceClientFlags
		executor, pub, label, fingerp string
	)
	cmd := &cobra.Command{
		Use:   "add --executor <principal> --pub x25519:<hex>",
		Short: "Register an executor's public key as a wrap recipient",
		Long: `Register an executor's X25519 public key as a service recipient for the
executor principal (the name given to ` + "`heyarr token create --executor`" + `).

Read the key and its fingerprint off the executor's own host, not off anything
that relayed them. This command prints the fingerprint it computes and asks you
to type the one the host displayed (or pass --fingerprint); a mismatch registers
nothing. Registering wraps nothing yet: ` + "`heyarr space grant`" + ` does that, per space.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fp, err := servicerecipient.Fingerprint(pub)
			if err != nil {
				return err
			}
			typed, err := confirmFingerprint(cmd, fp, fingerp)
			if err != nil {
				return err
			}
			return flags.withDeviceClient(cmd, configPath, *deviceDir, func(ctx context.Context, c *client.Client) error {
				sr, err := c.RegisterServiceRecipient(ctx, client.RegisterServiceRecipientRequest{
					Principal: executor, Recipient: pub, Label: label, Fingerprint: typed,
				})
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), sr)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "service recipient registered\n\n  id:           %s\n  executor:     %s\n  key:          %s\n  fingerprint:  %s\n",
					sr.ID, sr.PrincipalID, sr.Recipient, sr.Fingerprint)
				return nil
			})
		},
	}
	flags.register(cmd)
	cmd.Flags().StringVar(&executor, "executor", "", "the executor principal, by name or id")
	cmd.Flags().StringVar(&pub, "pub", "", "the executor's X25519 public key, x25519:<hex>")
	cmd.Flags().StringVar(&label, "label", "", "a note to recognise the executor by (no secrets: the controller stores it in the clear)")
	cmd.Flags().StringVar(&fingerp, "fingerprint", "", "the fingerprint the executor's host displayed (prompted for when absent)")
	_ = cmd.MarkFlagRequired("executor")
	_ = cmd.MarkFlagRequired("pub")
	return cmd
}

func newRecipientListCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var flags deviceClientFlags
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the registered service recipients",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return flags.withDeviceClient(cmd, configPath, *deviceDir, func(ctx context.Context, c *client.Client) error {
				list, err := c.ServiceRecipients(ctx)
				if err != nil {
					return err
				}
				if flags.asJSON {
					if list == nil {
						list = []client.ServiceRecipient{}
					}
					return emitJSON(cmd.OutOrStdout(), list)
				}
				if len(list) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "no service recipients")
					return nil
				}
				for _, sr := range list {
					fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %s  %s  %s\n", sr.ID, sr.PrincipalID, sr.Fingerprint, sr.Recipient, sr.Label)
				}
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}

func newRecipientRemoveCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var flags deviceClientFlags
	cmd := &cobra.Command{
		Use:   "remove <id | x25519:<hex>>",
		Short: "Withdraw a service recipient, and every space-key copy wrapped for it",
		Long: `Withdraw a service recipient. In the same step the controller deletes every
copy of a space key wrapped for it, on every space, so the executor can unwrap
nothing more from this node. Its grants stay; revoke them with
` + "`heyarr space revoke-executor`" + `.

This is not forward secrecy: the executor keeps any key it already unwrapped,
and a peer that replicated a copy keeps it until a rotation. See
` + "`heyarr space revoke-executor --help`" + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.withDeviceClient(cmd, configPath, *deviceDir, func(ctx context.Context, c *client.Client) error {
				out, err := c.RemoveServiceRecipient(ctx, args[0])
				if err != nil {
					return err
				}
				if flags.asJSON {
					return emitJSON(cmd.OutOrStdout(), out)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "service recipient %s removed; its copy of the key deleted from %d space(s)\n",
					out.ID, len(out.WrapsDeletedFrom))
				return nil
			})
		},
	}
	flags.register(cmd)
	return cmd
}
