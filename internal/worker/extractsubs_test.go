package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/ffmpeg"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

type fakeExtractor struct {
	streams []ffmpeg.SubtitleStream
	listErr error
	extract func(ffmpeg.SubtitleStream) (ffmpeg.Result, error)
}

func (f *fakeExtractor) List(context.Context, string) ([]ffmpeg.SubtitleStream, error) {
	return f.streams, f.listErr
}

func (f *fakeExtractor) Extract(_ context.Context, _ string, s ffmpeg.SubtitleStream) (ffmpeg.Result, error) {
	return f.extract(s)
}

type fakeSubStore struct {
	adopt   func() (string, int64, error)
	adopted int
}

func (f *fakeSubStore) SourcePath(_ context.Context, hash string) (string, error) {
	return "/src/" + hash, nil
}

func (f *fakeSubStore) Adopt(context.Context, string) (string, int64, error) {
	f.adopted++
	return f.adopt()
}

type fakeSubRecorder struct {
	recorded []catalog.ExtractedSubtitle
	err      error
}

func (f *fakeSubRecorder) RecordExtractedSubtitle(_ context.Context, _ string, sub catalog.ExtractedSubtitle, _ time.Time) error {
	if f.err != nil {
		return f.err
	}
	f.recorded = append(f.recorded, sub)
	return nil
}

func extractJob(t *testing.T, blob, asset string) jobs.Job {
	t.Helper()
	p, err := json.Marshal(ffmpeg.ExtractPayload{BlobHash: blob, AssetID: asset})
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{Payload: p}
}

func okResult() ffmpeg.Result { return ffmpeg.Result{Path: "/work/nonexistent.srt", Size: 42} }

func TestExtractSubsNoTracksIsACleanNoOp(t *testing.T) {
	t.Parallel()
	store := &fakeSubStore{adopt: func() (string, int64, error) { return "blake3:x", 1, nil }}
	rec := &fakeSubRecorder{}
	h := ExtractSubsHandler(ExtractSubsHandlerOptions{
		Extractor: &fakeExtractor{streams: nil},
		Store:     store, Recorder: rec,
	})
	if err := h(context.Background(), extractJob(t, "blake3:v", "asset-1")); err != nil {
		t.Fatalf("no-op should succeed, got %v", err)
	}
	if len(rec.recorded) != 0 || store.adopted != 0 {
		t.Errorf("a video with no text tracks must adopt/record nothing, got adopted=%d recorded=%d", store.adopted, len(rec.recorded))
	}
}

func TestExtractSubsRecordsEachTrack(t *testing.T) {
	t.Parallel()
	n := 0
	store := &fakeSubStore{adopt: func() (string, int64, error) {
		n++
		return "blake3:sub" + string(rune('0'+n)), int64(100 + n), nil
	}}
	rec := &fakeSubRecorder{}
	h := ExtractSubsHandler(ExtractSubsHandlerOptions{
		Extractor: &fakeExtractor{
			streams: []ffmpeg.SubtitleStream{
				{Index: 2, Codec: "mov_text", Language: "eng"},
				{Index: 3, Codec: "mov_text", Language: "spa", Forced: true},
			},
			extract: func(ffmpeg.SubtitleStream) (ffmpeg.Result, error) { return okResult(), nil },
		},
		Store: store, Recorder: rec,
	})
	if err := h(context.Background(), extractJob(t, "blake3:v", "asset-1")); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(rec.recorded) != 2 {
		t.Fatalf("recorded %d subtitles, want 2", len(rec.recorded))
	}
	if rec.recorded[0].Language != "eng" || rec.recorded[1].Language != "spa" || !rec.recorded[1].Forced {
		t.Errorf("recorded subtitles lost their metadata: %+v", rec.recorded)
	}
}

func TestExtractSubsSkipsATrackThatCannotBeExtracted(t *testing.T) {
	t.Parallel()
	store := &fakeSubStore{adopt: func() (string, int64, error) { return "blake3:sub", 100, nil }}
	rec := &fakeSubRecorder{}
	h := ExtractSubsHandler(ExtractSubsHandlerOptions{
		Extractor: &fakeExtractor{
			streams: []ffmpeg.SubtitleStream{
				{Index: 2, Codec: "mov_text", Language: "eng"},
				{Index: 3, Codec: "ass", Language: "spa"},
			},
			extract: func(s ffmpeg.SubtitleStream) (ffmpeg.Result, error) {
				if s.Index == 2 {
					return ffmpeg.Result{}, errors.New("ffmpeg could not convert this track")
				}
				return okResult(), nil
			},
		},
		Store: store, Recorder: rec,
	})
	// A track ffmpeg cannot convert is permanent — it is skipped, the other is
	// still recorded, and the job SUCCEEDS rather than retrying forever.
	if err := h(context.Background(), extractJob(t, "blake3:v", "asset-1")); err != nil {
		t.Fatalf("a per-track extraction failure must not fail the job, got %v", err)
	}
	if len(rec.recorded) != 1 || rec.recorded[0].Language != "spa" {
		t.Errorf("the surviving track must be recorded, got %+v", rec.recorded)
	}
}

func TestExtractSubsRetriesOnInfrastructureFailure(t *testing.T) {
	t.Parallel()
	// Adopt (the store) failing is transient — the job must return an error so
	// the queue retries, unlike a per-track conversion failure.
	adoptErr := ExtractSubsHandler(ExtractSubsHandlerOptions{
		Extractor: &fakeExtractor{
			streams: []ffmpeg.SubtitleStream{{Index: 2, Codec: "mov_text"}},
			extract: func(ffmpeg.SubtitleStream) (ffmpeg.Result, error) { return okResult(), nil },
		},
		Store:    &fakeSubStore{adopt: func() (string, int64, error) { return "", 0, errors.New("disk full") }},
		Recorder: &fakeSubRecorder{},
	})(context.Background(), extractJob(t, "blake3:v", "asset-1"))
	if adoptErr == nil {
		t.Error("an adopt failure must return an error so the queue retries")
	}

	recErr := ExtractSubsHandler(ExtractSubsHandlerOptions{
		Extractor: &fakeExtractor{
			streams: []ffmpeg.SubtitleStream{{Index: 2, Codec: "mov_text"}},
			extract: func(ffmpeg.SubtitleStream) (ffmpeg.Result, error) { return okResult(), nil },
		},
		Store:    &fakeSubStore{adopt: func() (string, int64, error) { return "blake3:sub", 100, nil }},
		Recorder: &fakeSubRecorder{err: errors.New("db down")},
	})(context.Background(), extractJob(t, "blake3:v", "asset-1"))
	if recErr == nil {
		t.Error("a recorder failure must return an error so the queue retries")
	}
}

func TestExtractSubsRejectsAnEmptyPayload(t *testing.T) {
	t.Parallel()
	h := ExtractSubsHandler(ExtractSubsHandlerOptions{
		Extractor: &fakeExtractor{}, Store: &fakeSubStore{}, Recorder: &fakeSubRecorder{},
	})
	if err := h(context.Background(), extractJob(t, "", "")); err == nil {
		t.Error("a payload naming no blob or asset must be rejected")
	}
}
