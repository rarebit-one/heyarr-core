package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// tokenConfig writes a config file pointing at a temporary data directory.
func tokenConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "heyarr.yaml")
	body := "data_dir: " + dir + "\npeer:\n  name: test\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var (
	uuidPattern      = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	timestampPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[\d:.]+Z`)
	tokenPattern     = regexp.MustCompile(auth.TokenPrefix + `[a-z2-7]+_[a-z2-7]+`)
)

// normalise replaces the parts that legitimately differ on every run, so the
// golden file asserts the *shape* — field names, nesting, presence — which is
// the part a client depends on and a refactor breaks silently.
func normalise(s string) string {
	s = uuidPattern.ReplaceAllString(s, "<uuid>")
	s = timestampPattern.ReplaceAllString(s, "<timestamp>")
	s = tokenPattern.ReplaceAllString(s, "<token>")
	return s
}

func TestTokenCreateJSONShape(t *testing.T) {
	cfg := tokenConfig(t)
	out, _, err := run(t, context.Background(), "--config", cfg,
		"token", "create", "jellyfin", "--scopes", "read,write", "--expires", "90d", "--json")
	if err != nil {
		t.Fatalf("token create: %v", err)
	}
	testutil.Golden(t, "testdata/token_create.json", []byte(normalise(out)))
}

func TestTokenListJSONShape(t *testing.T) {
	cfg := tokenConfig(t)
	ctx := context.Background()
	if _, _, err := run(t, ctx, "--config", cfg, "token", "create", "jellyfin", "--scopes", "read"); err != nil {
		t.Fatal(err)
	}
	out, _, err := run(t, ctx, "--config", cfg, "token", "list", "--json")
	if err != nil {
		t.Fatalf("token list: %v", err)
	}
	testutil.Golden(t, "testdata/token_list.json", []byte(normalise(out)))
}

func TestTokenListIsEmptyRatherThanNull(t *testing.T) {
	// `[]` and `null` are different to every JSON client, and the difference
	// shows up as a nil-pointer in somebody's script rather than here.
	out, _, err := run(t, context.Background(), "--config", tokenConfig(t), "token", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("an empty listing produced %q, want []", strings.TrimSpace(out))
	}
}

func TestTokenCreatePrintsTheSecretExactlyOnceAndSaysItIsNotRecoverable(t *testing.T) {
	cfg := tokenConfig(t)
	ctx := context.Background()

	out, _, err := run(t, ctx, "--config", cfg, "token", "create", "sonarr", "--scopes", "read")
	if err != nil {
		t.Fatal(err)
	}
	tokens := tokenPattern.FindAllString(out, -1)
	if len(tokens) != 1 {
		t.Fatalf("the human output printed the token %d times, want exactly 1:\n%s", len(tokens), out)
	}
	if !strings.Contains(out, "not recoverable") {
		t.Errorf("the output does not warn that the token cannot be recovered:\n%s", out)
	}

	// And it must not come back from `list`.
	listed, _, err := run(t, ctx, "--config", cfg, "token", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listed, tokens[0]) {
		t.Error("`token list` printed a credential")
	}
	if strings.Contains(listed, "argon2") || strings.Contains(strings.ToLower(listed), "hash") {
		t.Errorf("`token list` printed credential material:\n%s", listed)
	}
}

func TestTokenRevokeMarksAndThenRefuses(t *testing.T) {
	cfg := tokenConfig(t)
	ctx := context.Background()

	out, _, err := run(t, ctx, "--config", cfg, "token", "create", "svc", "--scopes", "admin", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatal(err)
	}

	if _, _, err := run(t, ctx, "--config", cfg, "token", "revoke", created.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	listed, _, err := run(t, ctx, "--config", cfg, "token", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed, `"status": "revoked"`) {
		t.Errorf("the listing does not show the token as revoked:\n%s", listed)
	}

	// A second revocation must report that it did nothing, or a script cannot
	// tell "revoked it" from "it was already gone".
	if _, _, err := run(t, ctx, "--config", cfg, "token", "revoke", created.ID); err == nil {
		t.Error("revoking twice reported success")
	}
	if _, _, err := run(t, ctx, "--config", cfg, "token", "revoke", "not-a-token"); err == nil {
		t.Error("revoking an unknown id reported success")
	}
}

func TestTokenCreateRejectsAnUnknownScope(t *testing.T) {
	if _, _, err := run(t, context.Background(), "--config", tokenConfig(t),
		"token", "create", "svc", "--scopes", "read,root"); err == nil {
		t.Error("an unknown scope was accepted, silently granting less than asked for")
	}
}

func TestExpiryDurations(t *testing.T) {
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{in: "90d", want: 90 * 24 * time.Hour},
		{in: "12h", want: 12 * time.Hour},
		{in: "2w", want: 14 * 24 * time.Hour},
		{in: "1y", want: 365 * 24 * time.Hour},
		{in: "30m", want: 30 * time.Minute},
		{in: "0d", wantErr: true},
		{in: "-5d", wantErr: true},
		{in: "soon", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseDuration(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDuration(%q) = %v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("parseDuration(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
