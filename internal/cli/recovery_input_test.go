package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rarebit-one/voidbind-go/recovery"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/spacerecover"
	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

var (
	ed25519Pattern     = regexp.MustCompile(`ed25519:[0-9a-f]{64}`)
	fingerprintPattern = regexp.MustCompile(`"[A-Z2-7]{4} [A-Z2-7]{4} [A-Z2-7]{4} [A-Z2-7]{4}"`)
	bareFingerprint    = regexp.MustCompile(`fingerprint +[A-Z2-7]{4} [A-Z2-7]{4} [A-Z2-7]{4} [A-Z2-7]{4}\n`)
)

// generateWithSecret runs `identity generate --json` and returns the secret it
// printed, written to a 0600 file.
func generateWithSecret(t *testing.T, idDir string, extra ...string) (secret, secretFile string) {
	t.Helper()
	args := append([]string{"identity", "generate", "--identity-dir", idDir, "--json"}, extra...)
	out, _, err := run(t, context.Background(), args...)
	if err != nil {
		t.Fatalf("identity generate: %v", err)
	}
	var got identityGenerateJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("generate --json: %v\n%s", err, out)
	}
	return got.RecoverySecret, writeSecretFile(t, got.RecoverySecret)
}

func writeSecretFile(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestVerifyRecoveryChecksWithoutUsing: the written secret, as written (grouped,
// upper case), matches its identity and prints the fingerprint; another
// identity's secret is an error. Nothing about the identity changes.
func TestVerifyRecoveryChecksWithoutUsing(t *testing.T) {
	ctx := context.Background()
	idDir := identityDir(t)
	secret, secretFile := generateWithSecret(t, idDir)
	before := showIdentityJSON(t, ctx, idDir)

	out, _, err := run(t, ctx, "identity", "verify-recovery", "--identity-dir", idDir, "--secret-file", secretFile)
	if err != nil {
		t.Fatalf("verify-recovery: %v", err)
	}
	if !strings.Contains(out, "matches this identity") || !bareFingerprint.MatchString(out) {
		t.Fatalf("output lacks the match or the fingerprint:\n%s", out)
	}

	var grouped strings.Builder
	for i := 0; i < len(secret); i += 4 {
		grouped.WriteString(strings.ToUpper(secret[i:min(i+4, len(secret))]) + " ")
	}
	if _, _, err := run(t, ctx, "identity", "verify-recovery", "--identity-dir", idDir,
		"--secret-file", writeSecretFile(t, grouped.String())); err != nil {
		t.Fatalf("the grouped upper-case form was refused: %v", err)
	}

	other, _ := recovery.GenerateSecret()
	_, _, err = run(t, ctx, "identity", "verify-recovery", "--identity-dir", idDir, "--secret-file", writeSecretFile(t, other.String()))
	if err == nil || !strings.Contains(err.Error(), "not this identity's recovery secret") {
		t.Fatalf("another identity's secret: err = %v", err)
	}
	if after := showIdentityJSON(t, ctx, idDir); after.PublicKey != before.PublicKey {
		t.Fatal("verify-recovery changed the identity")
	}
}

// TestVerifyRecoveryJSONShape pins the --json output.
func TestVerifyRecoveryJSONShape(t *testing.T) {
	idDir := identityDir(t)
	_, secretFile := generateWithSecret(t, idDir)
	out, _, err := run(t, context.Background(), "identity", "verify-recovery", "--identity-dir", idDir,
		"--secret-file", secretFile, "--json")
	if err != nil {
		t.Fatal(err)
	}
	norm := ed25519Pattern.ReplaceAllString(x25519Pattern.ReplaceAllString(out, "x25519:<hex>"), "ed25519:<hex>")
	norm = fingerprintPattern.ReplaceAllString(norm, `"XXXX XXXX XXXX XXXX"`)
	testutil.Golden(t, "testdata/identity_verify_recovery.json", []byte(norm))
}

// TestSharesStandInForTheSecret: two of three SLIP-39 shares, one per line, both
// verify and recover the same identity; one share is refused.
func TestSharesStandInForTheSecret(t *testing.T) {
	ctx := context.Background()
	idDir := identityDir(t)
	secret, _ := generateWithSecret(t, idDir)
	parsed, err := recovery.ParseSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	shares, err := recovery.SplitShares(parsed, 2, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	two := writeSecretFile(t, shares[0]+"\n\n"+shares[2]+"\n")
	if _, _, err := run(t, ctx, "identity", "verify-recovery", "--identity-dir", idDir, "--secret-file", two); err != nil {
		t.Fatalf("two shares did not verify: %v", err)
	}
	if _, _, err := run(t, ctx, "identity", "verify-recovery", "--identity-dir", idDir,
		"--secret-file", writeSecretFile(t, shares[1])); err == nil {
		t.Fatal("one share of a 2-of-3 set was accepted")
	}

	// Recover on a fresh machine from the shares: the same identity.
	fresh := identityDir(t)
	if _, _, err := run(t, ctx, "identity", "recover", "--identity-dir", fresh, "--device-dir", deviceDir(t),
		"--secret-file", two); err != nil {
		t.Fatalf("recover from shares: %v", err)
	}
	if got, want := showIdentityJSON(t, ctx, fresh).PublicKey, showIdentityJSON(t, ctx, idDir).PublicKey; got != want {
		t.Fatalf("shares recovered %s, want %s", got, want)
	}
}

// TestBlobAsTextRecovers: the blob's base64 text form, wrapped across lines as
// a note would hold it, recovers like the file.
func TestBlobAsTextRecovers(t *testing.T) {
	f := newBlobFixture(t)
	blobPath := f.export(t)
	data, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatal(err)
	}
	text := spacerecover.EncodeBlobText(data)
	scanned := filepath.Join(f.dir, "blob.txt")
	if err := os.WriteFile(scanned, []byte(text[:40]+"\n"+text[40:]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.recoverRewrap(t, scanned); err != nil {
		t.Fatalf("recover from the blob text: %v", err)
	}
}
