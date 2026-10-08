package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/events"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
	"github.com/rarebit-one/heyarr-core/internal/personalstate/store"
	"github.com/rarebit-one/heyarr-core/internal/testutil/testdb"
)

// newStoreWithLog is newStore that also hands back the event log, for the tests
// that assert a transition was recorded (Invariant 7).
func newStoreWithLog(t *testing.T) (*store.Store, *events.Log) {
	t.Helper()
	db := testdb.Migrated(t)
	clock := &fixedClock{t: now}
	log, err := events.New(events.Options{Writer: db.Writer(), Reader: db.Reader(), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(store.Options{Writer: db.Writer(), Reader: db.Reader(), Events: log, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	return s, log
}

func eventsOfType(t *testing.T, log *events.Log, typ string) []events.Event {
	t.Helper()
	evs, err := log.Since(context.Background(), 0, []string{typ}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func wrapsByRecipient(t *testing.T, s *store.Store, spaceID string) map[string]store.WrappedKey {
	t.Helper()
	keys, err := s.WrappedKeysFor(context.Background(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]store.WrappedKey, len(keys))
	for _, k := range keys {
		out[k.Recipient] = k
	}
	return out
}

func mustEpoch(t *testing.T, s *store.Store, spaceID string) int {
	t.Helper()
	e, err := s.KeyEpoch(context.Background(), spaceID)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestKeyEpochIsDerivedFromHistory: a fresh space is at epoch 0 with no history;
// each rotation adds one row and the epoch is the newest row's.
func TestKeyEpochIsDerivedFromHistory(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindPersonal)
	_, aID := device(t)

	if got := mustEpoch(t, s, sp.ID); got != 0 {
		t.Fatalf("fresh space epoch = %d, want 0", got)
	}
	if h, err := s.KeyHistory(ctx, sp.ID); err != nil || len(h) != 0 {
		t.Fatalf("fresh space history = %v, %v; want empty", h, err)
	}
	if _, err := s.PutWrappedKey(ctx, sp.ID, aID, []byte{0xa0, 0}, 0); err != nil {
		t.Fatal(err)
	}
	for want := 1; want <= 3; want++ {
		got, err := s.RotateKey(ctx, sp.ID, want-1, []byte{byte(want)}, []store.RecipientWrap{{Recipient: aID, Wrapped: []byte{0xa0, byte(want)}}}, nil, nil)
		if err != nil {
			t.Fatalf("rotation %d: %v", want, err)
		}
		if got != want || mustEpoch(t, s, sp.ID) != want {
			t.Fatalf("rotation %d returned %d, epoch now %d", want, got, mustEpoch(t, s, sp.ID))
		}
	}
	h, err := s.KeyHistory(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 {
		t.Fatalf("history has %d rows, want 3", len(h))
	}
	for i, e := range h {
		if e.Epoch != i+1 || !bytes.Equal(e.SealedPrev, []byte{byte(i + 1)}) || e.SpaceID != sp.ID {
			t.Fatalf("history[%d] = %+v, want epoch %d ascending with its own bytes", i, e, i+1)
		}
	}
	if _, err := s.KeyEpoch(ctx, mustUUIDStr(t)); !errors.Is(err, store.ErrUnknownSpace) {
		t.Fatalf("KeyEpoch(unknown) = %v, want ErrUnknownSpace", err)
	}
	if _, err := s.KeyHistory(ctx, mustUUIDStr(t)); !errors.Is(err, store.ErrUnknownSpace) {
		t.Fatalf("KeyHistory(unknown) = %v, want ErrUnknownSpace", err)
	}
}

// TestPutWrappedKeyEpochGate: a wrap at the current epoch is stored with it; an
// older or newer epoch is refused and stores nothing.
func TestPutWrappedKeyEpochGate(t *testing.T) {
	t.Parallel()
	_, aID := device(t)
	_, bID := device(t)
	cases := []struct {
		name    string
		rotate  int // rotations before the put
		epoch   int
		wantErr error
	}{
		{name: "original key at epoch 0", rotate: 0, epoch: 0},
		{name: "current epoch after a rotation", rotate: 1, epoch: 1},
		{name: "stale epoch after a rotation", rotate: 1, epoch: 0, wantErr: store.ErrStaleKeyEpoch},
		{name: "negative epoch is stale", rotate: 0, epoch: -1, wantErr: store.ErrStaleKeyEpoch},
		{name: "future epoch before its history", rotate: 0, epoch: 1, wantErr: store.ErrFutureKeyEpoch},
		{name: "future epoch two ahead", rotate: 1, epoch: 3, wantErr: store.ErrFutureKeyEpoch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			ctx := context.Background()
			sp, _ := s.CreateSpace(ctx, spaces.KindFamily)
			if tc.rotate > 0 {
				if _, err := s.PutWrappedKey(ctx, sp.ID, aID, []byte("a"), 0); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < tc.rotate; i++ {
				if _, err := s.RotateKey(ctx, sp.ID, i, []byte("prev"), []store.RecipientWrap{{Recipient: aID, Wrapped: []byte("a")}}, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			w, err := s.PutWrappedKey(ctx, sp.ID, bID, []byte("b"), tc.epoch)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("PutWrappedKey = %v, want %v", err, tc.wantErr)
				}
				if _, held := wrapsByRecipient(t, s, sp.ID)[bID]; held {
					t.Fatal("a refused wrap was stored anyway")
				}
				return
			}
			if err != nil {
				t.Fatalf("PutWrappedKey: %v", err)
			}
			if w.Epoch != tc.epoch || wrapsByRecipient(t, s, sp.ID)[bID].Epoch != tc.epoch {
				t.Fatalf("stored epoch = %d (returned %d), want %d", wrapsByRecipient(t, s, sp.ID)[bID].Epoch, w.Epoch, tc.epoch)
			}
		})
	}
}

// TestRotateKeyIsACompareAndSwapThatDropsStaleWraps: a rotation from the current
// epoch lands the history row, re-wraps the listed recipients at the new epoch,
// and deletes every older wrap — so a recipient left out (the revoked device)
// loses its copy. Its events record the rotation and the revocation, no secret.
func TestRotateKeyIsACompareAndSwapThatDropsStaleWraps(t *testing.T) {
	t.Parallel()
	s, log := newStoreWithLog(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindShared)
	_, aID := device(t)
	_, bID := device(t)
	_, revokedID := device(t)
	for _, r := range []string{aID, bID, revokedID} {
		if _, err := s.PutWrappedKey(ctx, sp.ID, r, []byte("k0-"+r), 0); err != nil {
			t.Fatal(err)
		}
	}

	sealed := []byte("k0 sealed under k1")
	next, err := s.RotateKey(ctx, sp.ID, 0, sealed, []store.RecipientWrap{
		{Recipient: aID, Wrapped: []byte("k1-a")},
		{Recipient: bID, Wrapped: []byte("k1-b")},
	}, []string{revokedID}, nil)
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if next != 1 {
		t.Fatalf("RotateKey returned epoch %d, want 1", next)
	}

	held := wrapsByRecipient(t, s, sp.ID)
	if len(held) != 2 {
		t.Fatalf("after rotation %d wraps are held, want 2 (the revoked one dropped): %v", len(held), held)
	}
	if _, ok := held[revokedID]; ok {
		t.Fatal("the recipient the rotation left out still holds a copy")
	}
	for r, want := range map[string]string{aID: "k1-a", bID: "k1-b"} {
		if got := held[r]; got.Epoch != 1 || string(got.Wrapped) != want {
			t.Fatalf("wrap for %s = epoch %d %q, want epoch 1 %q", r, got.Epoch, got.Wrapped, want)
		}
	}
	h, err := s.KeyHistory(ctx, sp.ID)
	if err != nil || len(h) != 1 || !bytes.Equal(h[0].SealedPrev, sealed) {
		t.Fatalf("history = %+v, %v; want the one sealed row", h, err)
	}

	// The CAS: a second rotation from the stale expectation is refused, and
	// nothing it carried lands.
	if _, err := s.RotateKey(ctx, sp.ID, 0, []byte("fork"), []store.RecipientWrap{{Recipient: aID, Wrapped: []byte("fork-a")}}, []string{bID}, nil); !errors.Is(err, store.ErrKeyEpochConflict) {
		t.Fatalf("racing rotation = %v, want ErrKeyEpochConflict", err)
	}
	if got := wrapsByRecipient(t, s, sp.ID)[aID]; string(got.Wrapped) != "k1-a" {
		t.Fatalf("a refused rotation overwrote a wrap: %q", got.Wrapped)
	}
	if mustEpoch(t, s, sp.ID) != 1 {
		t.Fatal("a refused rotation moved the epoch")
	}

	rotated := eventsOfType(t, log, events.TypeSpaceKeyRotated)
	if len(rotated) != 1 {
		t.Fatalf("got %d rotation events, want 1", len(rotated))
	}
	var payload map[string]any
	if err := json.Unmarshal(rotated[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["epoch"] != float64(1) || payload["recipients"] != float64(2) || payload["revoked"] != float64(1) {
		t.Fatalf("rotation payload = %v", payload)
	}
	if bytes.Contains(rotated[0].Payload, sealed) || bytes.Contains(rotated[0].Payload, []byte("k1-a")) {
		t.Fatal("the rotation event carries key material")
	}
	revoked := eventsOfType(t, log, events.TypeSpaceKeyRevoked)
	if len(revoked) != 1 || !bytes.Contains(revoked[0].Payload, []byte(revokedID)) {
		t.Fatalf("revocation events = %d, want one naming the dropped recipient", len(revoked))
	}
}

// TestRotateKeyRefusesMalformedInput: every refusal writes nothing.
func TestRotateKeyRefusesMalformedInput(t *testing.T) {
	t.Parallel()
	_, aID := device(t)
	ok := []store.RecipientWrap{{Recipient: aID, Wrapped: []byte("a")}}
	cases := []struct {
		name     string
		space    string // "" = the created space
		expected int
		sealed   []byte
		wraps    []store.RecipientWrap
		wantErr  error
	}{
		{name: "no sealed previous key", sealed: nil, wraps: ok, wantErr: store.ErrEmptySealedPrev},
		{name: "no wraps", sealed: []byte("p"), wraps: nil, wantErr: store.ErrNoRotationWraps},
		{name: "empty recipient", sealed: []byte("p"), wraps: []store.RecipientWrap{{Wrapped: []byte("a")}}, wantErr: store.ErrEmptyRecipient},
		{name: "empty wrapped", sealed: []byte("p"), wraps: []store.RecipientWrap{{Recipient: aID}}, wantErr: store.ErrEmptyWrapped},
		{name: "negative expected epoch", expected: -1, sealed: []byte("p"), wraps: ok, wantErr: store.ErrInvalidKeyEpoch},
		{name: "expected epoch ahead", expected: 1, sealed: []byte("p"), wraps: ok, wantErr: store.ErrKeyEpochConflict},
		{name: "unknown space", space: "unknown", sealed: []byte("p"), wraps: ok, wantErr: store.ErrUnknownSpace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			ctx := context.Background()
			sp, _ := s.CreateSpace(ctx, spaces.KindPersonal)
			id := sp.ID
			if tc.space != "" {
				id = mustUUIDStr(t)
			}
			if _, err := s.RotateKey(ctx, id, tc.expected, tc.sealed, tc.wraps, nil, nil); !errors.Is(err, tc.wantErr) {
				t.Fatalf("RotateKey = %v, want %v", err, tc.wantErr)
			}
			if mustEpoch(t, s, sp.ID) != 0 {
				t.Fatal("a refused rotation moved the epoch")
			}
		})
	}
}

// TestPutKeyHistoryReplicates: a history row arriving by replication is stored
// once (a re-push is a silent no-op), a different row for the same epoch is a
// conflict, and a row that becomes the newest epoch drops the older wraps — the
// replicated form of revocation.
func TestPutKeyHistoryReplicates(t *testing.T) {
	t.Parallel()
	s, log := newStoreWithLog(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindFamily)
	_, aID := device(t)
	_, bID := device(t)
	for _, r := range []string{aID, bID} {
		if _, err := s.PutWrappedKey(ctx, sp.ID, r, []byte("k0"), 0); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.PutKeyHistory(ctx, sp.ID, 1, []byte("row-1")); err != nil {
		t.Fatalf("PutKeyHistory: %v", err)
	}
	if mustEpoch(t, s, sp.ID) != 1 {
		t.Fatal("the replicated row did not move the epoch")
	}
	if held := wrapsByRecipient(t, s, sp.ID); len(held) != 0 {
		t.Fatalf("epoch-0 wraps survived the epoch-1 row: %v", held)
	}
	// Now the epoch-1 wraps can land, and an epoch-0 one cannot come back.
	if _, err := s.PutWrappedKey(ctx, sp.ID, aID, []byte("k1"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutWrappedKey(ctx, sp.ID, bID, []byte("k0"), 0); !errors.Is(err, store.ErrStaleKeyEpoch) {
		t.Fatalf("resurrecting an epoch-0 wrap = %v, want ErrStaleKeyEpoch", err)
	}

	// Idempotent: the same bytes again is a no-op with no second event.
	if err := s.PutKeyHistory(ctx, sp.ID, 1, []byte("row-1")); err != nil {
		t.Fatalf("idempotent re-push: %v", err)
	}
	if n := len(eventsOfType(t, log, events.TypeSpaceKeyRotated)); n != 1 {
		t.Fatalf("got %d rotation events after a re-push, want 1", n)
	}
	// A fork: different bytes for a held epoch.
	if err := s.PutKeyHistory(ctx, sp.ID, 1, []byte("other")); !errors.Is(err, store.ErrKeyHistoryConflict) {
		t.Fatalf("forked row = %v, want ErrKeyHistoryConflict", err)
	}
	if got := wrapsByRecipient(t, s, sp.ID)[aID]; got.Epoch != 1 {
		t.Fatal("a refused fork disturbed the current wraps")
	}

	cases := []struct {
		name    string
		epoch   int
		sealed  []byte
		wantErr error
	}{
		{name: "epoch 0 has no predecessor", epoch: 0, sealed: []byte("x"), wantErr: store.ErrInvalidKeyEpoch},
		{name: "empty bytes", epoch: 2, sealed: nil, wantErr: store.ErrEmptySealedPrev},
	}
	for _, tc := range cases {
		if err := s.PutKeyHistory(ctx, sp.ID, tc.epoch, tc.sealed); !errors.Is(err, tc.wantErr) {
			t.Fatalf("%s: PutKeyHistory = %v, want %v", tc.name, err, tc.wantErr)
		}
	}
	if err := s.PutKeyHistory(ctx, mustUUIDStr(t), 1, []byte("x")); !errors.Is(err, store.ErrUnknownSpace) {
		t.Fatalf("PutKeyHistory(unknown space) = %v, want ErrUnknownSpace", err)
	}
}

// TestPutKeyHistoryOlderRowDropsNothing: a link older than the current epoch
// arriving late fills the chain without touching the current wraps.
func TestPutKeyHistoryOlderRowDropsNothing(t *testing.T) {
	t.Parallel()
	s, log := newStoreWithLog(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindFamily)
	_, aID := device(t)
	if err := s.PutKeyHistory(ctx, sp.ID, 2, []byte("row-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutWrappedKey(ctx, sp.ID, aID, []byte("k2"), 2); err != nil {
		t.Fatal(err)
	}
	if err := s.PutKeyHistory(ctx, sp.ID, 1, []byte("row-1")); err != nil {
		t.Fatal(err)
	}
	if mustEpoch(t, s, sp.ID) != 2 {
		t.Fatal("an older row moved the epoch")
	}
	if got := wrapsByRecipient(t, s, sp.ID)[aID]; got.Epoch != 2 {
		t.Fatal("an older row dropped a current wrap")
	}
	h, _ := s.KeyHistory(ctx, sp.ID)
	if len(h) != 2 || h[0].Epoch != 1 || h[1].Epoch != 2 {
		t.Fatalf("history = %+v, want epochs 1, 2", h)
	}
	// The late row is a backfill, not a rotation: one rotation event (epoch 2)
	// and one backfill event (epoch 1).
	if rot := eventsOfType(t, log, events.TypeSpaceKeyRotated); len(rot) != 1 {
		t.Fatalf("rotation events = %d, want 1 (the epoch-2 row only)", len(rot))
	}
	if bf := eventsOfType(t, log, events.TypeSpaceKeyHistoryBackfilled); len(bf) != 1 {
		t.Fatalf("backfill events = %d, want 1", len(bf))
	}
}

// TestRotateKeyMustKeepPreservedRecipients: a recovery key holding a copy of the
// current key cannot be left out of a rotation — the history seals backwards
// only, so it would never reach the new key. A preserved recipient that holds no
// current copy is not required, and nothing is written on refusal.
func TestRotateKeyMustKeepPreservedRecipients(t *testing.T) {
	t.Parallel()
	_, deviceID := device(t)
	_, recoveryID := device(t)
	_, otherRecovery := device(t)
	cases := []struct {
		name     string
		wraps    []string
		revoke   []string
		preserve map[string]bool
		wantErr  error
	}{
		{name: "the recovery key re-wrapped", wraps: []string{deviceID, recoveryID}, preserve: map[string]bool{recoveryID: true}},
		{name: "the recovery key revoked", wraps: []string{deviceID}, revoke: []string{recoveryID}, preserve: map[string]bool{recoveryID: true}, wantErr: store.ErrRotationDropsPreserved},
		{name: "a recovery key holding no copy is not required", wraps: []string{deviceID, recoveryID}, preserve: map[string]bool{recoveryID: true, otherRecovery: true}},
		{name: "no preserve set leaves the check off", wraps: []string{deviceID}, revoke: []string{recoveryID}, preserve: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			ctx := context.Background()
			sp, _ := s.CreateSpace(ctx, spaces.KindPersonal)
			for _, r := range []string{deviceID, recoveryID} {
				if _, err := s.PutWrappedKey(ctx, sp.ID, r, []byte("k0"), 0); err != nil {
					t.Fatal(err)
				}
			}
			var wraps []store.RecipientWrap
			for _, r := range tc.wraps {
				wraps = append(wraps, store.RecipientWrap{Recipient: r, Wrapped: []byte("k1")})
			}
			_, err := s.RotateKey(ctx, sp.ID, 0, []byte("prev"), wraps, tc.revoke, tc.preserve)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("RotateKey = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr != nil {
				if mustEpoch(t, s, sp.ID) != 0 || len(wrapsByRecipient(t, s, sp.ID)) != 2 {
					t.Fatal("a refused rotation wrote something")
				}
			}
		})
	}
}

// TestKeyStateIsOneConsistentRead: the epoch and the wraps come back together,
// before and after a rotation, and an unknown space is ErrUnknownSpace.
func TestKeyStateIsOneConsistentRead(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindPersonal)
	_, aID := device(t)
	if _, err := s.PutWrappedKey(ctx, sp.ID, aID, []byte("k0"), 0); err != nil {
		t.Fatal(err)
	}
	epoch, keys, err := s.KeyState(ctx, sp.ID)
	if err != nil || epoch != 0 || len(keys) != 1 || keys[0].Epoch != 0 {
		t.Fatalf("KeyState before = %d %+v %v", epoch, keys, err)
	}
	if _, err := s.RotateKey(ctx, sp.ID, 0, []byte("p"), []store.RecipientWrap{{Recipient: aID, Wrapped: []byte("k1")}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	epoch, keys, err = s.KeyState(ctx, sp.ID)
	if err != nil || epoch != 1 || len(keys) != 1 || keys[0].Epoch != 1 {
		t.Fatalf("KeyState after = %d %+v %v", epoch, keys, err)
	}
	if _, _, err := s.KeyState(ctx, mustUUIDStr(t)); !errors.Is(err, store.ErrUnknownSpace) {
		t.Fatalf("KeyState(unknown) = %v, want ErrUnknownSpace", err)
	}
}

// TestRotateKeyComparesTheRecipientSet (#703): a rotation's wraps plus its
// revoke set must name exactly the recipients holding a copy of the current key.
// The device read that set before rotating; a recipient added since (through
// the re-wrap path, which does not move the epoch) or removed since makes the
// rotation stale, and it is refused with nothing written — so the newcomer's
// wrap is not silently dropped and the removed recipient is not handed the new
// key. A recipient both re-wrapped and revoked is malformed.
func TestRotateKeyComparesTheRecipientSet(t *testing.T) {
	t.Parallel()
	_, keepID := device(t)
	_, revokeID := device(t)
	_, addedID := device(t)
	_, strangerID := device(t)
	cases := []struct {
		name    string
		between func(t *testing.T, s *store.Store, spaceID string) // after the device read {keep, revoke}
		wraps   []string
		revoke  []string
		wantErr error
	}{
		{name: "the set unchanged", wraps: []string{keepID}, revoke: []string{revokeID}},
		{name: "a pure re-key keeping everyone", wraps: []string{keepID, revokeID}, revoke: []string{}},
		{
			name: "a recipient added in between",
			between: func(t *testing.T, s *store.Store, spaceID string) {
				if _, err := s.PutWrappedKey(context.Background(), spaceID, addedID, []byte("k0-added"), 0); err != nil {
					t.Fatal(err)
				}
			},
			wraps: []string{keepID}, revoke: []string{revokeID}, wantErr: store.ErrRotationRecipientsChanged,
		},
		{
			name: "a kept recipient removed in between",
			between: func(t *testing.T, s *store.Store, spaceID string) {
				if err := s.DeleteWrappedKey(context.Background(), spaceID, keepID); err != nil {
					t.Fatal(err)
				}
			},
			wraps: []string{keepID, revokeID}, revoke: []string{}, wantErr: store.ErrRotationRecipientsChanged,
		},
		{
			name: "a revoked recipient already removed",
			between: func(t *testing.T, s *store.Store, spaceID string) {
				if err := s.DeleteWrappedKey(context.Background(), spaceID, revokeID); err != nil {
					t.Fatal(err)
				}
			},
			wraps: []string{keepID}, revoke: []string{revokeID}, wantErr: store.ErrRotationRecipientsChanged,
		},
		{name: "a recipient left out without being revoked", wraps: []string{keepID}, revoke: []string{}, wantErr: store.ErrRotationRecipientsChanged},
		{name: "a wrap for a recipient holding no copy", wraps: []string{keepID, revokeID, strangerID}, revoke: []string{}, wantErr: store.ErrRotationRecipientsChanged},
		{name: "revoking a recipient holding no copy", wraps: []string{keepID}, revoke: []string{revokeID, strangerID}, wantErr: store.ErrRotationRecipientsChanged},
		{name: "a recipient both re-wrapped and revoked", wraps: []string{keepID, revokeID}, revoke: []string{revokeID}, wantErr: store.ErrRotationRevokesRewrapped},
		{name: "an empty revoked recipient", wraps: []string{keepID}, revoke: []string{revokeID, ""}, wantErr: store.ErrEmptyRecipient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, log := newStoreWithLog(t)
			ctx := context.Background()
			sp, _ := s.CreateSpace(ctx, spaces.KindShared)
			for _, r := range []string{keepID, revokeID} {
				if _, err := s.PutWrappedKey(ctx, sp.ID, r, []byte("k0-"+r), 0); err != nil {
					t.Fatal(err)
				}
			}
			if tc.between != nil {
				tc.between(t, s, sp.ID)
			}
			before := wrapsByRecipient(t, s, sp.ID)
			countEvents := func() int {
				return len(eventsOfType(t, log, events.TypeSpaceKeyRotated)) + len(eventsOfType(t, log, events.TypeSpaceKeyRevoked))
			}
			eventsBefore := countEvents()
			var wraps []store.RecipientWrap
			for _, r := range tc.wraps {
				wraps = append(wraps, store.RecipientWrap{Recipient: r, Wrapped: []byte("k1-" + r)})
			}
			_, err := s.RotateKey(ctx, sp.ID, 0, []byte("prev"), wraps, tc.revoke, nil)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("RotateKey = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil {
				held := wrapsByRecipient(t, s, sp.ID)
				if mustEpoch(t, s, sp.ID) != 1 || len(held) != len(tc.wraps) {
					t.Fatalf("after rotation: epoch %d, wraps %v", mustEpoch(t, s, sp.ID), held)
				}
				for _, r := range tc.wraps {
					if held[r].Epoch != 1 {
						t.Fatalf("%s not re-wrapped at epoch 1: %+v", r, held[r])
					}
				}
				return
			}
			// Nothing written: the epoch, the history, every wrap (the newcomer's
			// included) and the event log are as they were.
			if mustEpoch(t, s, sp.ID) != 0 {
				t.Fatal("a refused rotation moved the epoch")
			}
			if h, err := s.KeyHistory(ctx, sp.ID); err != nil || len(h) != 0 {
				t.Fatalf("a refused rotation stored history: %v %v", h, err)
			}
			after := wrapsByRecipient(t, s, sp.ID)
			if len(after) != len(before) {
				t.Fatalf("a refused rotation changed the wraps: %v -> %v", before, after)
			}
			for r, w := range before {
				if got := after[r]; got.Epoch != w.Epoch || !bytes.Equal(got.Wrapped, w.Wrapped) {
					t.Fatalf("a refused rotation changed %s's wrap: %+v -> %+v", r, w, got)
				}
			}
			if n := countEvents() - eventsBefore; n != 0 {
				t.Fatalf("a refused rotation emitted %d events", n)
			}
		})
	}
}

// TestPutChangeAtEpochIsConditional (#712): a change pushed at the space's
// current epoch is stored; one sealed at a superseded or unreached epoch is
// refused with nothing stored; a re-send of a change already held stays a no-op
// whatever epoch it names; a negative epoch is invalid.
func TestPutChangeAtEpochIsConditional(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	ctx := context.Background()
	sp, _ := s.CreateSpace(ctx, spaces.KindPersonal)
	_, aID := device(t)
	if _, err := s.PutWrappedKey(ctx, sp.ID, aID, []byte{0xa0, 0}, 0); err != nil {
		t.Fatal(err)
	}

	atZero := change(t, sp.ID, nil, []byte("sealed at epoch 0"))
	if err := s.PutChangeAtEpoch(ctx, atZero, 0); err != nil {
		t.Fatalf("push at the current epoch: %v", err)
	}
	if _, err := s.RotateKey(ctx, sp.ID, 0, []byte("prev"), []store.RecipientWrap{{Recipient: aID, Wrapped: []byte{0xa0, 1}}}, nil, nil); err != nil {
		t.Fatal(err)
	}

	stale := change(t, sp.ID, []string{atZero.ChangeID}, []byte("sealed at epoch 0, published late"))
	if err := s.PutChangeAtEpoch(ctx, stale, 0); !errors.Is(err, store.ErrChangeKeyEpoch) {
		t.Fatalf("push at a superseded epoch = %v, want ErrChangeKeyEpoch", err)
	}
	ahead := change(t, sp.ID, []string{atZero.ChangeID}, []byte("sealed at an epoch not reached"))
	if err := s.PutChangeAtEpoch(ctx, ahead, 2); !errors.Is(err, store.ErrChangeKeyEpoch) {
		t.Fatalf("push at an unreached epoch = %v, want ErrChangeKeyEpoch", err)
	}
	got, err := s.ChangesFor(ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ChangeID != atZero.ChangeID {
		t.Fatalf("held changes = %v, want only the epoch-0 change", got)
	}

	// A lost response: the writer re-sends what the peer already holds.
	if err := s.PutChangeAtEpoch(ctx, atZero, 0); err != nil {
		t.Fatalf("re-send of a held change = %v, want a no-op", err)
	}
	fresh := change(t, sp.ID, []string{atZero.ChangeID}, []byte("sealed at epoch 1"))
	if err := s.PutChangeAtEpoch(ctx, fresh, 1); err != nil {
		t.Fatalf("push at the new epoch: %v", err)
	}
	if err := s.PutChangeAtEpoch(ctx, fresh, -1); !errors.Is(err, store.ErrInvalidKeyEpoch) {
		t.Fatalf("negative epoch = %v, want ErrInvalidKeyEpoch", err)
	}
	if err := s.PutChangeAtEpoch(ctx, change(t, mustUUIDStr(t), nil, []byte("x")), 0); !errors.Is(err, store.ErrUnknownSpace) {
		t.Fatalf("unknown space = %v, want ErrUnknownSpace", err)
	}
}
