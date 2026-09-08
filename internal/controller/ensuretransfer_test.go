package controller

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/replication"
	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// The client route's ensure-on-GET adapter against a real database (#371).
//
// Two properties the route depends on but cannot show on its own, because both
// are about the JOB TABLE the adapter writes through: the gate (only a desired
// blob starts a transfer) and idempotency (a second GET, or a GET racing a
// reconcile cycle, creates no second transfer — the dedupe key is the one
// reconcile_peer uses).

func seedDesiredBlob(t *testing.T, h *beatHarness, blob string) {
	t.Helper()
	h.exec(t, `INSERT INTO works
		(id, content_type, work_key, title, sort_title, year, attributes, created_at, updated_at)
		VALUES ('w1', 'movie', 'movie:x:2016', 'X', 'x', 2016, '{}', ?, ?)`, beatStamp, beatStamp)
	h.exec(t, `INSERT INTO blobs (hash, size, mime, first_seen_at)
		VALUES (?, 100, 'video/x-matroska', ?)`, blob, beatStamp)
	h.exec(t, `INSERT INTO editions
		(id, work_id, label, edition_type, language, attributes, created_at)
		VALUES ('e1', 'w1', '1080p', 'release', 'en', '{}', ?)`, beatStamp)
	h.exec(t, `INSERT INTO assets
		(id, edition_id, library_id, source_class, blob_hash, source_path, role, filename,
		 mime, identification_source, created_at, updated_at)
		VALUES ('a1', 'e1', NULL, 'managed', ?, '/srv/x.mkv', 'primary', 'x.mkv',
			'video/x-matroska', 'path', ?, ?)`, blob, beatStamp, beatStamp)
}

func TestEnsureTransferGatesAndIsIdempotent(t *testing.T) {
	h := newBeatHarness(t)
	ctx := context.Background()

	blob := "blake3:" + strings.Repeat("a", 64)
	seedDesiredBlob(t, h, blob)
	hash, err := hashing.Parse(blob)
	if err != nil {
		t.Fatal(err)
	}

	ens := blobTransferEnsurer{cat: h.cat, queue: h.queue, log: slog.New(slog.DiscardHandler)}

	// A desired blob: the gate passes and a transfer is enqueued.
	desired, err := ens.EnsureTransfer(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if !desired {
		t.Fatal("a blob a live asset references must be reported desired")
	}

	// A SECOND GET for the same blob: still desired, and still ONE job. This is
	// the idempotency the route leans on — two players pressing play, or a GET
	// racing a reconcile cycle, must not stack two copies of the same transfer.
	if desired, err = ens.EnsureTransfer(ctx, hash); err != nil {
		t.Fatal(err)
	} else if !desired {
		t.Fatal("the second call must also report desired")
	}

	live, err := h.queue.LiveDedupeKeys(ctx, replication.ReplicateBlobJobType)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("two GETs created %d live replicate_blob jobs, want exactly 1 (idempotent enqueue)", len(live))
	}
	self, err := h.cat.SelfPeer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := replication.Gap{BlobHash: blob, PeerID: self}.DedupeKey()
	if _, ok := live[wantKey]; !ok {
		t.Fatalf("the one live job is not keyed on blob+destination as reconcile_peer keys it (want %q)", wantKey)
	}

	// A blob NOBODY desires: the gate refuses, and no job is created for it.
	other, err := hashing.Parse("blake3:" + strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if desired, err = ens.EnsureTransfer(ctx, other); err != nil {
		t.Fatal(err)
	} else if desired {
		t.Fatal("a blob no asset references must NOT be desired — the gate that closes the DoS surface")
	}
	live, err = h.queue.LiveDedupeKeys(ctx, replication.ReplicateBlobJobType)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("a non-desired GET changed the job count to %d, want it to stay 1", len(live))
	}
}
