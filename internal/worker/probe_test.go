package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/domain/ingest"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
)

// fakeEnqueuer records what ingest queued.
type fakeEnqueuer struct {
	queued []jobs.EnqueueOptions
	err    error
}

func (f *fakeEnqueuer) Enqueue(_ context.Context, opts jobs.EnqueueOptions) (jobs.Job, error) {
	if f.err != nil {
		return jobs.Job{}, f.err
	}
	f.queued = append(f.queued, opts)
	return jobs.Job{ID: "j1"}, nil
}

// The join between ingest and probing. §66 puts probe in the pipeline, and a
// probe is a job (§75) — so what ingest does is queue one, with the capability
// that decides who may run it.
func TestIngestQueuesAProbeForMedia(t *testing.T) {
	for _, tc := range []struct {
		name    string
		relPath string
		dedup   bool
		want    bool
	}{
		{"a film", "films/Blue Harvest (2019)/Blue Harvest.mkv", false, true},
		{"an album track", "music/Artist/Album/01 - Track.flac", false, true},
		{"an audiobook", "books/Author/Title.m4b", false, true},
		// Probing these costs a subprocess and a job slot to learn nothing:
		// ffprobe describes a JPEG as a one-frame video stream, which is true,
		// useless, and then has to be reasoned about by the planner.
		{"cover art", "films/Blue Harvest (2019)/poster.jpg", false, false},
		{"a subtitle", "tv/Show/S01E01.en.srt", false, false},
		{"an ebook", "books/Author/Title.epub", false, false},
		{"something unrecognised", "films/notes.bin", false, false},
		// Already under management means already probed, or a probe already
		// pending under the same dedupe key.
		{"a deduplicated blob", "films/Copy/Copy.mkv", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeEnqueuer{}
			enqueueProbe(t.Context(), q, ingest.Result{
				BlobHash: "blake3:abc", BlobSize: 1234, Deduplicated: tc.dedup,
			}, tc.relPath)

			if tc.want && len(q.queued) != 1 {
				t.Fatalf("queued %d probes, want 1", len(q.queued))
			}
			if !tc.want {
				if len(q.queued) != 0 {
					t.Fatalf("queued a probe for %s", tc.relPath)
				}
				return
			}

			got := q.queued[0]
			if got.Type != probe.JobType {
				t.Errorf("type = %q", got.Type)
			}
			// The capability is what makes the degrade path work at all. A
			// probe job queued without it would be claimed by a worker with no
			// ffprobe and fail, which is the opposite of ADR-0023's promise.
			if got.RequiredCapability != probe.Capability {
				t.Errorf("required_capability = %q, want %q", got.RequiredCapability, probe.Capability)
			}
			if got.DedupeKey != probe.DedupeKey("blake3:abc") {
				t.Errorf("dedupe_key = %q", got.DedupeKey)
			}
			payload, ok := got.Payload.(probe.Payload)
			if !ok {
				t.Fatalf("payload is %T, want probe.Payload", got.Payload)
			}
			if payload.BlobHash != "blake3:abc" || payload.Size != 1234 {
				t.Errorf("payload = %+v", payload)
			}
		})
	}
}

// The bytes are under management, hashed, replicated and servable. Losing that
// because a follow-up job could not be queued would be trading the whole asset
// for its metadata.
func TestAFailureToQueueAProbeDoesNotFailTheIngest(t *testing.T) {
	q := &fakeEnqueuer{err: errors.New("the queue is on fire")}
	// The assertion is that this returns at all rather than panicking or
	// propagating; enqueueProbe has no error to return by design.
	enqueueProbe(t.Context(), q, ingest.Result{BlobHash: "blake3:abc", BlobSize: 1}, "films/x.mkv")
}

// fakeMinter issues credentials without a database.
type fakeMinter struct {
	created []auth.Scope
	expires *time.Time
	err     error
}

func (f *fakeMinter) Create(
	_ context.Context, _ string, scopes []auth.Scope, expiresAt *time.Time,
) (auth.CreatedToken, error) {
	if f.err != nil {
		return auth.CreatedToken{}, f.err
	}
	f.created = scopes
	f.expires = expiresAt
	return auth.CreatedToken{Secret: "minted-secret"}, nil
}

// A probe credential is scoped to read and expires. A worker holding a
// long-lived admin token for the life of the process is what this avoids.
func TestAProbeCredentialIsScopedAndExpiring(t *testing.T) {
	minter := &fakeMinter{}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

	handler := ProbeHandler(ProbeHandlerOptions{
		Prober:   nil, // unreached: the mint happens first and this asserts on it
		Recorder: nil,
		Tokens:   minter,
		BaseURL:  "http://heyarr",
		Now:      func() time.Time { return now },
	})

	// The handler will panic or error past the mint; what matters is what the
	// mint was asked for.
	func() {
		defer func() { _ = recover() }()
		payload, _ := json.Marshal(probe.Payload{BlobHash: "blake3:" + "a", Size: 1})
		_ = handler(t.Context(), jobs.Job{Type: probe.JobType, Payload: payload})
	}()

	if len(minter.created) != 1 || minter.created[0] != auth.ScopeRead {
		t.Errorf("scopes = %v, want [read] only — a probe reads bytes and does nothing else",
			minter.created)
	}
	if minter.expires == nil {
		t.Fatal("the probe credential does not expire")
	}
	if got := minter.expires.Sub(now); got != probeTokenTTL {
		t.Errorf("ttl = %s, want %s", got, probeTokenTTL)
	}
}

// A payload that cannot be decoded will never decode, and a payload naming no
// blob is not a probe.
func TestProbeHandlerRefusesNonsensePayloads(t *testing.T) {
	handler := ProbeHandler(ProbeHandlerOptions{Tokens: &fakeMinter{}})
	for _, tc := range []struct{ name, payload string }{
		{"not json", `{{{`},
		{"no blob", `{"size":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := handler(t.Context(), jobs.Job{Payload: []byte(tc.payload)}); err == nil {
				t.Error("a nonsense payload was accepted")
			}
		})
	}
}
