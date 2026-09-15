package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spacerecover"
	"github.com/rarebit-one/heyarr-core/internal/recovery"
)

// spaceRecoverResult is the outcome of a space-key recovery. It deliberately
// carries NO key material — only which spaces were opened — so a recovery can be
// logged and scripted without ever writing a plaintext key to a terminal or file.
type spaceRecoverResult struct {
	Recipient string   `json:"recipient"`
	Recovered int      `json:"recovered"`
	SpaceIDs  []string `json:"space_ids"`
}

func newSpaceRecoverCommand(_ Options, configPath *string) *cobra.Command {
	var (
		secretStr  string
		secretFile string
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

The whole flow is offline: it reads the secret and the wrapped bytes and derives
the key, touching no server. Key material is never printed — only which spaces
were opened — so the output is safe to log. Re-wrapping the recovered keys for a
fresh device (so this machine can keep reading the spaces) is the next step (see
issue #545).

The secret is read from --secret-file, or from --secret, or from standard input
— prefer a file or a pipe, since a secret in argv is visible in ps and shell
history.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSpaceRecover(cmd.Context(), cmd, *configPath, secretStr, secretFile, asJSON)
		},
	}
	cmd.Flags().StringVar(&secretStr, "secret", "", "the recovery secret (prefer --secret-file or stdin; argv is visible in ps)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "read the recovery secret from this file")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func runSpaceRecover(ctx context.Context, cmd *cobra.Command, configPath, secretStr, secretFile string, asJSON bool) error {
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
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	printSpaceRecover(cmd.OutOrStdout(), res)
	return nil
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
}
