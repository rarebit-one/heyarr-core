package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/identification"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// mzHeader is the start of a Windows program: what a fake release carries.
var mzHeader = append([]byte("MZ\x90\x00\x03\x00\x00\x00"), make([]byte, 64)...)

func TestSniffExecutable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]struct {
		path string
		want bool
	}{
		"an .exe by name":               {write("Show.S06E06.1080p.WEB.H264.exe", []byte("x")), true},
		"an upper-case .MSI":            {write("setup.MSI", []byte("x")), true},
		"a program renamed to .mkv":     {write("Show.S06E06.mkv", mzHeader), true},
		"an ELF renamed to .mp4":        {write("film.mp4", []byte("\x7fELF\x02\x01")), true},
		"a matroska file":               {write("real.mkv", []byte("\x1a\x45\xdf\xa3\x01")), false},
		"a one-byte file":               {write("tiny.mkv", []byte("M")), false},
		"a subtitle that starts with M": {write("subs.srt", []byte("Maybe not")), false},
	}
	for name, tc := range cases {
		if got := sniffExecutable(tc.path) != ""; got != tc.want {
			t.Errorf("%s: executable = %v, want %v", name, got, tc.want)
		}
	}
}

// A single-file release that is a program is refused with the stable reason,
// unless the want is a game, whose payload is installers by definition.
func TestASingleProgramIsRefusedUnlessTheWantIsAGame(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "setup_some_game_1.0_(1).exe")
	if err := os.WriteFile(p, mzHeader, 0o600); err != nil {
		t.Fatal(err)
	}
	acq := catalog.Acquisition{LocalPath: p}

	_, err := verifyArtifacts(acq, identification.Series)
	if err == nil || !strings.HasPrefix(err.Error(), "payload_executable: ") {
		t.Fatalf("a series want took a program: %v", err)
	}
	if isNodeLocal(err) {
		t.Fatal("a program is the release's fault and must block it, not take the node-local edge")
	}
	if _, err := verifyArtifacts(acq, identification.Game); err != nil {
		t.Fatalf("a game installer was refused: %v", err)
	}
}

// A pack holding only programs is named for what it is, and a program wearing
// a media extension is dropped from a pack that also holds real files.
func TestAPackOfProgramsIsRefused(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "Show.S06.1080p.WEB")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{
		"Show.S06E05.1080p.exe": []byte("x"),
		"Show.S06E06.mkv":       mzHeader,
		"readme.txt":            []byte("run the exe"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err := verifyDirectory(dir, identification.Series)
	if err == nil || !strings.HasPrefix(err.Error(), "payload_executable: ") {
		t.Fatalf("a pack of programs was not refused as one: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "Show.S06E07.mkv"), []byte("\x1a\x45\xdf\xa3real"), 0o600); err != nil {
		t.Fatal(err)
	}
	arts, err := verifyDirectory(dir, identification.Series)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 1 || filepath.Base(arts[0].Path) != "Show.S06E07.mkv" {
		t.Errorf("kept %+v, want only the real episode", arts)
	}
}

// End to end through the ingest job: a completed fake creates no asset, the
// want goes back to idle, and the release is blocked so the next search moves
// on.
func TestAnIngestOfAProgramBlocksTheRelease(t *testing.T) {
	t.Parallel()
	h := newIngestHarness(t)
	path := h.selectAndComplete(t, "Arrival.2016.1080p.WEB.H264.exe", []byte("x"))
	if err := os.WriteFile(path, mzHeader, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.ingest(t); err != nil {
		t.Fatalf("a program is an outcome, not a job failure: %v", err)
	}
	if st := h.state(t); st.Phase != acquisition.PhaseIdle || st.Managed {
		t.Fatalf("state = %+v, want idle and holding nothing", st)
	}
	if n := h.count(t, `SELECT count(*) FROM assets`); n != 0 {
		t.Fatalf("%d asset(s) were created from a program", n)
	}
	blocked, err := h.cat.BlockedFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 || !strings.HasPrefix(blocked[0].Detail, "payload_executable: ") {
		t.Fatalf("blocked = %+v, want the release blocked as payload_executable", blocked)
	}
}

// A program-only transfer is stopped as soon as the client knows its files:
// the want goes back to idle, the release is blocked, the search re-runs now,
// and the transfer leaves the client instead of seeding (#819).
func TestAProgramTransferIsStoppedAtPollTime(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.1080p.WEB.H264.exe", []byte("MZ a program"))
	id := h.transferID(t)
	if err := h.client.MarkExecutable(id); err != nil {
		t.Fatal(err)
	}
	h.poll(t)

	if st := h.state(t); st.Phase != acquisition.PhaseIdle || st.Managed {
		t.Fatalf("state = %+v, want idle and holding nothing", st)
	}
	blocked, err := h.cat.BlockedFor(t.Context(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 || blocked[0].Reason != catalog.BlockVerificationFailed ||
		!strings.HasPrefix(blocked[0].Detail, "payload_executable: ") {
		t.Fatalf("blocked = %+v, want the release blocked as payload_executable", blocked)
	}
	if _, found, err := h.cat.SearchSchedule(t.Context(), h.want); err != nil {
		t.Fatal(err)
	} else if found {
		t.Error("search schedule still present; the want must be due a fresh search")
	}
	left, err := h.client.Transfers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("the program is still in the client: %+v", left)
	}
}

// A game's payload is installers: the same verdict leaves a game want alone.
func TestAGameTransferOfInstallersIsNotStopped(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.1080p.WEB.H264.exe", []byte("MZ an installer"))
	h.exec(t, `UPDATE works SET content_type = 'game'`)
	if err := h.client.MarkExecutable(h.transferID(t)); err != nil {
		t.Fatal(err)
	}
	h.poll(t)
	if got := h.state(t).Phase; got != acquisition.PhaseQueued {
		t.Fatalf("phase = %s, want a game's grab left queued", got)
	}
	if blocked, err := h.cat.BlockedFor(t.Context(), h.want); err != nil || len(blocked) != 0 {
		t.Fatalf("blocked = %+v (%v), want nothing blocked for a game", blocked, err)
	}
}

// A removal that fails keeps the transfer row, so the next poll retries it
// rather than forgetting a program that is still seeding.
func TestAFailedRemovalOfAProgramIsRetried(t *testing.T) {
	t.Parallel()
	h := newPollHarness(t)
	h.grabAfterSearch(t, "Arrival.2016.1080p.WEB.H264.exe", []byte("MZ a program"))
	id := h.transferID(t)
	if err := h.client.MarkExecutable(id); err != nil {
		t.Fatal(err)
	}
	h.client.FailNextRemove(errors.New("the client is briefly unreachable"))
	h.poll(t)

	if got := h.state(t).Phase; got != acquisition.PhaseIdle {
		t.Fatalf("phase = %s, want idle", got)
	}
	if left, _ := h.client.Transfers(t.Context()); len(left) != 1 {
		t.Fatalf("setup: %d transfers, want the one whose removal failed", len(left))
	}
	if _, err := h.cat.AcquisitionFor(t.Context(), h.want); err != nil {
		t.Fatalf("the transfer row was dropped before the removal succeeded: %v", err)
	}

	h.poll(t)
	if left, _ := h.client.Transfers(t.Context()); len(left) != 0 {
		t.Errorf("the next poll did not retry the removal: %+v", left)
	}
	if blocked, _ := h.cat.BlockedFor(t.Context(), h.want); len(blocked) != 1 {
		t.Errorf("blocked = %+v, want the one block, not a second", blocked)
	}
}
