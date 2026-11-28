package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/rarebit-one/heyarr-core/internal/api/resources"
	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/void-which-binds-go/hashing"
)

// This test uses only generated tones; no copyrighted recordings or network
// services are involved. It exercises the real ffprobe binary, not a stub.
func TestRecordingIngestWithRealFFmpegSelectsPlaybackBytes(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg unavailable")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe unavailable")
	}
	h := newIngestHarness(t)
	h.exec(t, `UPDATE works SET content_type='music',work_key='music-recording:11111111-1111-4111-8111-111111111111',title='Jump',attributes='{"artist":"Van Halen","recording_id":"11111111-1111-4111-8111-111111111111"}' WHERE id='w1'`)
	h.exec(t, `UPDATE libraries SET content_type='music' WHERE id='lib1'`)
	candidate := offer("good", 2160, "hevc")
	candidate.Title = "Van Halen - Greatest Hits 1996"
	h.fake.Offer("Jump", candidate)
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Fatalf("state %s", got)
	}
	pack := filepath.Join(h.downloads, "album")
	if err := os.MkdirAll(pack, 0755); err != nil {
		t.Fatal(err)
	}
	for _, track := range []struct{ file, artist, freq string }{{"01-wrong.flac", "Another Artist", "440"}, {"02-matching.flac", "Van Halen", "880"}} {
		cmd := exec.CommandContext(t.Context(), ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency="+track.freq+":duration=1", "-metadata", "artist="+track.artist, "-metadata", "title=Jump", "-y", filepath.Join(pack, track.file))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generating audio: %v: %s", err, out)
		}
	}
	expected, err := os.ReadFile(filepath.Join(pack, "02-matching.flac"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.cat.RecordAcquisition(ctx, catalog.Acquisition{ID: NewAcquisitionID(), DesiredItemID: h.want, Provider: "fake", ExternalID: "real-test", ExternalName: "album", RemotePath: pack, LocalPath: pack}); err != nil {
		t.Fatal(err)
	}
	for _, tr := range []acquisition.Transition{acquisition.TransitionQueue, acquisition.TransitionStartDownload, acquisition.TransitionDownloaded} {
		if _, err := h.cat.AdvanceAcquisition(ctx, h.want, tr, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.ingest(t); err != nil {
		t.Fatal(err)
	}
	log, err := events.New(events.Options{Writer: h.db.Writer(), Reader: h.db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewStore(auth.StoreOptions{Writer: h.db.Writer(), Reader: h.db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	api, err := resources.New(resources.Options{DB: h.db, Jobs: h.queue, Events: log, Tokens: tokens, Catalog: h.cat})
	if err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Route("/api/v1", api.Mount)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/works/w1", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("work response %d: %s", rr.Code, rr.Body.String())
	}
	var work struct {
		Primary *struct {
			AssetID  string `json:"asset_id"`
			BlobHash string `json:"blob_hash"`
		} `json:"primary_asset"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &work); err != nil {
		t.Fatal(err)
	}
	if work.Primary == nil || work.Primary.AssetID == "" {
		t.Fatalf("no playable primary: %s", rr.Body.String())
	}
	planBody := fmt.Sprintf(`{"asset_id":%q,"client":{"containers":["flac"],"audio":["flac"]}}`, work.Primary.AssetID)
	planReq := httptest.NewRequest(http.MethodPost, "/api/v1/playback/plan", strings.NewReader(planBody))
	planReq.Header.Set("Content-Type", "application/json")
	planRR := httptest.NewRecorder()
	router.ServeHTTP(planRR, planReq)
	if planRR.Code != http.StatusOK {
		t.Fatalf("playback plan %d: %s", planRR.Code, planRR.Body.String())
	}
	var plan struct {
		AssetID string `json:"asset_id"`
	}
	if err := json.Unmarshal(planRR.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.AssetID != work.Primary.AssetID {
		t.Fatalf("plan picked different asset: %s", planRR.Body.String())
	}
	hash, err := hashing.Parse(work.Primary.BlobHash)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.OpenFS(filepath.Join(filepath.Dir(h.downloads), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	reader, _, err := store.Open(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, expected) {
		t.Fatal("playback-selected blob differs from the matching generated track")
	}
}
