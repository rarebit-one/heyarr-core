package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/rarebit-one/voidbind-go/encryption"
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
	// BlobGeneratedAt is set when the keys came from an exported recovery blob
	// (--from-blob) rather than the control database: when that blob was made.
	BlobGeneratedAt string `json:"blob_generated_at,omitempty"`
}

func newSpaceRecoverCommand(_ Options, configPath, deviceDir *string) *cobra.Command {
	var (
		secretStr  string
		secretFile string
		fromBlob   string
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

With --from-blob the wrapped copies come from an exported recovery blob
(` + "`heyarr space export-recovery`" + `) instead of the control database, so recovery
needs no database at all. Anyone who knows your recovery PUBLIC key could make a
blob, so a key from one is not trusted for writing on its word: with --rewrap,
each key must first decrypt the newest content this node's database holds for
its space, and a key that does not (a stale blob from before the space was
re-keyed, or a forged one) is refused (ADR-0022 addendum).

The secret is read from --secret-file, or from --secret, or from standard input
— prefer a file or a pipe, since a secret in argv is visible in ps and shell
history.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSpaceRecover(cmd.Context(), cmd, *configPath, *deviceDir, secretStr, secretFile, fromBlob, rewrap, asJSON)
		},
	}
	cmd.Flags().StringVar(&secretStr, "secret", "", "the recovery secret (prefer --secret-file or stdin; argv is visible in ps)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "read the recovery secret from this file")
	cmd.Flags().StringVar(&fromBlob, "from-blob", "", "take the wrapped copies from this exported recovery blob instead of the control database")
	cmd.Flags().BoolVar(&rewrap, "rewrap", false, "also re-seal the recovered keys for THIS machine's device so it keeps reading the spaces (writes the control DB; run with the controller stopped)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func runSpaceRecover(ctx context.Context, cmd *cobra.Command, configPath, deviceDir, secretStr, secretFile, fromBlob string, rewrap, asJSON bool) error {
	raw, err := readRecoverySecret(cmd, secretStr, secretFile)
	if err != nil {
		return err
	}
	secret, err := parseRecoveryInput(raw)
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

	res := spaceRecoverResult{Recipient: recID}

	// The control database is needed to read the copies (the default source) or
	// to store re-wrapped ones; a blob recovery without --rewrap needs none.
	var db *sqlite.DB
	if fromBlob == "" || rewrap {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		db, err = sqlite.Open(ctx, sqlite.Options{Path: cfg.Database.Path})
		if err != nil {
			return fmt.Errorf("opening the controller database: %w", err)
		}
		defer func() { _ = db.Close() }()
	}

	var wrapped map[string][]byte
	if fromBlob != "" {
		data, err := os.ReadFile(fromBlob) // #nosec G304 -- the operator explicitly passed this path to read their own blob
		if err != nil {
			return fmt.Errorf("reading the recovery blob: %w", err)
		}
		blob, err := spacerecover.OpenBlob(secret, data)
		if err != nil {
			return err
		}
		wrapped = blob.Wrapped()
		res.BlobGeneratedAt = blob.GeneratedAt.UTC().Format(time.RFC3339)
	} else {
		wrapped, err = wrappedKeysForRecipient(ctx, db.Reader(), recID)
		if err != nil {
			return err
		}
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

	res.Recovered, res.SpaceIDs = len(ids), ids

	if rewrap && len(keys) > 0 {
		if fromBlob != "" {
			if err := verifyBlobKeys(ctx, db.Reader(), keys, ids); err != nil {
				return err
			}
		}
		device, err := rewrapForThisDevice(ctx, db, deviceDir, keys, ids)
		if err != nil {
			return err
		}
		res.Rewrapped = len(ids)
		res.Device = device
	}

	if asJSON {
		return emitJSON(cmd.OutOrStdout(), res)
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

// verifyBlobKeys is the check a key taken from a recovery blob must pass before
// it is re-wrapped for this device (ADR-0022 addendum). A blob is sealed to the
// recovery PUBLIC key, so opening one proves nothing about who made it: a forger
// can seal keys of their choosing, and re-wrapping such a key would have this
// device write future content under a key the forger holds. So each key must
// decrypt the NEWEST ciphertext (change or snapshot) this database holds for its
// space. That ties the key to content the space's real writers produced, and to
// the current key rather than one a rotation has since replaced (the rotation's
// snapshot is newer than anything under the old key). A space with no content
// here cannot be checked and is refused; nothing in it could be read anyway.
func verifyBlobKeys(ctx context.Context, r *sql.DB, keys map[string]encryption.SpaceKey, ids []string) error {
	for _, id := range ids {
		ct, ok, err := newestSpaceCiphertext(ctx, r, id)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("space %s: this database holds no content for it to check the blob's key against, "+
				"so the key is not re-wrapped (a blob can be forged; see `heyarr space recover --help`)", id)
		}
		if _, err := encryption.DecryptChange(keys[id], ct); err != nil {
			return fmt.Errorf("space %s: the blob's key does not open this space's newest content — "+
				"the blob is stale (the space was re-keyed after it was exported) or forged; nothing was re-wrapped", id)
		}
	}
	return nil
}

// newestSpaceCiphertext returns the most recently stored ciphertext of a space,
// change or snapshot, and whether there is any. Changes are ordered by their
// arrival seq; a snapshot beats the newest change when stored at or after it.
// Times are compared parsed, not as text: this store writes RFC 3339 with the
// fraction trimmed, which does not sort as text within a second.
func newestSpaceCiphertext(ctx context.Context, r *sql.DB, spaceID string) ([]byte, bool, error) {
	var (
		changeCT     []byte
		changeAt     string
		haveChange   = true
		snapID       string
		haveSnap     bool
		newestSnapAt time.Time
	)
	err := r.QueryRowContext(ctx,
		`SELECT ciphertext, created_at FROM encrypted_changes WHERE space_id = ? ORDER BY seq DESC LIMIT 1`,
		spaceID).Scan(&changeCT, &changeAt)
	if errors.Is(err, sql.ErrNoRows) {
		haveChange = false
	} else if err != nil {
		return nil, false, fmt.Errorf("reading the newest change of space %s: %w", spaceID, err)
	}

	rows, err := r.QueryContext(ctx, `SELECT snapshot_id, created_at FROM encrypted_snapshots WHERE space_id = ?`, spaceID)
	if err != nil {
		return nil, false, fmt.Errorf("listing the snapshots of space %s: %w", spaceID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, false, fmt.Errorf("reading a snapshot of space %s: %w", spaceID, err)
		}
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, false, fmt.Errorf("snapshot %s of space %s: %w", id, spaceID, err)
		}
		if !haveSnap || t.After(newestSnapAt) {
			haveSnap, snapID, newestSnapAt = true, id, t
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	useSnap := haveSnap
	if haveSnap && haveChange {
		ct, err := time.Parse(time.RFC3339Nano, changeAt)
		if err != nil {
			return nil, false, fmt.Errorf("the newest change of space %s: %w", spaceID, err)
		}
		useSnap = !newestSnapAt.Before(ct)
	}
	switch {
	case useSnap:
		var snapCT []byte
		if err := r.QueryRowContext(ctx, `SELECT ciphertext FROM encrypted_snapshots WHERE snapshot_id = ?`, snapID).Scan(&snapCT); err != nil {
			return nil, false, fmt.Errorf("reading snapshot %s: %w", snapID, err)
		}
		return snapCT, true, nil
	case haveChange:
		return changeCT, true, nil
	default:
		return nil, false, nil
	}
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
	if res.BlobGeneratedAt != "" {
		fmt.Fprintf(out, "From a recovery blob exported %s.\n", res.BlobGeneratedAt)
	}
	fmt.Fprintf(out, "Recovered %d space key(s) offline for recipient %s:\n", res.Recovered, res.Recipient)
	for _, id := range res.SpaceIDs {
		fmt.Fprintf(out, "  %s\n", id)
	}
	if res.Rewrapped > 0 {
		fmt.Fprintf(out, "Re-wrapped %d key(s) for this device (%s); it can now read the spaces.\n", res.Rewrapped, res.Device)
	}
}
