package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
	"github.com/rarebit-one/void-which-binds-go/hashing"
)

func TestSelectMusicArtifactChoosesUniqueTaggedFile(t *testing.T) {
	h := newIngestHarness(t)
	bin := t.TempDir()
	script := `#!/bin/sh
 for x do :; done
 read x < "$x"
 case "$x" in
  correct) printf '%s' '{"format":{"tags":{"artist":"Van Halen","title":"Jump"}}}' ;;
  wrong) printf '%s' '{"format":{"tags":{"artist":"Other","title":"Jump"}}}' ;;
  *) exit 1 ;;
 esac
 `
	if err := os.WriteFile(filepath.Join(bin, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path1 := filepath.Join(h.downloads, "1.flac")
	path2 := filepath.Join(h.downloads, "2.flac")
	pathLive := filepath.Join(h.downloads, "3 Jump Live.flac")
	for path, content := range map[string]string{path1: "wrong\n", path2: "correct\n", pathLive: "correct\n"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	batch := verifiedTransfer{contentType: "music", pack: true, artifacts: []artifact{{Path: path1, RelPath: "1.flac"}, {Path: path2, RelPath: "2.flac"}, {Path: pathLive, RelPath: "3 Jump Live.flac"}}}
	selected, err := selectMusicArtifact(context.Background(), h.pipeline, batch, "Jump", "Van Halen")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.artifacts) != 1 || selected.artifacts[0].Path != path2 || selected.artifacts[0].expectedBlobHash == "" {
		t.Fatalf("selection %+v", selected)
	}
	if err := os.WriteFile(path2, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = h.pipeline.Ingest(context.Background(), ingest.Request{RootID: "root1", SourcePath: path2, RelPath: "2.flac", ExpectedBlobHash: selected.artifacts[0].expectedBlobHash})
	if err == nil || !strings.Contains(err.Error(), "changed after inspection") {
		t.Fatalf("source replacement was not rejected: %v", err)
	}
}

func TestRecordingRequestSearchAndPackIngest(t *testing.T) {
	h := newIngestHarness(t)
	bin := t.TempDir()
	script := `#!/bin/sh
 for x do :; done
 read x < "$x"
 case "$x" in
  correct) printf '%s' '{"format":{"tags":{"artist":"Van Halen","title":"Jump"}}}' ;;
  wrong) printf '%s' '{"format":{"tags":{"artist":"Other","title":"Jump"}}}' ;;
  *) exit 1 ;;
 esac
 `
	if err := os.WriteFile(filepath.Join(bin, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.exec(t, `UPDATE works SET content_type='music', work_key='music-recording:11111111-1111-4111-8111-111111111111', title='Jump', attributes='{"artist":"Van Halen","recording_id":"11111111-1111-4111-8111-111111111111"}' WHERE id='w1'`)
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
	if err := os.MkdirAll(pack, 0o755); err != nil {
		t.Fatal(err)
	}
	for filename, data := range map[string]string{"01.flac": "wrong\n", "02.flac": "correct\n"} {
		if err := os.WriteFile(filepath.Join(pack, filename), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if _, err := h.cat.RecordAcquisition(ctx, catalog.Acquisition{ID: NewAcquisitionID(), DesiredItemID: h.want, Provider: "fake", ExternalID: "x", ExternalName: "album", RemotePath: pack, LocalPath: pack}); err != nil {
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
	var matched, method string
	if err := h.db.Reader().QueryRow(`SELECT json_extract(attributes,'$.metadata_matched_blob'),json_extract(attributes,'$.match_method') FROM works WHERE id='w1'`).Scan(&matched, &method); err != nil {
		t.Fatal(err)
	}
	if matched == "" || method != "embedded_tags" {
		t.Fatalf("metadata evidence %q %q", matched, method)
	}
	if n := h.count(t, `SELECT count(*) FROM assets a JOIN editions e ON e.id=a.edition_id WHERE e.work_id='w1' AND a.role='primary' AND a.blob_hash=?`, matched); n != 1 {
		t.Fatalf("matching primary count %d", n)
	}
	if n := h.count(t, `SELECT count(*) FROM assets a JOIN editions e ON e.id=a.edition_id WHERE e.work_id='w1' AND a.role='primary'`); n != 1 {
		t.Fatalf("pack exposed %d tracks", n)
	}
}

func TestMusicIngestRefusesCorruptDeduplicatedBlob(t *testing.T) {
	h := newIngestHarness(t)
	path := filepath.Join(h.downloads, "audio.flac")
	if err := os.WriteFile(path, []byte("original bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := cas.OpenFS(filepath.Join(filepath.Dir(h.downloads), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.Link(context.Background(), path, cas.Copy)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.LocalPath(context.Background(), blob.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stored, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored, []byte("corrupt bytes!"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = h.pipeline.Ingest(context.Background(), ingest.Request{RootID: "root1", SourcePath: path, RelPath: "audio.flac", ExpectedBlobHash: blob.Hash.String()})
	if err == nil || !strings.Contains(err.Error(), "stored bytes differ") {
		t.Fatalf("corrupt incumbent accepted: %v", err)
	}
	data, err := os.ReadFile(stored)
	if err != nil || string(data) != "corrupt bytes!" {
		t.Fatalf("corrupt incumbent was modified: %q %v", data, err)
	}
}

func TestMusicInspectionRejectsStageMutationAndBadAudio(t *testing.T) {
	bin := t.TempDir()
	probe := filepath.Join(bin, "ffprobe")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	path := filepath.Join(t.TempDir(), "audio.flac")
	if err := os.WriteFile(path, []byte("music"), 0o600); err != nil {
		t.Fatal(err)
	}
	mutation := `#!/bin/sh
for x do :; done
printf bad > "$x"
printf '%s' '{"format":{"tags":{"artist":"Van Halen","title":"Jump"}}}'
`
	if err := os.WriteFile(probe, []byte(mutation), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := inspectMusicMetadata(context.Background(), t.TempDir(), path)
	if err == nil || !strings.Contains(err.Error(), "staged bytes changed") {
		t.Fatalf("mutation not rejected: %v", err)
	}
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err = inspectMusicMetadata(context.Background(), t.TempDir(), path)
	if !errors.Is(err, errMusicMatch) {
		t.Fatalf("bad audio should be inconclusive: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	_, _, err = inspectMusicMetadata(context.Background(), t.TempDir(), path)
	if err == nil || errors.Is(err, errMusicMatch) {
		t.Fatalf("missing ffprobe should be retryable: %v", err)
	}
}

func TestMusicIngestRefusesDeduplicatedHardlink(t *testing.T) {
	h := newIngestHarness(t)
	path := filepath.Join(h.downloads, "audio.flac")
	if err := os.WriteFile(path, []byte("audio bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := cas.OpenFS(filepath.Join(filepath.Dir(h.downloads), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.Link(context.Background(), path, cas.Hardlink)
	if err != nil {
		t.Fatal(err)
	}
	req := ingest.Request{RootID: "root1", SourcePath: path, RelPath: "audio.flac", ExpectedBlobHash: blob.Hash.String()}
	_, err = h.pipeline.Ingest(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "cannot establish private blob") {
		t.Fatalf("shared hardlink accepted: %v", err)
	}
}

func TestMusicIngestResumesPrivateDeduplicatedBlob(t *testing.T) {
	h := newIngestHarness(t)
	path := filepath.Join(h.downloads, "audio.flac")
	if err := os.WriteFile(path, []byte("audio bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, _, err := hashing.HashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	req := ingest.Request{RootID: "root1", SourcePath: path, RelPath: "audio.flac", ExpectedBlobHash: hash.String()}
	first, err := h.pipeline.Ingest(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.pipeline.Ingest(context.Background(), req)
	if err != nil || first.AssetID != second.AssetID {
		t.Fatalf("resume did not converge: first=%+v second=%+v err=%v", first, second, err)
	}
}

func TestMusicSharedBlobFailsLocallyWithoutBlockingCandidate(t *testing.T) {
	h := newIngestHarness(t)
	bin := t.TempDir()
	script := `#!/bin/sh
printf '%s' '{"format":{"tags":{"artist":"Van Halen","title":"Jump"}}}'
`
	if err := os.WriteFile(filepath.Join(bin, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.exec(t, `UPDATE works SET content_type='music',work_key='music-recording:11111111-1111-4111-8111-111111111111',title='Jump',attributes='{"artist":"Van Halen","recording_id":"11111111-1111-4111-8111-111111111111"}' WHERE id='w1'`)
	h.exec(t, `UPDATE libraries SET content_type='music' WHERE id='lib1'`)
	candidate := offer("good", 2160, "hevc")
	candidate.Title = "Van Halen - Jump 1984"
	h.fake.Offer("Jump", candidate)
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.downloads, "track.flac")
	if err := os.WriteFile(path, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := cas.OpenFS(filepath.Join(filepath.Dir(h.downloads), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Link(context.Background(), path, cas.Hardlink); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := h.cat.RecordAcquisition(ctx, catalog.Acquisition{ID: NewAcquisitionID(), DesiredItemID: h.want, Provider: "fake", ExternalID: "x", ExternalName: "track", RemotePath: path, LocalPath: path}); err != nil {
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
	rec, err := h.cat.Acquisition(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State.Phase != acquisition.PhaseIdle || !strings.Contains(rec.Detail, "local music storage needs repair") {
		t.Fatalf("unexpected acquisition state: %+v", rec)
	}
	blocked, err := h.cat.BlockedKeys(ctx, h.want)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("local fault blocked release: %v %v", blocked, err)
	}
	held, err := h.cat.AcquisitionRepairHeld(ctx, h.want)
	if err != nil || !held {
		t.Fatalf("repair hold missing: %v %v", held, err)
	}
	if _, err := h.cat.BlockRelease(ctx, catalog.BlockedRelease{DesiredItemID: h.want, Provider: "fake", CandidateID: "good", Reason: catalog.BlockGrabFailed}); !errors.Is(err, catalog.ErrRepairHold) {
		t.Fatalf("held want allowed blocking: %v", err)
	}
	if err := h.cat.DropAcquisition(ctx, h.want); !errors.Is(err, catalog.ErrRepairHold) {
		t.Fatalf("held want allowed transfer drop: %v", err)
	}
	if _, err := h.cat.BlockTransferRelease(ctx, h.want, "fake", "x", []acquisition.Phase{acquisition.PhaseIdle}, "stale poll", catalog.BlockGrabFailed); !errors.Is(err, catalog.ErrRepairHold) {
		t.Fatalf("stale poll blocked held transfer: %v", err)
	}
	if _, err := h.cat.DropAcquisitionOf(ctx, h.want, "fake", "x"); !errors.Is(err, catalog.ErrRepairHold) {
		t.Fatalf("stale poll dropped held transfer: %v", err)
	}
	if _, err := h.cat.RecordAcquisition(ctx, catalog.Acquisition{DesiredItemID: h.want, Provider: "fake", ExternalID: "stale"}); !errors.Is(err, catalog.ErrRepairHold) {
		t.Fatalf("stale poll replaced held transfer: %v", err)
	}
	if _, err := h.cat.RecordSearch(ctx, h.want, nil, acquisition.Incumbent{}); !errors.Is(err, catalog.ErrRepairHold) {
		t.Fatalf("stale search altered held candidates: %v", err)
	}
	if _, err := h.cat.AdvanceAcquisition(ctx, h.want, acquisition.TransitionAdopt, "stale adoption"); !errors.Is(err, catalog.ErrRepairHold) {
		t.Fatalf("stale adoption bypassed hold: %v", err)
	}
	if _, err := h.cat.SelectByHand(ctx, h.want, "good", false); !errors.Is(err, catalog.ErrRepairHold) {
		t.Fatalf("manual selection bypassed hold: %v", err)
	}
	if _, err := h.cat.RecordDirectRelease(ctx, h.want, offer("direct", 2160, "hevc")); !errors.Is(err, catalog.ErrRepairHold) {
		t.Fatalf("direct release bypassed hold: %v", err)
	}
	due, err := h.cat.DueSearches(ctx, time.Now().Add(365*24*time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.DesiredItemID == h.want {
			t.Fatal("held want remained due for automatic search")
		}
	}
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	after, err := h.cat.Acquisition(ctx, h.want)
	if err != nil || after.Detail != rec.Detail {
		t.Fatalf("queued search erased repair detail: %+v %v", after, err)
	}
	changed, err := h.cat.RetryHeldIngest(ctx, h.want)
	if err != nil || !changed {
		t.Fatalf("manual retry failed: %v %v", changed, err)
	}
	after, err = h.cat.Acquisition(ctx, h.want)
	if err != nil || after.State.Phase != acquisition.PhaseVerifying {
		t.Fatalf("retry did not restore verifying: %+v %v", after, err)
	}
	held, err = h.cat.AcquisitionRepairHeld(ctx, h.want)
	if err != nil || held {
		t.Fatalf("retry did not clear hold: %v %v", held, err)
	}
}

func TestMusicCorruptionQueuesKnownBlobAndFlagsUntracked(t *testing.T) {
	h := newIngestHarness(t)
	ctx := context.Background()
	hash := "blake3:" + strings.Repeat("a", 64)
	detail, err := queueMusicIntegrityCheck(ctx, h.cat, h.queue, hash)
	if err != nil || !strings.Contains(detail, "untracked") {
		t.Fatalf("untracked result: %q %v", detail, err)
	}
	if n := h.count(t, `SELECT count(*) FROM jobs WHERE type='verify_blob'`); n != 0 {
		t.Fatalf("queued %d untracked jobs", n)
	}
	h.exec(t, `INSERT INTO blobs (hash,size,first_seen_at) VALUES (?,1,?)`, hash, time.Now().UTC().Format(time.RFC3339Nano))
	for i := 0; i < 2; i++ {
		detail, err = queueMusicIntegrityCheck(ctx, h.cat, h.queue, hash)
		if err != nil || !strings.Contains(detail, "verification queued") {
			t.Fatalf("tracked result: %q %v", detail, err)
		}
	}
	if n := h.count(t, `SELECT count(*) FROM jobs WHERE type='verify_blob'`); n != 1 {
		t.Fatalf("expected one deduplicated verify job, got %d", n)
	}
}

func TestMusicCorruptTrackedBlobQueuesIntegrityWithoutBlocking(t *testing.T) {
	h := newIngestHarness(t)
	bin := t.TempDir()
	script := `#!/bin/sh
printf '%s' '{"format":{"tags":{"artist":"Van Halen","title":"Jump"}}}'
`
	if err := os.WriteFile(filepath.Join(bin, "ffprobe"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h.exec(t, `UPDATE works SET content_type='music',work_key='music-recording:11111111-1111-4111-8111-111111111111',title='Jump',attributes='{"artist":"Van Halen","recording_id":"11111111-1111-4111-8111-111111111111"}' WHERE id='w1'`)
	h.exec(t, `UPDATE libraries SET content_type='music' WHERE id='lib1'`)
	candidate := offer("good", 2160, "hevc")
	candidate.Title = "Van Halen - Jump 1984"
	h.fake.Offer("Jump", candidate)
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.downloads, "track.flac")
	if err := os.WriteFile(path, []byte("good-audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := cas.OpenFS(filepath.Join(filepath.Dir(h.downloads), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.Link(context.Background(), path, cas.Copy)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.LocalPath(context.Background(), blob.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stored, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored, []byte("bad--audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `INSERT INTO blobs (hash,size,first_seen_at) VALUES (?,10,?)`, blob.Hash.String(), time.Now().UTC().Format(time.RFC3339Nano))
	ctx := context.Background()
	if _, err := h.cat.RecordAcquisition(ctx, catalog.Acquisition{ID: NewAcquisitionID(), DesiredItemID: h.want, Provider: "fake", ExternalID: "x", ExternalName: "track", RemotePath: path, LocalPath: path}); err != nil {
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
	rec, err := h.cat.Acquisition(ctx, h.want)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State.Phase != acquisition.PhaseIdle || !strings.Contains(rec.Detail, "integrity verification queued") {
		t.Fatalf("unexpected acquisition state: %+v", rec)
	}
	if n := h.count(t, `SELECT count(*) FROM jobs WHERE type='verify_blob'`); n != 1 {
		t.Fatalf("expected verify job, got %d", n)
	}
	blocked, err := h.cat.BlockedKeys(ctx, h.want)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("corruption blocked release: %v %v", blocked, err)
	}
}
