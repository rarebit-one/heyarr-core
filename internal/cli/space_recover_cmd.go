package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/rarebit-one/voidbind-go/encryption"
	"github.com/rarebit-one/voidbind-go/recovery"
	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spacerecover"
	psstore "github.com/rarebit-one/heyarr-core/internal/personalstate/store"
)

// spaceRecoverResult is the outcome of a space-key recovery. It deliberately
// carries NO key material — only which spaces were opened (and, with --rewrap, how
// many were re-sealed for this device) — so a recovery can be logged and scripted
// without ever writing a plaintext key to a terminal or file.
type spaceRecoverResult struct {
	Recipient string   `json:"recipient"`
	Recovered int      `json:"recovered"`
	SpaceIDs  []string `json:"space_ids"`
	Rewrapped int      `json:"rewrapped,omitempty"`
	Device    string   `json:"device,omitempty"`
}

func newSpaceRecoverCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		secretStr  string
		secretFile string
		rewrap     bool
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Recover vault space keys from your recovery secret, offline (ADR-0022, ADR-0049)",
		Long: `Recover the space keys of your encrypted vaults from the recovery secret you
saved at ` + "`heyarr identity generate`" + ` — on a machine with NO Heyarr running.

Every space is stored with a copy of its key sealed for your recovery encryption
key (ADR-0049). This re-derives that key's private half from the secret, offline,
and unwraps those copies from THIS node's control database. It is distinct from
` + "`heyarr recover`" + ` (which rebuilds a peer's control plane) and
` + "`heyarr identity recover`" + ` (which rebuilds your signing identity): this
one recovers the ability to READ vault content.

With --rewrap it also re-seals each recovered key for THIS machine's device key
(ADR-0022's recovery tail), so the recovered machine keeps reading the spaces
without the paper secret. That writes to the control database, so run it with the
controller stopped; the device must be enrolled first (` + "`heyarr identity recover`" + `
does that).

The whole flow is offline. Key material is never printed — only which spaces were
opened — so the output is safe to log.

The secret is read from --secret-file, or from --secret, or from standard input
— prefer a file or a pipe, since a secret in argv is visible in ps and shell
history.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSpaceRecover(cmd.Context(), cmd, *configPath, *deviceDir, secretStr, secretFile, rewrap, asJSON)
		},
	}
	cmd.Flags().StringVar(&secretStr, "secret", "", "the recovery secret (prefer --secret-file or stdin; argv is visible in ps)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "read the recovery secret from this file")
	cmd.Flags().BoolVar(&rewrap, "rewrap", false, "also re-seal the recovered keys for THIS machine's device so it keeps reading the spaces (writes the control DB; run with the controller stopped)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func runSpaceRecover(ctx context.Context, cmd *cobra.Command, configPath, deviceDir, secretStr, secretFile string, rewrap, asJSON bool) error {
	raw, err := readRecoverySecret(cmd, secretStr, secretFile)
	if err != nil {
		return err
	}
	secret, err := recovery.ParseSecret(raw)
	if err != nil {
		// A mistyped secret is caught by its checksum here rather than opening
		// nothing and looking like data loss — surface it cleanly.
		return fmt.Errorf("the recovery secret was not accepted: %w\n"+
			"check it against what you wrote down — a single mistyped character is caught here", err)
	}

	recID, err := spacerecover.RecipientID(secret)
	if err != nil {
		return err
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	db, err := sqlite.Open(ctx, sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		return fmt.Errorf("opening the controller database: %w", err)
	}
	defer func() { _ = db.Close() }()

	wrapped, err := wrappedKeysForRecipient(ctx, db.Reader(), recID)
	if err != nil {
		return err
	}

	keys, err := spacerecover.UnwrapAll(secret, wrapped)
	if err != nil {
		return err
	}

	ids := make([]string, 0, len(keys))
	for id := range keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	res := spaceRecoverResult{Recipient: recID, Recovered: len(ids), SpaceIDs: ids}

	if rewrap && len(keys) > 0 {
		device, err := rewrapForThisDevice(ctx, db, deviceDir, keys, ids)
		if err != nil {
			return err
		}
		res.Rewrapped = len(ids)
		res.Device = device
	}

	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	printSpaceRecover(cmd.OutOrStdout(), res)
	return nil
}

// rewrapForThisDevice re-seals each recovered space key for this machine's device
// encryption key and stores the wrapped copies (ADR-0022's recovery tail), so the
// device reads the spaces going forward. It writes the control DB, so it is an
// offline step (controller stopped). Returns the device recipient id it wrapped for.
func rewrapForThisDevice(ctx context.Context, db *sqlite.DB, deviceDir string, keys map[string]encryption.SpaceKey, ids []string) (string, error) {
	devPriv, err := loadDeviceEncKey(deviceDir)
	if err != nil {
		return "", fmt.Errorf("loading this machine's device key to re-wrap for (enrol it first with `heyarr identity recover`): %w", err)
	}
	deviceID := encryption.FormatPublicKey(devPriv.PublicKey().Bytes())

	rewrapped, err := spacerecover.RewrapForDevice(keys, deviceID)
	if err != nil {
		return "", err
	}

	if err := sqlite.Migrate(ctx, db); err != nil {
		return "", fmt.Errorf("migrating the control database: %w", err)
	}
	eventLog, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		return "", err
	}
	st, err := psstore.New(psstore.Options{Writer: db.Writer(), Reader: db.Reader(), Events: eventLog})
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		if _, err := st.PutWrappedKey(ctx, id, deviceID, rewrapped[id]); err != nil {
			return "", fmt.Errorf("storing the re-wrapped key for space %q: %w", id, err)
		}
	}
	return deviceID, nil
}

// wrappedKeysForRecipient reads, offline, the wrapped copy of every space's key
// that was sealed for the given recipient. The (space_id, recipient) uniqueness
// on wrapped_keys means at most one copy per space.
func wrappedKeysForRecipient(ctx context.Context, r *sql.DB, recipient string) (map[string][]byte, error) {
	rows, err := r.QueryContext(ctx, `SELECT space_id, wrapped FROM wrapped_keys WHERE recipient = ?`, recipient)
	if err != nil {
		return nil, fmt.Errorf("reading wrapped keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string][]byte)
	for rows.Next() {
		var spaceID string
		var wrapped []byte
		if err := rows.Scan(&spaceID, &wrapped); err != nil {
			return nil, fmt.Errorf("scanning a wrapped key: %w", err)
		}
		out[spaceID] = wrapped
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating wrapped keys: %w", err)
	}
	return out, nil
}

func printSpaceRecover(out io.Writer, res spaceRecoverResult) {
	if res.Recovered == 0 {
		fmt.Fprintf(out, "No space keys were wrapped for your recovery key (recipient %s).\n", res.Recipient)
		return
	}
	fmt.Fprintf(out, "Recovered %d space key(s) offline for recipient %s:\n", res.Recovered, res.Recipient)
	for _, id := range res.SpaceIDs {
		fmt.Fprintf(out, "  %s\n", id)
	}
	if res.Rewrapped > 0 {
		fmt.Fprintf(out, "Re-wrapped %d key(s) for this device (%s); it can now read the spaces.\n", res.Rewrapped, res.Device)
	}
}
