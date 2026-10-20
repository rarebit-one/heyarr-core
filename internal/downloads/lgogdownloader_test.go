package downloads

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// fakeGOG stands in for lgogdownloader: Download writes the package files under
// dir/<game>/ the way the real tool's %gamename% subdirectory does, and Status
// answers with whatever codes the test chose.
type fakeGOG struct {
	mu        sync.Mutex
	downloads int
	files     []string
	codes     map[string]string // file -> status code; missing means OK
	dlErr     error
	gate      chan struct{} // when set, Download waits for it to close
}

func (f *fakeGOG) Download(_ context.Context, dir, game string) error {
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	f.downloads++
	f.mu.Unlock()
	if f.dlErr != nil {
		return f.dlErr
	}
	for _, name := range f.files {
		p := filepath.Join(dir, game, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(name), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeGOG) Status(_ context.Context, _, game string) ([]GOGFileStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]GOGFileStatus, 0, len(f.files))
	for _, name := range f.files {
		code := "OK"
		if c, ok := f.codes[name]; ok {
			code = c
		}
		out = append(out, GOGFileStatus{Code: code, File: filepath.Base(name), Size: int64(len(name))})
	}
	return out, nil
}

func newGOGClient(t *testing.T, r GOGRunner) (*GOGClient, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := NewGOG(GOGOptions{Name: "gog", Dir: dir, Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

func waitGOG(t *testing.T, c *GOGClient, id string) providers.Transfer {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		transfers, err := c.Transfers(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range transfers {
			if tr.ID == id && (tr.Done || tr.Error != "") {
				return tr
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("transfer %s never finished", id)
	return providers.Transfer{}
}

var gogPackage = []string{
	"setup_some_game_1.2_(1234).exe",
	"setup_some_game_1.2_(1234)-1.bin",
	"setup_some_game_1.2_(1234)-2.bin",
	"dlc/some_game_extra/setup_some_game_extra_1.0_(99).exe",
}

// A package GOG vouches for, file by file, completes as one directory the
// bundle ingest can walk — the game's own subdirectory, holding every part.
func TestAVerifiedGOGPackageCompletesAsItsDirectory(t *testing.T) {
	r := &fakeGOG{files: gogPackage}
	c, dir := newGOGClient(t, r)

	tr, err := c.Add(context.Background(), secret.Value("gog:some_game"))
	if err != nil {
		t.Fatal(err)
	}
	got := waitGOG(t, c, tr.ID)
	if !got.Done || got.Error != "" {
		t.Fatalf("done=%v error=%q, want a completed transfer", got.Done, got.Error)
	}
	want := filepath.Join(dir, "gog-some_game", "some_game")
	if got.Path != want {
		t.Errorf("path = %q, want the game directory %q", got.Path, want)
	}
	for _, f := range gogPackage {
		if _, err := os.Stat(filepath.Join(want, f)); err != nil {
			t.Errorf("package file %s is not under the transfer path: %v", f, err)
		}
	}
	// What GOG vouched for is left beside the package for ingest to hold the
	// transfer to.
	raw, err := os.ReadFile(filepath.Join(want, acquisition.StoreManifestName))
	if err != nil {
		t.Fatalf("no manifest beside the verified package: %v", err)
	}
	var m acquisition.StoreManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Store != "gog" || len(m.Files) != len(gogPackage) || m.Files[0].Size == 0 {
		t.Errorf("manifest %+v, want every GOG-verified file with its size", m)
	}
}

// One file GOG does not vouch for fails the whole transfer: a game with a part
// missing or corrupt is not a copy of the game, and must never reach ingest as
// if it were.
func TestAnUnverifiedGOGPackageFailsTheTransfer(t *testing.T) {
	cases := map[string]map[string]string{
		"a checksum mismatch":         {gogPackage[1]: "MD5"},
		"a part never downloaded":     {gogPackage[2]: "ND"},
		"a short file and a mismatch": {gogPackage[1]: "FS", gogPackage[3]: "MD5"},
	}
	for name, codes := range cases {
		t.Run(name, func(t *testing.T) {
			c, _ := newGOGClient(t, &fakeGOG{files: gogPackage, codes: codes})
			tr, err := c.Add(context.Background(), secret.Value("gog:some_game"))
			if err != nil {
				t.Fatal(err)
			}
			got := waitGOG(t, c, tr.ID)
			if got.Done {
				t.Fatal("a package with an unverified file completed")
			}
			if !strings.Contains(got.Error, "GOG verification failed") {
				t.Errorf("error = %q, want it to name the failed verification", got.Error)
			}
			for _, code := range codes {
				if !strings.Contains(got.Error, code+"=") {
					t.Errorf("error = %q, want it to count %s", got.Error, code)
				}
			}
		})
	}
}

// GOG listing no files is not a verified package either.
func TestAGOGPackageWithNothingToVerifyFails(t *testing.T) {
	c, _ := newGOGClient(t, &fakeGOG{})
	tr, _ := c.Add(context.Background(), secret.Value("gog:some_game"))
	if got := waitGOG(t, c, tr.ID); got.Done || !strings.Contains(got.Error, "nothing was verified") {
		t.Errorf("done=%v error=%q, want a failure naming that nothing was verified", got.Done, got.Error)
	}
}

func TestAGOGDownloadFailureIsReportedOnTheTransfer(t *testing.T) {
	c, _ := newGOGClient(t, &fakeGOG{dlErr: errors.New("lgogdownloader failed to download the game")})
	tr, _ := c.Add(context.Background(), secret.Value("gog:some_game"))
	if got := waitGOG(t, c, tr.ID); got.Done || !strings.Contains(got.Error, "failed to download") {
		t.Errorf("done=%v error=%q", got.Done, got.Error)
	}
}

// The client takes only GOG-tagged sources with a well-formed gamename. The
// gamename becomes a regular expression for the store tool, so anything that
// could widen it is refused rather than escaped.
func TestTheGOGClientRefusesWhatIsNotItsToTake(t *testing.T) {
	c, _ := newGOGClient(t, &fakeGOG{files: gogPackage})
	for _, src := range []string{
		"https://example.invalid/setup.exe",
		"magnet:?xt=urn:btih:abc",
		"ytdlp:https://example.invalid/watch?v=x",
		"gog:",
		"gog:.*",
		"gog:some_game|other_game",
		"gog:Some_Game",
		"gog:some game",
		"gog:some_game@",
		"gog:some_game@a want",
		"gog:some_game@x@y",
	} {
		if _, err := c.Add(context.Background(), secret.Value(src)); !errors.Is(err, ErrNotGOGSource) {
			t.Errorf("Add(%q) err = %v, want ErrNotGOGSource", src, err)
		}
	}
}

// A re-run grab gets the same transfer and does not download the game twice.
// A re-run grab while the fetch is in flight gets the same transfer and starts
// nothing (invariant 9). Once a transfer is over, the next Add is a new attempt
// — the earlier package did not stick — and fetches (and verifies) again rather
// than replaying a finished transfer ingest already refused.
func TestAGOGTransferIsIdempotentInFlightAndFetchedAgainWhenOver(t *testing.T) {
	gate := make(chan struct{})
	r := &fakeGOG{files: gogPackage, gate: gate}
	c, _ := newGOGClient(t, r)
	src := secret.Value("gog:some_game@want-1")
	first, _ := c.Add(context.Background(), src)
	second, _ := c.Add(context.Background(), src)
	if first.ID != second.ID {
		t.Errorf("ids %q and %q; a re-run grab must get the same transfer", first.ID, second.ID)
	}
	close(gate)
	waitGOG(t, c, first.ID)
	r.mu.Lock()
	if r.downloads != 1 {
		t.Errorf("downloaded %d times in flight, want once", r.downloads)
	}
	r.mu.Unlock()

	again, _ := c.Add(context.Background(), src)
	if got := waitGOG(t, c, again.ID); !got.Done {
		t.Fatalf("the next attempt did not complete: %q", got.Error)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.downloads != 2 {
		t.Errorf("downloaded %d times; a finished transfer must be fetched again on the next attempt", r.downloads)
	}
}

// Each want's release is its own transfer: two profiles wanting one game never
// share — and strand — one acquisition.
func TestEachWantsGOGReleaseIsItsOwnTransfer(t *testing.T) {
	c, _ := newGOGClient(t, &fakeGOG{files: gogPackage})
	a, _ := c.Add(context.Background(), secret.Value("gog:some_game@want-1"))
	b, _ := c.Add(context.Background(), secret.Value("gog:some_game@want-2"))
	if a.ID == b.ID {
		t.Errorf("two wants share transfer %q", a.ID)
	}
	// Let both fetches finish before the temp directory is cleaned up under them.
	waitGOG(t, c, a.ID)
	waitGOG(t, c, b.ID)
}

func TestParseGOGStatusReadsOnlyFileLines(t *testing.T) {
	out := strings.Join([]string{
		"Getting game info 1 / 1",
		"OK some_game setup_some_game_1.2_(1234).exe 1048576 0123abcd",
		"MD5 some_game setup_some_game_1.2_(1234)-1.bin 4294967296 4567ef01",
		// A file never downloaded has no size or hash (lgogdownloader's
		// addStatusLine omits both).
		"ND some_game_extra setup_some_game_extra_1.0_(99).exe",
		"",
		"OKAY not a status line",
	}, "\n")
	got := ParseGOGStatus(out)
	want := []GOGFileStatus{
		{Code: "OK", File: "setup_some_game_1.2_(1234).exe", Size: 1048576},
		{Code: "MD5", File: "setup_some_game_1.2_(1234)-1.bin", Size: 4294967296},
		{Code: "ND", File: "setup_some_game_extra_1.0_(99).exe"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d lines %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A failed fetch is over, and the store schedule's next attempt must really
// fetch again — not replay the old error, which would leave the game
// unrecoverable until a restart.
func TestAFailedGOGFetchIsRetriedByTheNextAdd(t *testing.T) {
	r := &fakeGOG{files: gogPackage, codes: map[string]string{gogPackage[1]: "MD5"}}
	c, _ := newGOGClient(t, r)
	first, _ := c.Add(context.Background(), secret.Value("gog:some_game"))
	if got := waitGOG(t, c, first.ID); got.Error == "" {
		t.Fatal("fixture: the first fetch should have failed verification")
	}

	r.mu.Lock()
	r.codes = nil // the next attempt downloads a good copy
	r.mu.Unlock()
	second, err := c.Add(context.Background(), secret.Value("gog:some_game"))
	if err != nil {
		t.Fatal(err)
	}
	if got := waitGOG(t, c, second.ID); !got.Done || got.Error != "" {
		t.Fatalf("retry: done=%v error=%q, want a fresh fetch that completes", got.Done, got.Error)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.downloads != 2 {
		t.Errorf("downloaded %d times, want a second download on retry", r.downloads)
	}
}
