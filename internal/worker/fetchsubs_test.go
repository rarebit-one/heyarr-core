package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The fetch_subtitle handler drives a FAKE SubtitleProvider (ADR-0026: the real
// service is never hit in CI), asserting it selects the best candidate, adopts
// the bytes and records the subtitle — and that a miss or a provider error backs
// the want off rather than failing.

// fakeSubtitleProvider is an in-memory providers.SubtitleProvider.
type fakeSubtitleProvider struct {
	name       string
	candidates []providers.SubtitleCandidate
	findErr    error
	link       providers.SubtitleLink
	resolveErr error
	finds      int
	resolves   int
	lastQuery  providers.SubtitleQuery
	lastFileID string
}

func (f *fakeSubtitleProvider) Name() string { return f.name }
func (f *fakeSubtitleProvider) Capabilities() []providers.Capability {
	return []providers.Capability{providers.CapabilitySubtitle}
}

func (f *fakeSubtitleProvider) Check(context.Context) providers.Health {
	return providers.Healthy("test", time.Now())
}

func (f *fakeSubtitleProvider) FindSubtitle(_ context.Context, q providers.SubtitleQuery) ([]providers.SubtitleCandidate, error) {
	f.finds++
	f.lastQuery = q
	return f.candidates, f.findErr
}

func (f *fakeSubtitleProvider) ResolveSubtitle(_ context.Context, fileID string) (providers.SubtitleLink, error) {
	f.resolves++
	f.lastFileID = fileID
	return f.link, f.resolveErr
}

// recordingRecorder captures what the handler asked the catalog to do.
type recordingRecorder struct {
	ctx       catalog.SubtitleFetchContext
	ctxOK     bool
	ctxErr    error
	recorded  *catalog.FetchedSubtitle
	recordFor string
	recordErr error
	scheduled []scheduledFetch
	schedErr  error
}

type scheduledFetch struct {
	id        string
	fruitless int
}

func (r *recordingRecorder) SubtitleFetchContext(context.Context, string) (catalog.SubtitleFetchContext, bool, error) {
	return r.ctx, r.ctxOK, r.ctxErr
}

func (r *recordingRecorder) RecordFetchedSubtitle(_ context.Context, sourceAssetID string, sub catalog.FetchedSubtitle, _ time.Time) error {
	if r.recordErr != nil {
		return r.recordErr
	}
	r.recorded = &sub
	r.recordFor = sourceAssetID
	return nil
}

func (r *recordingRecorder) RecordSubtitleFetchScheduled(_ context.Context, id string, fruitless int, _, _ time.Time) error {
	if r.schedErr != nil {
		return r.schedErr
	}
	r.scheduled = append(r.scheduled, scheduledFetch{id: id, fruitless: fruitless})
	return nil
}

type stringStore struct {
	hash string
	size int64
	got  string
}

func (s *stringStore) Put(_ context.Context, r io.Reader) (string, int64, error) {
	b, _ := io.ReadAll(r)
	s.got = string(b)
	return s.hash, s.size, nil
}

type stringDownloader struct {
	body    string
	fetched string
	err     error
}

func (d *stringDownloader) Fetch(_ context.Context, u secret.Value) (io.ReadCloser, error) {
	if d.err != nil {
		return nil, d.err
	}
	d.fetched = u.Reveal()
	return io.NopCloser(strings.NewReader(d.body)), nil
}

func fetchJob(t *testing.T, wantID string) jobs.Job {
	t.Helper()
	p, err := json.Marshal(acquisition.FetchSubtitlePayload{DesiredItemID: wantID})
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{Type: acquisition.FetchSubtitleJobType, Payload: p}
}

func baseCtx() catalog.SubtitleFetchContext {
	return catalog.SubtitleFetchContext{
		DesiredItemID: "want-1", Language: "en", IMDBID: "0944947",
		Season: 1, Episode: 1, SourceVideoAssetID: "video-1",
	}
}

func TestFetchSubsAttachesBestCandidate(t *testing.T) {
	rec := &recordingRecorder{ctx: baseCtx(), ctxOK: true}
	prov := &fakeSubtitleProvider{
		name: "opensubtitles",
		candidates: []providers.SubtitleCandidate{
			{FileID: "1", Language: "en", HearingImpaired: true, DownloadCount: 5000},
			{FileID: "2", Language: "en", HearingImpaired: false, DownloadCount: 100},
			{FileID: "3", Language: "de", HearingImpaired: false, DownloadCount: 9000},
		},
		link: providers.SubtitleLink{URL: secret.Value("https://dl/x.srt"), Remaining: 90},
	}
	store := &stringStore{hash: "blake3:abc", size: 42}
	dl := &stringDownloader{body: "1\n00:00:01,000 --> 00:00:02,000\nhi\n"}

	h := FetchSubsHandler(FetchSubsHandlerOptions{
		Providers: []providers.SubtitleProvider{prov}, Recorder: rec, Store: store, Downloader: dl,
	})
	if err := h(context.Background(), fetchJob(t, "want-1")); err != nil {
		t.Fatalf("handler: %v", err)
	}

	// Candidate 2: English, not hearing-impaired, chosen over the more-downloaded
	// SDH (1) and the German (3, wrong language).
	if prov.lastFileID != "2" {
		t.Errorf("resolved file %q, want 2 (clean English)", prov.lastFileID)
	}
	if dl.fetched != "https://dl/x.srt" {
		t.Errorf("downloaded %q", dl.fetched)
	}
	if rec.recorded == nil || rec.recorded.BlobHash != "blake3:abc" || rec.recordFor != "video-1" {
		t.Fatalf("recorded = %+v for %q", rec.recorded, rec.recordFor)
	}
	if rec.recorded.Language != "en" {
		t.Errorf("recorded language = %q, want en", rec.recorded.Language)
	}
	// The query carried the ids the want had.
	if prov.lastQuery.IMDBID != "0944947" || prov.lastQuery.Season != 1 || prov.lastQuery.Episode != 1 {
		t.Errorf("query = %+v", prov.lastQuery)
	}
	// Success resets the fruitless streak.
	if len(rec.scheduled) != 1 || rec.scheduled[0].fruitless != 0 {
		t.Errorf("schedule = %+v, want one reset to 0", rec.scheduled)
	}
}

func TestFetchSubsBacksOffWhenNothingFound(t *testing.T) {
	rec := &recordingRecorder{ctx: catalog.SubtitleFetchContext{
		DesiredItemID: "want-1", Language: "en", IMDBID: "0944947",
		SourceVideoAssetID: "video-1", Fruitless: 2,
	}, ctxOK: true}
	prov := &fakeSubtitleProvider{name: "opensubtitles"} // no candidates
	h := FetchSubsHandler(FetchSubsHandlerOptions{
		Providers: []providers.SubtitleProvider{prov}, Recorder: rec,
		Store: &stringStore{}, Downloader: &stringDownloader{},
	})
	if err := h(context.Background(), fetchJob(t, "want-1")); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if rec.recorded != nil {
		t.Fatal("recorded a subtitle when none was found")
	}
	if len(rec.scheduled) != 1 || rec.scheduled[0].fruitless != 3 {
		t.Errorf("schedule = %+v, want one backoff to fruitless 3", rec.scheduled)
	}
}

func TestFetchSubsBacksOffOnProviderError(t *testing.T) {
	rec := &recordingRecorder{ctx: baseCtx(), ctxOK: true}
	prov := &fakeSubtitleProvider{name: "opensubtitles", findErr: errors.New("quota spent")}
	h := FetchSubsHandler(FetchSubsHandlerOptions{
		Providers: []providers.SubtitleProvider{prov}, Recorder: rec,
		Store: &stringStore{}, Downloader: &stringDownloader{},
	})
	// A provider error is not a job failure — it backs off and returns nil.
	if err := h(context.Background(), fetchJob(t, "want-1")); err != nil {
		t.Fatalf("handler returned an error for a provider failure: %v", err)
	}
	if len(rec.scheduled) != 1 || rec.scheduled[0].fruitless != 1 {
		t.Errorf("schedule = %+v, want one backoff", rec.scheduled)
	}
}

func TestFetchSubsNoOpWhenIneligible(t *testing.T) {
	rec := &recordingRecorder{ctxOK: false} // source video gone / not a subtitle want
	prov := &fakeSubtitleProvider{name: "opensubtitles"}
	h := FetchSubsHandler(FetchSubsHandlerOptions{
		Providers: []providers.SubtitleProvider{prov}, Recorder: rec,
		Store: &stringStore{}, Downloader: &stringDownloader{},
	})
	if err := h(context.Background(), fetchJob(t, "want-1")); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if prov.finds != 0 || len(rec.scheduled) != 0 {
		t.Error("an ineligible want should do nothing")
	}
}

func TestFetchSubsStoreFailureIsRetryable(t *testing.T) {
	rec := &recordingRecorder{ctx: baseCtx(), ctxOK: true}
	prov := &fakeSubtitleProvider{
		name:       "opensubtitles",
		candidates: []providers.SubtitleCandidate{{FileID: "2", Language: "en"}},
		link:       providers.SubtitleLink{URL: secret.Value("https://dl/x.srt")},
	}
	store := &failingStore{err: errors.New("disk full")}
	h := FetchSubsHandler(FetchSubsHandlerOptions{
		Providers: []providers.SubtitleProvider{prov}, Recorder: rec,
		Store: store, Downloader: &stringDownloader{body: "x"},
	})
	// A store failure is the transient kind a queue retry is for — returned.
	if err := h(context.Background(), fetchJob(t, "want-1")); err == nil {
		t.Fatal("a store failure should return an error for the queue to retry")
	}
}

type failingStore struct{ err error }

func (s *failingStore) Put(context.Context, io.Reader) (string, int64, error) {
	return "", 0, s.err
}
