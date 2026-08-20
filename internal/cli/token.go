package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// newTokenCommand builds `heyarr token`.
//
// These commands talk to the database directly rather than to the API, and that
// is not a shortcut around ADR-0002's "roles communicate only over HTTP": they
// are not a role. They are host administration, and they have to work before
// any credential exists — an API that could mint its own first token would be
// an API with an unauthenticated write endpoint, which is the thing ADR-0011
// exists to prevent. Being administrative, they require access to the database
// file, which is the same access that would let you edit it by hand anyway.
func newTokenCommand(opts Options, configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage API tokens (ADR-0011)",
		Long: `Manage the bearer tokens that authenticate API callers.

These commands operate on the controller database directly and must be run on
the host, as the user that owns the data directory. A token's secret is shown
once, at creation, and is stored only as an argon2id hash — it cannot be
recovered afterwards.`,
	}
	cmd.AddCommand(
		newTokenCreateCommand(opts, configPath),
		newTokenListCommand(opts, configPath),
		newTokenRevokeCommand(opts, configPath),
	)
	return cmd
}

// withStore opens the database, migrates it and hands a store to fn.
//
// Migrating here is deliberate: `heyarr token create` is frequently the very
// first command run on a new install, before any role has started, and failing
// with "no such table: api_tokens" would be a puzzle rather than an error.
func withStore(ctx context.Context, configPath string, fn func(context.Context, *auth.Store) error) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := cfg.EnsureDataDir(); err != nil {
		return err
	}
	db, err := sqlite.Open(ctx, sqlite.Options{Path: cfg.Database.Path})
	if err != nil {
		return fmt.Errorf("token: opening the controller database: %w", err)
	}
	defer func() { _ = db.Close() }()

	if err := sqlite.Migrate(ctx, db); err != nil {
		return fmt.Errorf("token: migrating the controller database: %w", err)
	}
	store, err := auth.NewStore(auth.StoreOptions{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		return err
	}
	return fn(ctx, store)
}

func newTokenCreateCommand(opts Options, configPath *string) *cobra.Command {
	var (
		scopeList string
		expires   string
		asJSON    bool
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Mint an API token",
		Long: `Mint an API token for a named service.

The token is printed once and cannot be recovered. Creating a second token for
the same name is how rotation works: both are valid until you revoke the old
one.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scopes, err := auth.ParseScopes(scopeList)
			if err != nil {
				return err
			}
			var expiresAt *time.Time
			if expires != "" {
				d, err := parseDuration(expires)
				if err != nil {
					return err
				}
				t := time.Now().UTC().Add(d)
				expiresAt = &t
			}
			return withStore(cmd.Context(), *configPath, func(ctx context.Context, store *auth.Store) error {
				created, err := store.Create(ctx, args[0], scopes, expiresAt)
				if err != nil {
					return err
				}
				return printCreatedToken(cmd.OutOrStdout(), created, asJSON)
			})
		},
	}
	cmd.Flags().StringVar(&scopeList, "scopes", "read", "comma-separated scopes: read, write, admin")
	cmd.Flags().StringVar(&expires, "expires", "",
		"expiry as a duration from now, e.g. 90d, 12h, 1y (default: never)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

// createdTokenJSON is the --json shape. `token` appears only here and only
// once, which is why the field is named plainly rather than hidden: a script
// capturing it must be able to see that it is capturing a secret.
type createdTokenJSON struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	CreatedAt string   `json:"created_at"`
	ExpiresAt string   `json:"expires_at,omitempty"`
	Token     string   `json:"token"`
	Warning   string   `json:"warning"`
}

const notRecoverable = "this token is shown once and is not recoverable — it is stored only as an argon2id hash"

func printCreatedToken(w io.Writer, created auth.CreatedToken, asJSON bool) error {
	tk := created.Token
	if asJSON {
		out := createdTokenJSON{
			ID:        tk.ID,
			Name:      tk.Name,
			Scopes:    scopeStrings(tk.Scopes),
			CreatedAt: tk.CreatedAt.UTC().Format(time.RFC3339Nano),
			Token:     created.Secret,
			Warning:   notRecoverable,
		}
		if tk.ExpiresAt != nil {
			out.ExpiresAt = tk.ExpiresAt.UTC().Format(time.RFC3339Nano)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	expiry := "never"
	if tk.ExpiresAt != nil {
		expiry = tk.ExpiresAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(w, "token created\n\n")
	fmt.Fprintf(w, "  id       %s\n", tk.ID)
	fmt.Fprintf(w, "  name     %s\n", tk.Name)
	fmt.Fprintf(w, "  scopes   %s\n", auth.Join(tk.Scopes))
	fmt.Fprintf(w, "  expires  %s\n\n", expiry)
	fmt.Fprintf(w, "  %s\n\n", created.Secret)
	fmt.Fprintf(w, "Copy it now: %s.\n", notRecoverable)
	fmt.Fprintf(w, "Use it as:   Authorization: Bearer <token>\n")
	return nil
}

func newTokenListCommand(opts Options, configPath *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List API tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withStore(cmd.Context(), *configPath, func(ctx context.Context, store *auth.Store) error {
				tokens, err := store.List(ctx)
				if err != nil {
					return err
				}
				return printTokens(cmd.OutOrStdout(), tokens, store.Now(), asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

// tokenJSON is the listing shape. There is no hash field and never will be.
type tokenJSON struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	Status     string   `json:"status"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	ExpiresAt  string   `json:"expires_at,omitempty"`
	RevokedAt  string   `json:"revoked_at,omitempty"`
}

// tokenStatus collapses revoked_at and expires_at into the one word an operator
// is actually looking for. Presenting two nullable timestamps and expecting the
// reader to do the date comparison is how a live token gets mistaken for a dead
// one and left in place.
func tokenStatus(tk auth.Token, now time.Time) string {
	switch err := tk.Active(now); {
	case errors.Is(err, auth.ErrRevoked):
		return "revoked"
	case errors.Is(err, auth.ErrExpired):
		return "expired"
	default:
		return "active"
	}
}

func printTokens(w io.Writer, tokens []auth.Token, now time.Time, asJSON bool) error {
	if asJSON {
		out := make([]tokenJSON, 0, len(tokens))
		for _, tk := range tokens {
			row := tokenJSON{
				ID:        tk.ID,
				Name:      tk.Name,
				Scopes:    scopeStrings(tk.Scopes),
				Status:    tokenStatus(tk, now),
				CreatedAt: tk.CreatedAt.UTC().Format(time.RFC3339Nano),
			}
			for _, f := range []struct {
				src *time.Time
				dst *string
			}{{tk.LastUsedAt, &row.LastUsedAt}, {tk.ExpiresAt, &row.ExpiresAt}, {tk.RevokedAt, &row.RevokedAt}} {
				if f.src != nil {
					*f.dst = f.src.UTC().Format(time.RFC3339Nano)
				}
			}
			out = append(out, row)
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if len(tokens) == 0 {
		fmt.Fprintln(w, "no tokens — create one with `heyarr token create <name> --scopes read`")
		return nil
	}
	fmt.Fprintf(w, "%-36s  %-16s  %-16s  %-8s  %s\n", "ID", "NAME", "SCOPES", "STATUS", "LAST USED")
	for _, tk := range tokens {
		lastUsed := "never"
		if tk.LastUsedAt != nil {
			lastUsed = tk.LastUsedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%-36s  %-16s  %-16s  %-8s  %s\n",
			tk.ID, tk.Name, auth.Join(tk.Scopes), tokenStatus(tk, now), lastUsed)
	}
	return nil
}

func newTokenRevokeCommand(opts Options, configPath *string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke an API token",
		Long: `Revoke an API token by id, as printed by 'heyarr token list'.

Revocation takes effect on the very next request: the server reads the token
row on every call, so nothing is cached past it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withStore(cmd.Context(), *configPath, func(ctx context.Context, store *auth.Store) error {
				tk, err := store.Revoke(ctx, args[0])
				switch {
				case errors.Is(err, auth.ErrNotFound):
					return fmt.Errorf("token: no token with id %s — check `heyarr token list`", args[0])
				case errors.Is(err, auth.ErrRevoked):
					return fmt.Errorf("token: %s was already revoked at %s",
						tk.ID, tk.RevokedAt.UTC().Format(time.RFC3339))
				case err != nil:
					return err
				}
				if asJSON {
					enc := json.NewEncoder(cmd.OutOrStdout())
					enc.SetIndent("", "  ")
					return enc.Encode(tokenJSON{
						ID:        tk.ID,
						Name:      tk.Name,
						Scopes:    scopeStrings(tk.Scopes),
						Status:    "revoked",
						CreatedAt: tk.CreatedAt.UTC().Format(time.RFC3339Nano),
						RevokedAt: tk.RevokedAt.UTC().Format(time.RFC3339Nano),
					})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "revoked %s (%s)\n", tk.ID, tk.Name)
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return cmd
}

func scopeStrings(scopes []auth.Scope) []string {
	out := make([]string, 0, len(scopes))
	for _, s := range auth.Sort(scopes) {
		out = append(out, string(s))
	}
	return out
}

// parseDuration accepts Go durations plus the day, week and year suffixes an
// operator reaches for. `--expires 90d` is the common case and time.ParseDuration
// rejects it, which would leave "2160h" as the only way to say it.
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("token: --expires needs a duration, e.g. 90d")
	}
	units := []struct {
		suffix string
		unit   time.Duration
	}{
		{"y", 365 * 24 * time.Hour},
		{"w", 7 * 24 * time.Hour},
		{"d", 24 * time.Hour},
	}
	for _, u := range units {
		if !strings.HasSuffix(s, u.suffix) {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
		if err != nil {
			return 0, fmt.Errorf("token: %q is not a duration — try 90d, 12h or 1y", s)
		}
		if n <= 0 {
			return 0, fmt.Errorf("token: %q expires in the past", s)
		}
		return time.Duration(n * float64(u.unit)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("token: %q is not a duration — try 90d, 12h or 1y", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("token: %q expires in the past", s)
	}
	return d, nil
}
