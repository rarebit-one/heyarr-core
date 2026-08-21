package downloads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The fake download client. It writes REAL bytes, and that is the point: a fake
// that reported "done" without producing a file would let the whole ingest path
// be tested against something that is not there.

func TestTheFakeWritesRealBytes(t *testing.T) {
	dir := t.TempDir()
	f := NewFake("fake", dir)
	ctx := context.Background()

	content := []byte("these are real bytes on a real filesystem")
	f.Queue("abc", "Film.mkv", content)

	// Before completion there is no path, matching a real client with
	// incomplete-dir enabled: the bytes are not where they will end up.
	transfers, err := f.Transfers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(transfers) != 1 {
		t.Fatalf("%d transfers", len(transfers))
	}
	if transfers[0].Path != "" {
		t.Errorf("a mid-transfer path was reported: %q", transfers[0].Path)
	}
	if transfers[0].Done {
		t.Error("the transfer has not completed")
	}

	done, err := f.Complete("abc")
	if err != nil {
		t.Fatal(err)
	}
	if !done.Done || done.Path == "" {
		t.Fatalf("a completed transfer needs a path: %+v", done)
	}

	// The file is genuinely there, hardlinkable and hashable like any other.
	got, err := os.ReadFile(filepath.Clean(done.Path))
	if err != nil {
		t.Fatalf("the fake reported completion without producing a file: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}
	if done.BytesDone != done.BytesTotal {
		t.Errorf("a completed transfer reports %d of %d bytes", done.BytesDone, done.BytesTotal)
	}
}

// Progress moves without completing, and still reports no path.
func TestTheFakeReportsProgressWithoutAPath(t *testing.T) {
	f := NewFake("fake", t.TempDir())
	f.Queue("abc", "Film.mkv", make([]byte, 100))

	if err := f.Progress("abc", 40); err != nil {
		t.Fatal(err)
	}
	transfers, err := f.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if transfers[0].BytesDone != 40 {
		t.Errorf("bytes done = %d, want 40", transfers[0].BytesDone)
	}
	if transfers[0].Path != "" || transfers[0].Done {
		t.Errorf("a partial transfer looks complete: %+v", transfers[0])
	}
}

// Failure is reported in the shape the real client produces, so a caller cannot
// come to depend on something only the fake does.
func TestTheFakeReportsTroubleTheSameWayTheRealClientDoes(t *testing.T) {
	f := NewFake("fake", t.TempDir())
	f.Queue("abc", "Film.mkv", nil)

	if err := f.Fail("abc", TroubleTrackerUnreachable, "every tracker failed to answer"); err != nil {
		t.Fatal(err)
	}
	transfers, err := f.Transfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if transfers[0].Error == "" {
		t.Fatal("no trouble reported")
	}
	// Same prefix the real client uses, so downstream branching works against
	// either.
	if got := transfers[0].Error; got[:len(TroubleTrackerUnreachable)] != string(TroubleTrackerUnreachable) {
		t.Errorf("error = %q, want it prefixed with the machine-readable reason", got)
	}
}

// The fake refuses what it does not hold, exactly as the real client does: a
// caller must not be able to reach past what this client queued.
func TestTheFakeRefusesAForeignTransfer(t *testing.T) {
	f := NewFake("fake", t.TempDir())
	ctx := context.Background()

	for _, err := range []error{
		f.Remove(ctx, "not-mine", true),
		f.Progress("not-mine", 1),
		f.Fail("not-mine", TroubleClientError, "x"),
	} {
		if !errors.Is(err, ErrNotOurs) {
			t.Errorf("expected ErrNotOurs, got %v", err)
		}
	}
	if _, err := f.Complete("not-mine"); !errors.Is(err, ErrNotOurs) {
		t.Errorf("Complete: expected ErrNotOurs, got %v", err)
	}
}

// Removal with delete-local-data removes the bytes; without it, it does not.
// Removal must never delete data Heyarr has not yet ingested, and that is the
// caller's judgement to state.
func TestTheFakeSeparatesRemovalFromDeletion(t *testing.T) {
	ctx := context.Background()

	t.Run("keeping the data", func(t *testing.T) {
		f := NewFake("fake", t.TempDir())
		f.Queue("abc", "Film.mkv", []byte("bytes"))
		done, err := f.Complete("abc")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Remove(ctx, "abc", false); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(done.Path); err != nil {
			t.Errorf("the bytes were deleted by a removal that did not ask for it: %v", err)
		}
	})

	t.Run("deleting the data", func(t *testing.T) {
		f := NewFake("fake", t.TempDir())
		f.Queue("abc", "Film.mkv", []byte("bytes"))
		done, err := f.Complete("abc")
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Remove(ctx, "abc", true); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(done.Path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the bytes survived a deletion that asked for it: %v", err)
		}
	})
}
