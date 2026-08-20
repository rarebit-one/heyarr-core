package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/testutil/fixtures"
)

// fakeRemuxStore stands in for the CAS.
type fakeRemuxStore struct {
	src       string
	adopted   []string
	adoptErr  error
	sourceErr error
}

func (f *fakeRemuxStore) SourcePath(context.Context, string) (string, error) {
	if f.sourceErr != nil {
		return "", f.sourceErr
	}
	return f.src, nil
}

func (f *fakeRemuxStore) Adopt(_ context.Context, path string) (string, int64, error) {
	if f.adoptErr != nil {
		return "", 0, f.adoptErr
	}
	f.adopted = append(f.adopted, path)
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	return "blake3:derived", info.Size(), nil
}

type fakeDerivedRecorder struct {
	calls []string
	err   error
}

func (f *fakeDerivedRecorder) RecordDerived(
	_ context.Context, sourceAssetID, blobHash string, _ int64, container string, _ time.Time,
) error {
	if f.err != nil {
		return f.err
	}
	f.calls = append(f.calls, sourceAssetID+"|"+blobHash+"|"+container)
	return nil
}

func ffmpegBinary(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", ".toolchain", "bin", "ffmpeg"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skip("no ffmpeg available; run scripts/toolchain.sh")
	}
	return abs
}

// mkvFixture is a real Matroska file with encoded streams.
func mkvFixture(t *testing.T) []byte {
	t.Helper()
	return fixtures.SampleMKV(1)
}

func remuxHarness(t *testing.T) (*ffmpeg.Remuxer, string) {
	t.Helper()
	dir := filepath.Join("..", "..", ".toolchain", "bin")
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(abs, "ffmpeg")
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no ffmpeg available; run scripts/toolchain.sh")
	}
	r, err := ffmpeg.New(ffmpeg.Options{FFmpegPath: bin, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return r, bin
}

func TestRemuxHandlerAdoptsTheOutputAndRecordsIt(t *testing.T) {
	remuxer, _ := remuxHarness(t)
	src := filepath.Join(t.TempDir(), "in.mkv")
	if err := os.WriteFile(src, mkvFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}

	store := &fakeRemuxStore{src: src}
	recorder := &fakeDerivedRecorder{}
	handler := RemuxHandler(RemuxHandlerOptions{
		Remuxer: remuxer, Store: store, Recorder: recorder,
	})

	payload, _ := json.Marshal(ffmpeg.Payload{
		BlobHash: "blake3:source", AssetID: "asset-1", Container: ffmpeg.ContainerMP4,
	})
	if err := handler(t.Context(), jobs.Job{Type: ffmpeg.JobType, Payload: payload}); err != nil {
		t.Fatalf("the handler failed: %v", err)
	}

	if len(store.adopted) != 1 {
		t.Fatalf("adopted %d outputs, want 1", len(store.adopted))
	}
	// The store hashes what it takes in (invariant 1). A remux output is not
	// exempt from having its bytes identified just because we produced it.
	if len(recorder.calls) != 1 || recorder.calls[0] != "asset-1|blake3:derived|mp4" {
		t.Errorf("recorded %v", recorder.calls)
	}
	// Nothing is left in the work directory: a leftover output is storage that
	// grows with every retry and belongs to nobody.
	if _, err := os.Stat(store.adopted[0]); !os.IsNotExist(err) {
		t.Errorf("the remux output was not cleaned up: %v", err)
	}
}

// A failure between remuxing and adopting must leave nothing behind.
func TestAFailedAdoptionLeavesNoFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "in.mkv")
	if err := os.WriteFile(src, mkvFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	remuxer, err := ffmpeg.New(ffmpeg.Options{
		FFmpegPath: ffmpegBinary(t), WorkDir: workDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	store := &fakeRemuxStore{src: src, adoptErr: errors.New("the store is full")}
	handler := RemuxHandler(RemuxHandlerOptions{
		Remuxer: remuxer, Store: store, Recorder: &fakeDerivedRecorder{},
	})
	payload, _ := json.Marshal(ffmpeg.Payload{
		BlobHash: "blake3:source", AssetID: "asset-1", Container: ffmpeg.ContainerMP4,
	})
	if err := handler(t.Context(), jobs.Job{Payload: payload}); err == nil {
		t.Fatal("a failed adoption was reported as success")
	}

	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed adoption left %d files behind", len(entries))
	}
}

func TestRemuxHandlerRefusesNonsense(t *testing.T) {
	handler := RemuxHandler(RemuxHandlerOptions{})
	for _, tc := range []struct{ name, payload string }{
		{"not json", `{{{`},
		{"no blob", `{"asset_id":"a1","container":"mp4"}`},
		{"no asset", `{"blob_hash":"blake3:x","container":"mp4"}`},
		{"an unsupported container", `{"blob_hash":"blake3:x","asset_id":"a1","container":"avi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := handler(t.Context(), jobs.Job{Payload: []byte(tc.payload)}); err == nil {
				t.Error("nonsense was accepted")
			}
		})
	}
}
