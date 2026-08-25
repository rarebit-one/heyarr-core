package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/testutil"
)

// backupConfig writes a minimal config file with a data directory and a peer,
// enough for `heyarr backup` to open a database and resolve this peer's id.
func backupConfig(t *testing.T) (configPath, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "heyarr.yaml")
	body := "data_dir: " + dir + "\npeer:\n  name: test\n  site: test-site\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

var (
	reBackupPath    = regexp.MustCompile(`"path": "[^"]*"`)
	reBackupTakenAt = regexp.MustCompile(`"taken_at": "[^"]*"`)
	reBackupDigest  = regexp.MustCompile(`"digest": "[^"]*"`)
	reBackupSize    = regexp.MustCompile(`"size_bytes": \d+`)
)

// normaliseBackup replaces the fields that vary run to run — the temp path, the
// read instant, the digest of a database whose peer id is a fresh random key,
// and the byte size — so the golden pins the SHAPE and the stable fields
// (generation, schema, signed, omissions), which are the ones that carry
// meaning.
func normaliseBackup(s string) string {
	s = reBackupPath.ReplaceAllString(s, `"path": "<path>"`)
	s = reBackupTakenAt.ReplaceAllString(s, `"taken_at": "<taken_at>"`)
	s = reBackupDigest.ReplaceAllString(s, `"digest": "<digest>"`)
	s = reBackupSize.ReplaceAllString(s, `"size_bytes": "<size>"`)
	return s
}

func TestBackupJSONShape(t *testing.T) {
	configPath, dataDir := backupConfig(t)

	out, _, err := run(t, context.Background(), "--config", configPath, "backup", "--json")
	if err != nil {
		t.Fatalf("backup --json failed: %v\n%s", err, out)
	}

	var decoded backupJSON
	if jsonErr := json.Unmarshal([]byte(out), &decoded); jsonErr != nil {
		t.Fatalf("not valid JSON: %v\n%s", jsonErr, out)
	}
	// Hand-written expectations for the fields that carry meaning, paired with
	// the golden (which proves only that nothing changed).
	if decoded.Generation <= 0 {
		t.Errorf("generation is %d, want positive — a backup at zero is not a backup", decoded.Generation)
	}
	if len(decoded.Omissions) != 1 || decoded.Omissions[0] != "provider-credentials" {
		t.Errorf("omissions = %v, want [provider-credentials]", decoded.Omissions)
	}

	// The backup landed on disk under the data directory.
	if _, statErr := os.Stat(filepath.Join(dataDir, "backups")); statErr != nil {
		t.Errorf("no backups directory under the data dir: %v", statErr)
	}

	testutil.Golden(t, "testdata/backup.json", []byte(normaliseBackup(out)))
}

// TestBackupIsIdempotentPerGeneration proves two backups with no state change
// between them report the same generation, so the demo's "generation advanced"
// assertion means a real change rather than a second invocation.
func TestBackupIsIdempotentPerGeneration(t *testing.T) {
	configPath, _ := backupConfig(t)

	first, _, err := run(t, context.Background(), "--config", configPath, "backup", "--json")
	if err != nil {
		t.Fatalf("first backup: %v\n%s", err, first)
	}
	second, _, err := run(t, context.Background(), "--config", configPath, "backup", "--json")
	if err != nil {
		t.Fatalf("second backup: %v\n%s", err, second)
	}
	var a, b backupJSON
	if err := json.Unmarshal([]byte(first), &a); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if err := json.Unmarshal([]byte(second), &b); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if a.Generation != b.Generation {
		t.Errorf("generation moved with no state change: %d then %d", a.Generation, b.Generation)
	}
}
