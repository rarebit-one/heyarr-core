package catalog_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// selectRelease walks the harness want to SELECTED on one candidate.
func selectRelease(t *testing.T, h *harness, want string, cand acquisition.ReleaseCandidate) {
	t.Helper()
	ctx := context.Background()
	if _, err := h.cat.StartAcquisition(ctx, want); err != nil {
		t.Fatal(err)
	}
	ranked := acquisition.EvaluateAll([]acquisition.ReleaseCandidate{cand}, candidateProfile())
	if _, err := h.cat.RecordSearch(ctx, want, ranked, acquisition.Incumbent{}); err != nil {
		t.Fatal(err)
	}
}

// TestAcquireFactsSpellTheWantTheOpsWay: a work-scoped primary want with a
// selected release reads back as the op's scope/aspect spelling, the profile's
// name rather than id, the work's natural key and description, and the
// release's indexer-reported size.
func TestAcquireFactsSpellTheWantTheOpsWay(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cand := release("infohash:0123456789abcdef0123456789abcdef01234567", 2160, "hevc")
	cand.Attributes[policy.AttrSizeBytes] = policy.Num(12_000_000_000)
	selectRelease(t, h, h.want, cand)

	f, err := h.cat.AcquireFacts(context.Background(), h.want)
	if err != nil {
		t.Fatal(err)
	}
	want := catalog.AcquireFacts{
		ContentType: "movie", WorkKey: "movie:arrival:2016", WorkTitle: "Arrival", Year: 2016,
		Scope: catalogop.ScopeWork, Aspect: catalogop.AspectPrimary, Profile: "living-room",
		CandidateID: cand.ID, CandidateTitle: cand.Title, Size: 12_000_000_000,
	}
	if f != want {
		t.Errorf("facts =\n %+v\nwant\n %+v", f, want)
	}
}

// TestAcquireFactsForEditionAndItemScopes: an edition-scoped want names the
// edition KEY (what converges), an item-scoped want the item key, and a
// subtitle want its language, lower-cased.
func TestAcquireFactsForEditionAndItemScopes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.exec(t, `INSERT INTO editions (id, work_id, edition_key, label, edition_type, attributes, created_at)
		VALUES ('e1', 'w1', 'bluray-2160p', '', '', '{}', ?)`, stamp)
	h.exec(t, `INSERT INTO items (id, work_id, edition_id, item_key, title, attributes, created_at, updated_at)
		VALUES ('i1', 'w1', NULL, 's01e04', 'Episode 4', '{}', ?, ?)`, stamp, stamp)
	h.exec(t, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, item_id, aspect, language, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES ('want-ed', 'edition', 'w1', 'e1', NULL, 'primary', '', 'q1', 1, '', ?, ?)`, stamp, stamp)
	h.exec(t, `INSERT INTO desired_items
		(id, scope, work_id, edition_id, item_id, aspect, language, quality_profile_id, monitor, reason, created_at, updated_at)
		VALUES ('want-it', 'item', 'w1', NULL, 'i1', 'subtitle', 'EN', 'q1', 1, '', ?, ?)`, stamp, stamp)
	selectRelease(t, h, "want-ed", release("ed-release", 2160, "hevc"))
	selectRelease(t, h, "want-it", release("it-release", 2160, "hevc"))

	ed, err := h.cat.AcquireFacts(context.Background(), "want-ed")
	if err != nil {
		t.Fatal(err)
	}
	if ed.Scope != catalogop.ScopeEditionPrefix+"bluray-2160p" || ed.Aspect != catalogop.AspectPrimary {
		t.Errorf("edition want: scope %q aspect %q", ed.Scope, ed.Aspect)
	}
	it, err := h.cat.AcquireFacts(context.Background(), "want-it")
	if err != nil {
		t.Fatal(err)
	}
	if it.Scope != catalogop.ScopeItemPrefix+"s01e04" || it.Aspect != catalogop.AspectSubtitlePrefix+"en" {
		t.Errorf("item want: scope %q aspect %q", it.Scope, it.Aspect)
	}
	if it.Size != 0 {
		t.Errorf("a release with no size_bytes reads as %d, want 0", it.Size)
	}
}

// TestAcquireFactsNeedASelection: a want with nothing selected has nothing to
// tell a sibling, and says so with ErrNoSelection.
func TestAcquireFactsNeedASelection(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, err := h.cat.AcquireFacts(context.Background(), h.want); !errors.Is(err, catalog.ErrNoSelection) {
		t.Fatalf("err = %v, want ErrNoSelection", err)
	}
}
