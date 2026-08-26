package spaces_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rarebit-one/heyarr-core/internal/personalstate/spaces"
)

// fixedTime is a pinned clock reading. The tests inject it so that time standing
// still cannot be what makes two spaces differ — their ids must differ on their
// own (that is the §38 opacity property), and a moving wall clock would hide a
// bug where they did not.
var fixedTime = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func TestKindValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kind    spaces.Kind
		wantErr bool
	}{
		{name: "personal", kind: spaces.KindPersonal},
		{name: "family", kind: spaces.KindFamily},
		{name: "shared", kind: spaces.KindShared},
		{name: "research", kind: spaces.KindResearch},
		{name: "unknown word", kind: spaces.Kind("playlist"), wantErr: true},
		{name: "empty", kind: spaces.Kind(""), wantErr: true},
		{name: "case-mismatch is unknown", kind: spaces.Kind("Personal"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.kind.Validate()
			if tt.wantErr {
				if !errors.Is(err, spaces.ErrUnknownKind) {
					t.Fatalf("Validate(%q) = %v, want ErrUnknownKind", tt.kind, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", tt.kind, err)
			}
		})
	}
}

// TestNewSpaceRejectsUnknownKind asserts an unknown kind never yields a space:
// the id is minted only after the kind clears [spaces.Kind.Validate].
func TestNewSpaceRejectsUnknownKind(t *testing.T) {
	t.Parallel()

	got, err := spaces.NewSpace(spaces.Kind("nope"), fixedTime)
	if !errors.Is(err, spaces.ErrUnknownKind) {
		t.Fatalf("NewSpace(unknown) err = %v, want ErrUnknownKind", err)
	}
	if got != (spaces.EncryptedSpace{}) {
		t.Fatalf("NewSpace(unknown) returned a non-zero space: %+v", got)
	}
}

// TestNewSpaceAcceptsEveryKnownKind is the positive half: each §39 kind builds a
// space whose fields are exactly what was asked for, with a valid opaque id.
func TestNewSpaceAcceptsEveryKnownKind(t *testing.T) {
	t.Parallel()

	for _, kind := range []spaces.Kind{
		spaces.KindPersonal,
		spaces.KindFamily,
		spaces.KindShared,
		spaces.KindResearch,
	} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()

			got, err := spaces.NewSpace(kind, fixedTime)
			if err != nil {
				t.Fatalf("NewSpace(%q) = %v, want nil", kind, err)
			}
			if got.Kind != kind {
				t.Errorf("Kind = %q, want %q", got.Kind, kind)
			}
			if !got.CreatedAt.Equal(fixedTime) {
				t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, fixedTime)
			}
			assertOpaqueV7(t, got.ID)
		})
	}
}

// TestIDsAreUniqueAndOpaque is the load-bearing §38 assertion. It mints many
// spaces of the SAME kind at the SAME pinned instant and requires every id to be
// distinct and a valid UUIDv7.
//
// SABOTAGE NOTE: if NewSpace ever derived the id from the kind (or a name, or any
// content) — e.g. a hash of the kind string — every space here would collide on
// one id and this test would fail. That is the point: the test is what makes
// "the id must not leak the kind" (§38) a property the code cannot quietly lose.
func TestIDsAreUniqueAndOpaque(t *testing.T) {
	t.Parallel()

	const n = 10_000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		got, err := spaces.NewSpace(spaces.KindPersonal, fixedTime)
		if err != nil {
			t.Fatalf("NewSpace: %v", err)
		}
		assertOpaqueV7(t, got.ID)
		if _, dup := seen[got.ID]; dup {
			t.Fatalf("duplicate id %q after %d spaces — id is not opaque/unique", got.ID, i)
		}
		seen[got.ID] = struct{}{}
	}
}

// TestIDDoesNotContainKind checks opacity directly: whatever the kind, its text
// never appears inside the id, so a peer reading the id learns nothing of it.
func TestIDDoesNotContainKind(t *testing.T) {
	t.Parallel()

	for _, kind := range []spaces.Kind{
		spaces.KindPersonal,
		spaces.KindFamily,
		spaces.KindShared,
		spaces.KindResearch,
	} {
		got, err := spaces.NewSpace(kind, fixedTime)
		if err != nil {
			t.Fatalf("NewSpace(%q): %v", kind, err)
		}
		// A UUID string is hex + dashes; a kind word like "family" could only
		// appear if the id were derived from it. It must not.
		if strings.Contains(strings.ToLower(got.ID), strings.ToLower(string(kind))) {
			t.Errorf("id %q contains kind %q — the id derives from content (§38 violation)", got.ID, kind)
		}
	}
}

// TestViewCarriesOnlySafeMetadata guards the rendering: the exported View shape
// exposes exactly the id, kind and created-at, all §38-safe, and round-trips the
// values faithfully.
func TestViewCarriesOnlySafeMetadata(t *testing.T) {
	t.Parallel()

	s, err := spaces.NewSpace(spaces.KindFamily, fixedTime)
	if err != nil {
		t.Fatalf("NewSpace: %v", err)
	}
	v := spaces.NewView(s)
	if v.ID != s.ID {
		t.Errorf("View.ID = %q, want %q", v.ID, s.ID)
	}
	if v.Kind != string(s.Kind) {
		t.Errorf("View.Kind = %q, want %q", v.Kind, s.Kind)
	}
	if want := fixedTime.Format(time.RFC3339Nano); v.CreatedAt != want {
		t.Errorf("View.CreatedAt = %q, want %q", v.CreatedAt, want)
	}

	if got := spaces.NewViews(nil); got == nil || len(got) != 0 {
		t.Errorf("NewViews(nil) = %v, want an empty (non-nil) slice", got)
	}
}

// assertOpaqueV7 fails unless id parses as a UUID of version 7 (ADR-0017).
func assertOpaqueV7(t *testing.T, id string) {
	t.Helper()
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("id %q is not a valid UUID: %v", id, err)
	}
	if parsed.Version() != 7 {
		t.Fatalf("id %q is UUID version %d, want 7", id, parsed.Version())
	}
}
