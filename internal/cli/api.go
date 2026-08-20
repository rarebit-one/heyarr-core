package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rarebit-one/heyarr-core/internal/buildinfo"
	"github.com/rarebit-one/heyarr-core/internal/client"
	"github.com/rarebit-one/heyarr-core/internal/config"
)

// The client commands.
//
// Everything in this file and its neighbours talks to a running Heyarr over
// /api/v1 — by default over the unix socket in the data directory. That is
// different from `token`, `fsck` and `gc`, which open the database directly
// because they are host administration and have to work before a credential
// exists or when the controller will not start. Nothing else may follow them:
// a command that reads the database because it is easier is a command that
// works on the host and nowhere else, and that stops agreeing with the API the
// first time the two are changed apart.

// TokenEnvVar is where a credential is read from when no flag gives one.
//
// An environment variable rather than an argument, and never an argument: a
// token on the command line is in the shell's history file, in `ps` output for
// every user on the machine, and in the CI log. The flag exists for scripts
// that build their own environment, and even it is documented as the last
// resort.
const TokenEnvVar = "HEYARR_TOKEN" // #nosec G101 -- the name of a variable, not a credential

// DefaultTokenFile is read, when it exists, relative to the data directory.
const DefaultTokenFile = "cli.token" // #nosec G101 -- a filename, not a credential

// clientFlags are the flags every client command shares.
type clientFlags struct {
	addr      string
	token     string
	tokenFile string
	timeout   time.Duration
	asJSON    bool
}

// register adds the shared flags to a command.
func (f *clientFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.addr, "addr", "",
		"where the API is: a unix socket path, unix:///path, http://host:port or host:port "+
			"(default: the unix socket in the data directory)")
	cmd.Flags().StringVar(&f.token, "token", "",
		"bearer token (prefer "+TokenEnvVar+": a token in argv is visible in ps and shell history)")
	cmd.Flags().StringVar(&f.tokenFile, "token-file", "",
		"read the bearer token from this file (default: <data_dir>/"+DefaultTokenFile+" when it exists)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", client.DefaultTimeout,
		"how long one request may take; streaming reads and the event stream are exempt")
	cmd.Flags().BoolVar(&f.asJSON, "json", false, "emit machine-readable JSON")
}

// listFlags are the flags a paginated listing adds.
type listFlags struct {
	limit    int
	pageSize int
}

func (f *listFlags) register(cmd *cobra.Command) {
	cmd.Flags().IntVar(&f.limit, "limit", 0,
		"stop after this many rows (default: every row, following pagination cursors)")
	cmd.Flags().IntVar(&f.pageSize, "page-size", client.DefaultPageSize,
		"how many rows one request asks for; the listing follows cursors regardless")
	// Not hidden, but not something a user needs: it exists so pagination can
	// be exercised against a handful of rows rather than by seeding 201.
	_ = cmd.Flags().MarkHidden("page-size")
}

func (f *listFlags) options() client.ListOptions {
	return client.ListOptions{Limit: f.limit, PageSize: f.pageSize}
}

// newClient builds the API client for a command.
//
// Token precedence, highest first, and it is documented here because a
// credential resolved from four places silently is a credential nobody can
// debug:
//
//  1. --token
//  2. $HEYARR_TOKEN
//  3. --token-file
//  4. <data_dir>/cli.token, when it exists
//
// The flag wins so that a script can override an inherited environment without
// unsetting it. The file is last because it is the ambient default, and an
// ambient default that overrode an explicit one would be a trap.
func (f *clientFlags) newClient(configPath string) (*client.Client, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	token, err := f.resolveToken(cfg)
	if err != nil {
		return nil, err
	}
	return client.New(client.Options{
		Addr:       f.addr,
		UnixSocket: cfg.HTTP.UnixSocket,
		Token:      token,
		Timeout:    f.timeout,
		UserAgent:  "heyarr-cli/" + buildinfo.Get().Version,
	})
}

func (f *clientFlags) resolveToken(cfg config.Config) (string, error) {
	if f.token != "" {
		return f.token, nil
	}
	if v := os.Getenv(TokenEnvVar); v != "" {
		return v, nil
	}
	path := f.tokenFile
	explicit := path != ""
	if path == "" && cfg.DataDir != "" {
		path = filepath.Join(cfg.DataDir, DefaultTokenFile)
	}
	if path == "" {
		return "", nil
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		if explicit {
			// Asked for by name and not readable is always a mistake worth
			// stopping for; the ambient default not existing is not.
			return "", fmt.Errorf("reading the token file %s: %w", path, err)
		}
		return "", nil
	}
	return strings.TrimSpace(string(raw)), nil
}

// emitJSON writes the --json contract.
//
// Indented, HTML escaping off, one trailing newline: the shape is a contract
// with scripts and with the golden files, so it is produced in exactly one
// place rather than by each command reaching for an encoder with its own
// settings.
func emitJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// table renders aligned columns for human output.
//
// Columns are sized from the data rather than fixed, because a fixed width
// truncates exactly the thing being looked for — a path, a title — and a
// truncated identifier that looks complete is worse than a ragged column.
type table struct {
	header []string
	rows   [][]string
}

func newTable(header ...string) *table { return &table{header: header} }

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *table) render(w io.Writer, empty string) error {
	if len(t.rows) == 0 {
		_, err := fmt.Fprintln(w, empty)
		return err
	}
	widths := make([]int, len(t.header))
	for i, h := range t.header {
		widths[i] = len(h)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	write := func(cells []string) error {
		var b strings.Builder
		for i, cell := range cells {
			if i > 0 {
				b.WriteString("  ")
			}
			if i == len(cells)-1 {
				b.WriteString(cell) // never pad the last column
				continue
			}
			b.WriteString(cell)
			b.WriteString(strings.Repeat(" ", widths[i]-len(cell)))
		}
		_, err := fmt.Fprintln(w, b.String())
		return err
	}
	if err := write(t.header); err != nil {
		return err
	}
	for _, row := range t.rows {
		if err := write(row); err != nil {
			return err
		}
	}
	return nil
}

// dash renders an absent optional value. An empty cell in a table is
// indistinguishable from a value that is the empty string.
func dash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// stamp renders a timestamp for human output, in UTC and to the second.
// Nanoseconds belong in --json, where something is parsing them.
func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func stampPtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return stamp(*t)
}

// withClient runs fn with a client built from the command's flags.
func (f *clientFlags) withClient(cmd *cobra.Command, configPath *string,
	fn func(context.Context, *client.Client) error,
) error {
	c, err := f.newClient(*configPath)
	if err != nil {
		return err
	}
	return fn(cmd.Context(), c)
}
