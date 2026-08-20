package clidocs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// docsDir is the committed reference, relative to this package.
const docsDir = "../../docs/cli"

// The committed reference must match what the command tree produces.
//
// CI asserts the same thing by running scripts/gen.sh and then `git diff
// --exit-code`, which is the real gate. This test exists so the failure arrives
// during `go test` — the moment a flag is renamed — rather than as a red CI job
// on a pull request that looks unrelated to documentation.
func TestTheCommittedReferenceIsCurrent(t *testing.T) {
	tmp := t.TempDir()
	if err := Generate(tmp); err != nil {
		t.Fatalf("generating the reference: %v", err)
	}

	want := readMarkdown(t, tmp)
	got := readMarkdown(t, docsDir)

	for name, wantBody := range want {
		gotBody, ok := got[name]
		if !ok {
			t.Errorf("docs/cli/%s is missing — run `make gen` and commit the result", name)
			continue
		}
		if gotBody != wantBody {
			t.Errorf("docs/cli/%s is stale — run `make gen` and commit the result\n"+
				"--- committed ---\n%s\n--- generated ---\n%s", name, gotBody, wantBody)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			// The case `git diff` cannot catch on its own: a page for a command
			// that no longer exists is still committed and still unchanged.
			t.Errorf("docs/cli/%s documents a command that no longer exists — run `make gen`", name)
		}
	}
}

// Regenerating must produce the same bytes every time, or the diff check in CI
// fails on a schedule rather than on a change.
func TestGenerationIsDeterministic(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	for _, dir := range []string{first, second} {
		if err := Generate(dir); err != nil {
			t.Fatal(err)
		}
	}
	a, b := readMarkdown(t, first), readMarkdown(t, second)
	if len(a) != len(b) {
		t.Fatalf("two runs produced %d and %d pages", len(a), len(b))
	}
	names := make([]string, 0, len(a))
	for name := range a {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if a[name] != b[name] {
			t.Errorf("%s differs between two runs — something in it is not deterministic", name)
		}
	}
}

// A page that no command produces is removed rather than left behind.
func TestStalePagesAreRemoved(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "heyarr_summon.md")
	if err := os.WriteFile(stale, []byte("# a command that was deleted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Generate(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the page for a deleted command survived regeneration (%v)", err)
	}
}

// Every client command must be documented, because the reference is the only
// place the flags are written down for someone who is not reading the source.
func TestEveryCommandHasAPage(t *testing.T) {
	got := readMarkdown(t, docsDir)
	for _, name := range []string{
		"heyarr_library_add.md", "heyarr_library_list.md", "heyarr_scan.md",
		"heyarr_works_list.md", "heyarr_works_show.md", "heyarr_assets_list.md",
		"heyarr_blobs_stat.md", "heyarr_blobs_cat.md", "heyarr_blobs_verify.md",
		"heyarr_jobs_list.md", "heyarr_jobs_show.md", "heyarr_jobs_retry.md",
		"heyarr_peers_list.md", "heyarr_events_tail.md",
		"heyarr_token_create.md", "heyarr_fsck.md", "heyarr_gc.md",
	} {
		if _, ok := got[name]; !ok {
			t.Errorf("docs/cli/%s is missing", name)
		}
	}
	// And --json is documented wherever it exists, since it is the contract.
	for name, body := range got {
		if name == IndexFile || !strings.Contains(body, "--json") {
			continue
		}
		if !strings.Contains(body, "emit machine-readable JSON") {
			t.Errorf("%s mentions --json without describing it", name)
		}
	}
}

func readMarkdown(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	out := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Clean(filepath.Join(dir, entry.Name())))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = string(body)
	}
	return out
}
