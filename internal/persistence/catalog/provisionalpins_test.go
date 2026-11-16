package catalog_test

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// Provisional upload pins (ADR-0096 amendment, #714). A vault write uploads its
// blobs before it pushes the change that names them; before this, a write that
// failed in between left the blobs pinned for ever with nothing naming them.

var uploadedAt = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// pinState is the pin on (blob, peer): whether it exists, and its expiry when
// it is provisional ("" when permanent).
func (h *harness) pinState(t *testing.T, blob, peer string) (bool, string) {
	t.Helper()
	var until sql.NullString
	err := h.db.Reader().QueryRow(
		`SELECT provisional_until FROM placement_pins WHERE blob_hash = ? AND peer_id = ?`,
		blob, peer).Scan(&until)
	if err == sql.ErrNoRows {
		return false, ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return true, until.String
}

func (h *harness) uploadPin(t *testing.T, blob, peer, uploader string, until time.Time) {
	t.Helper()
	if err := h.cat.RecordVaultUpload(context.Background(), blob, 1024, peer,
		catalog.UploadPin{Uploader: uploader, ProvisionalUntil: until}, nil); err != nil {
		t.Fatal(err)
	}
}

// 🔴 An upload pin's provisional state combines with the pin it finds by
// rules that always lean towards keeping the bytes.
func TestAnUploadPinCombinesWithThePinAlreadyThere(t *testing.T) {
	t.Parallel()
	day := uploadedAt.Add(24 * time.Hour)
	later := day.Add(time.Hour)
	cases := []struct {
		name      string
		first     catalog.UploadPin // zero ProvisionalUntil = permanent
		second    catalog.UploadPin
		permanent bool
		until     time.Time
	}{
		{
			"a provisional upload is provisional",
			catalog.UploadPin{Uploader: "exec-a", ProvisionalUntil: day},
			catalog.UploadPin{},
			false, day,
		},
		{
			"a permanent pin stays permanent under a provisional re-upload",
			catalog.UploadPin{Uploader: "exec-a"},
			catalog.UploadPin{Uploader: "exec-a", ProvisionalUntil: day},
			true,
			time.Time{},
		},
		{
			"a permanent re-upload settles a provisional pin",
			catalog.UploadPin{Uploader: "exec-a", ProvisionalUntil: day},
			catalog.UploadPin{Uploader: "exec-a"},
			true,
			time.Time{},
		},
		{
			"the same uploader extends the expiry",
			catalog.UploadPin{Uploader: "exec-a", ProvisionalUntil: day},
			catalog.UploadPin{Uploader: "exec-a", ProvisionalUntil: later},
			false, later,
		},
		{
			"a second uploader makes it permanent, since only one uploader can confirm",
			catalog.UploadPin{Uploader: "exec-a", ProvisionalUntil: day},
			catalog.UploadPin{Uploader: "exec-b", ProvisionalUntil: later},
			true,
			time.Time{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			self := h.seedRemotePeer(t)
			blob := hashOf('a')
			h.uploadPin(t, blob, self, tc.first.Uploader, tc.first.ProvisionalUntil)
			if tc.second != (catalog.UploadPin{}) {
				h.uploadPin(t, blob, self, tc.second.Uploader, tc.second.ProvisionalUntil)
			}
			exists, until := h.pinState(t, blob, self)
			if !exists {
				t.Fatal("the upload recorded no pin")
			}
			if tc.permanent && until != "" {
				t.Errorf("pin is provisional until %s, want permanent", until)
			}
			if !tc.permanent {
				if until == "" {
					t.Fatal("pin is permanent, want provisional")
				}
				got, err := time.Parse(time.RFC3339Nano, until)
				if err != nil || !got.Equal(tc.until) {
					t.Errorf("provisional until %s, want %s", until, tc.until)
				}
			}
		})
	}
}

// 🔴 Confirming makes a pin permanent; it is idempotent, per blob, and a
// restricted executor may confirm only what it uploaded itself.
func TestConfirmingUploadPins(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	day := uploadedAt.Add(24 * time.Hour)
	mine, theirs, permanent, unknown := hashOf('a'), hashOf('b'), hashOf('c'), hashOf('d')
	h.uploadPin(t, mine, self, "exec-a", day)
	h.uploadPin(t, theirs, self, "exec-b", day)
	h.uploadPin(t, permanent, self, "exec-a", time.Time{})

	got, err := h.cat.ConfirmUploadPins(ctx, self, []string{mine, theirs, permanent, unknown}, "exec-a", true)
	if err != nil {
		t.Fatal(err)
	}
	want := []catalog.ConfirmResult{
		{BlobHash: mine, Status: catalog.ConfirmConfirmed},
		{BlobHash: theirs, Status: catalog.ConfirmNotUploader},
		{BlobHash: permanent, Status: catalog.ConfirmAlreadyPermanent},
		{BlobHash: unknown, Status: catalog.ConfirmNotHeld},
	}
	if !slices.Equal(got, want) {
		t.Errorf("results = %+v\nwant %+v", got, want)
	}
	if _, until := h.pinState(t, mine, self); until != "" {
		t.Error("a confirmed pin is still provisional")
	}
	if _, until := h.pinState(t, theirs, self); until == "" {
		t.Error("a restricted executor confirmed another principal's upload")
	}
	if n := h.eventsOfType(t, events.TypePinConfirmed); n != 1 {
		t.Errorf("%d %s events, want 1 — only the one transition", n, events.TypePinConfirmed)
	}

	// Again: nothing changes, nothing is emitted.
	again, err := h.cat.ConfirmUploadPins(ctx, self, []string{mine}, "exec-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Status != catalog.ConfirmAlreadyPermanent {
		t.Errorf("re-confirming = %s, want %s", again[0].Status, catalog.ConfirmAlreadyPermanent)
	}

	// An unrestricted caller (a device) may confirm any pin it names.
	byDevice, err := h.cat.ConfirmUploadPins(ctx, self, []string{theirs}, "device-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if byDevice[0].Status != catalog.ConfirmConfirmed {
		t.Errorf("a device confirming = %s, want %s", byDevice[0].Status, catalog.ConfirmConfirmed)
	}
}

// 🔴 An unconfirmed pin past its expiry is removed and its blob released to
// GC; a confirmed, permanent or unexpired one is untouched.
func TestExpiredProvisionalPinsAreRemovedAndReleased(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	self := h.seedRemotePeer(t)
	day := uploadedAt.Add(24 * time.Hour)

	orphan, confirmed, permanent, fresh, alsoPinned := hashOf('a'), hashOf('b'), hashOf('c'), hashOf('d'), hashOf('e')
	h.seedBlobs(t, orphan, confirmed, permanent, fresh, alsoPinned)
	h.uploadPin(t, orphan, self, "exec-a", day)
	h.uploadPin(t, confirmed, self, "exec-a", day)
	h.uploadPin(t, permanent, self, "exec-a", time.Time{})
	h.uploadPin(t, fresh, self, "exec-a", day.Add(48*time.Hour))
	h.uploadPin(t, alsoPinned, self, "exec-a", day)
	// Another peer's permanent pin still wants this one after its upload pin goes.
	if err := h.cat.PinPlacement(ctx, alsoPinned, remotePeer); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cat.ConfirmUploadPins(ctx, self, []string{confirmed}, "exec-a", true); err != nil {
		t.Fatal(err)
	}

	n, err := h.cat.ExpireProvisionalPins(ctx, day.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expired %d pins, want 2 (the orphan and the one another peer still pins)", n)
	}

	for _, tc := range []struct {
		blob     string
		pinned   bool
		released bool
	}{
		{orphan, false, true},
		{confirmed, true, false},
		{permanent, true, false},
		{fresh, true, false},
		{alsoPinned, false, false},
	} {
		if exists, _ := h.pinState(t, tc.blob, self); exists != tc.pinned {
			t.Errorf("%s: pinned here = %v, want %v", tc.blob, exists, tc.pinned)
		}
		if b := h.releaseOf(t, tc.blob); b.ReleasedAt.IsZero() == tc.released {
			t.Errorf("%s: released = %v, want %v", tc.blob, !b.ReleasedAt.IsZero(), tc.released)
		}
	}
	if got := h.eventsOfType(t, events.TypePinExpired); got != 2 {
		t.Errorf("%d %s events, want one per removed pin", got, events.TypePinExpired)
	}

	// Re-running finds nothing: the sweep is idempotent (invariant 9).
	if n, err := h.cat.ExpireProvisionalPins(ctx, day.Add(time.Minute)); err != nil || n != 0 {
		t.Errorf("second sweep = (%d, %v), want (0, nil)", n, err)
	}
}

// 🔴 The legacy upload is unchanged: RecordVaultBlob pins permanently.
func TestRecordVaultBlobStillPinsPermanently(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	self := h.seedRemotePeer(t)
	blob := hashOf('a')
	if err := h.cat.RecordVaultBlob(context.Background(), blob, 1024, self, nil); err != nil {
		t.Fatal(err)
	}
	if exists, until := h.pinState(t, blob, self); !exists || until != "" {
		t.Errorf("pin exists=%v provisional_until=%q, want a permanent pin", exists, until)
	}
	if n, err := h.cat.ExpireProvisionalPins(context.Background(), uploadedAt.Add(1000*time.Hour)); err != nil || n != 0 {
		t.Errorf("expiry removed %d legacy pins (err %v), want none", n, err)
	}
}
