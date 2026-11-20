package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

// The runtime surface #228 adds, end to end through the CLI against a live API:
// a library gains a second root after creation, loses one without losing the
// library, and an emptied library is removed by name.
func TestLibraryRootAddRemoveAndLibraryRm(t *testing.T) {
	h := newAPIHarness(t)

	// Create with one root, then add a second — the case the issue hit, where
	// the only prior workaround was a second library over the same tree.
	h.mustRun("library", "add", "shows", "--content-type", "movie", "--root", "/srv/a")
	h.mustRun("library", "root", "add", "shows", "/srv/b", "--ingest-mode", "hardlink")

	libs := listLibraries(t, h)
	if len(libs) != 1 || len(libs[0].Roots) != 2 {
		t.Fatalf("after root add: %d libraries, %d roots (want 1, 2)", len(libs), rootCount(libs))
	}

	// Remove the second root by its path; the library and its other root stay.
	h.mustRun("library", "root", "rm", "shows", "/srv/b")
	libs = listLibraries(t, h)
	if len(libs) != 1 || len(libs[0].Roots) != 1 || libs[0].Roots[0].Path != "/srv/a" {
		t.Fatalf("after root rm: %d libraries, roots %v (want 1 library, root /srv/a)", len(libs), libs)
	}

	// A non-empty library refuses removal; this one holds no assets, so it goes.
	out := h.mustRun("library", "rm", "shows", "--json")
	var removed struct{ Status, Name string }
	if err := json.Unmarshal([]byte(out), &removed); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if removed.Status != "removed" || removed.Name != "shows" {
		t.Fatalf("rm = %+v", removed)
	}
	if libs := listLibraries(t, h); len(libs) != 0 {
		t.Fatalf("library survived rm: %v", libs)
	}
}

// Changing a root's ingest mode after creation, end to end through the CLI
// (#222): a root added on the reflink default is switched to hardlink in place,
// the supported way to stop a byte copy on a filesystem that cannot block-clone.
func TestLibraryRootSetIngestMode(t *testing.T) {
	h := newAPIHarness(t)
	h.mustRun("library", "add", "shows", "--content-type", "movie", "--root", "/srv/a")

	// The seeded root defaults to reflink; flip it.
	out := h.mustRun("library", "root", "set-ingest-mode", "shows", "/srv/a", "hardlink", "--json")
	var updated client.LibraryRoot
	if err := json.Unmarshal([]byte(out), &updated); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if updated.IngestMode != "hardlink" {
		t.Fatalf("set-ingest-mode returned %q, want hardlink", updated.IngestMode)
	}

	// And it is the persisted state a later ingest would read, not just the echo.
	libs := listLibraries(t, h)
	if len(libs) != 1 || len(libs[0].Roots) != 1 || libs[0].Roots[0].IngestMode != "hardlink" {
		t.Fatalf("after set-ingest-mode: roots %v (want one root, hardlink)", libs)
	}

	// An unknown mode is a clean error, not a 500 from the CHECK constraint.
	if _, stderr, err := h.run("library", "root", "set-ingest-mode", "shows", "/srv/a", "symlink"); err == nil {
		t.Error("an unknown ingest mode succeeded")
	} else if !strings.Contains(err.Error()+stderr, "ingest_mode") {
		t.Errorf("error does not name the field: err=%v stderr=%s", err, stderr)
	}
}

// Renaming a library end to end through the CLI (ADR-0124): named by its old
// name, it keeps its id and roots and answers to the new name only; a name
// another library holds is refused.
func TestLibraryRename(t *testing.T) {
	h := newAPIHarness(t)
	h.mustRun("library", "add", "verify-92", "--content-type", "movie", "--root", "/srv/a")
	h.mustRun("library", "add", "books", "--content-type", "book")
	before := listLibraries(t, h)

	out := h.mustRun("library", "rename", "verify-92", "Movies", "--json")
	var renamed client.Library
	if err := json.Unmarshal([]byte(out), &renamed); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var original client.Library
	for _, l := range before {
		if l.Name == "verify-92" {
			original = l
		}
	}
	if renamed.ID != original.ID || renamed.Name != "Movies" {
		t.Fatalf("rename = %+v, want id %s name Movies", renamed, original.ID)
	}
	if len(renamed.Roots) != 1 || renamed.Roots[0].Path != "/srv/a" {
		t.Fatalf("rename lost the root: %+v", renamed.Roots)
	}

	// By id works too, and the old name no longer resolves.
	h.mustRun("library", "rename", renamed.ID, "Films")
	if _, _, err := h.run("library", "rename", "verify-92", "Other"); err == nil {
		t.Error("the old name still resolves after the rename")
	}

	if _, stderr, err := h.run("library", "rename", "Films", "books"); err == nil {
		t.Error("renaming onto another library's name succeeded")
	} else if !strings.Contains(err.Error()+stderr, "already exists") {
		t.Errorf("error does not say the name is taken: err=%v stderr=%s", err, stderr)
	}
}

// Naming a root or a library that is not there is a clear error, not a panic.
func TestLibraryRootRmUnknownRoot(t *testing.T) {
	h := newAPIHarness(t)
	h.mustRun("library", "add", "shows", "--content-type", "movie", "--root", "/srv/a")

	_, stderr, err := h.run("library", "root", "rm", "shows", "/srv/nope")
	if err == nil {
		t.Fatal("removing a root that does not exist succeeded")
	}
	if !strings.Contains(err.Error(), "no root") && !strings.Contains(stderr, "no root") {
		t.Errorf("error does not name the missing root: err=%v stderr=%s", err, stderr)
	}
}

func listLibraries(t *testing.T, h *apiHarness) []client.Library {
	t.Helper()
	out := h.mustRun("library", "list", "--json")
	var libs []client.Library
	if err := json.Unmarshal([]byte(out), &libs); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return libs
}

func rootCount(libs []client.Library) int {
	n := 0
	for _, l := range libs {
		n += len(l.Roots)
	}
	return n
}

// A library whose type predates the gates (#896) is marked in `library list`
// and named on stderr, so the table stays parseable; a known type is not.
func TestLibraryListMarksAnUnknownContentType(t *testing.T) {
	h := newAPIHarness(t)
	h.mustRun("library", "add", "films", "--content-type", "movie", "--root", "/srv/a")
	h.exec(`INSERT INTO libraries (id, name, content_type, enabled, created_at)
		VALUES ('lib-legacy', 'tv', 'show', 1, '2026-08-01T00:00:00Z')`)

	stdout, stderr, err := h.run("library", "list")
	if err != nil {
		t.Fatalf("library list: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "show (unknown)") {
		t.Errorf("the table does not mark the unknown type:\n%s", stdout)
	}
	if strings.Contains(stdout, "movie (unknown)") {
		t.Errorf("the table marks a known type:\n%s", stdout)
	}
	if strings.Count(stderr, "warning:") != 1 {
		t.Fatalf("want exactly one warning on stderr, got:\n%s", stderr)
	}
	for _, want := range []string{`library "tv"`, `content type "show"`, "series", "#896"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the warning does not contain %q:\n%s", want, stderr)
		}
	}

	// --json is the row as the server has it, unannotated.
	for _, l := range listLibraries(t, h) {
		if strings.Contains(l.ContentType, "unknown") {
			t.Errorf("--json annotated a content type: %q", l.ContentType)
		}
	}
}
