package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// The upload pin expiry beat (ADR-0096 amendment, #714). catalog's tests prove
// ExpireProvisionalPins; these prove the beat runs it — at startup, so a node
// down past some expiries catches up — and only with the flag on.

// stoppedTicker never ticks, so only the startup pass runs.
func stoppedTicker(time.Duration) (<-chan time.Time, func()) { return nil, func() {} }

func expiredUploadPin(t *testing.T) (*sqlite.DB, *events.Log, string) {
	t.Helper()
	db := testdb.Migrated(t)
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader()})
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(catalog.Options{DB: db, Events: log, PeerName: "test", PeerSite: "test-site"})
	if err != nil {
		t.Fatal(err)
	}
	self, err := cat.SelfPeer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	blob := "blake3:" + strings.Repeat("a", 64)
	if err := cat.RecordVaultUpload(context.Background(), blob, 16, self, catalog.UploadPin{
		Uploader: "exec-a", ProvisionalUntil: time.Now().Add(-time.Hour),
	}, nil); err != nil {
		t.Fatal(err)
	}
	return db, log, blob
}

func uploadPinHeld(t *testing.T, db *sqlite.DB, blob string) bool {
	t.Helper()
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM placement_pins WHERE blob_hash = ?`, blob).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// 🔴 With the flag on, the beat's startup pass removes an expired pin.
func TestThePinExpiryBeatRemovesAnExpiredUploadPin(t *testing.T) {
	db, log, blob := expiredUploadPin(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startPinExpiry(ctx, db, log, true, "test", "test-site", discard(), stoppedTicker)

	deadline := time.Now().Add(30 * time.Second)
	for uploadPinHeld(t, db, blob) {
		if time.Now().After(deadline) {
			t.Fatal("the expired, unconfirmed upload pin is still held — nothing runs the expiry, " +
				"so a failed vault write's blobs are never reclaimed (#714)")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// 🔴 With the flag off — the rollout default — nothing expires, even a pin
// that became provisional while it was on.
func TestThePinExpiryBeatDoesNothingWithTheFlagOff(t *testing.T) {
	db, log, blob := expiredUploadPin(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Disabled, startPinExpiry starts nothing, so there is no pass to wait
	// for: whatever it would do, it would have done before returning or not
	// at all. Run the enabled variant on a second database alongside as the
	// control that a pass does happen within the same window.
	startPinExpiry(ctx, db, log, false, "test", "test-site", discard(), stoppedTicker)
	ctrlDB, ctrlLog, ctrlBlob := expiredUploadPin(t)
	startPinExpiry(ctx, ctrlDB, ctrlLog, true, "test", "test-site", discard(), stoppedTicker)
	deadline := time.Now().Add(30 * time.Second)
	for uploadPinHeld(t, ctrlDB, ctrlBlob) {
		if time.Now().After(deadline) {
			t.Fatal("the control never expired its pin")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !uploadPinHeld(t, db, blob) {
		t.Error("an upload pin expired with placement.provisional_upload_pins off — turning " +
			"the flag off must stop the reclaiming, not only the recording")
	}
}
