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
