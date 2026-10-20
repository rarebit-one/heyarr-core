package worker

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/catalogop"
	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

const fakeInfohash = "0123456789abcdef0123456789abcdef01234567"

// recordingPublisher captures what the grab asked it to publish, and can be
// made to fail so the retry contract is observable.
type recordingPublisher struct {
	calls []string
	fail  error
}

func (p *recordingPublisher) Publish(_ context.Context, desiredItemID, transferID string) error {
	p.calls = append(p.calls, desiredItemID+"|"+transferID)
	return p.fail
}

func (h *grabHarness) grabWith(t *testing.T, candidateID string, pub AcquirePublisher) error {
	t.Helper()
	payload, err := json.Marshal(acquisition.GrabPayload{DesiredItemID: h.want, CandidateID: candidateID})
	if err != nil {
		t.Fatal(err)
	}
	handler := GrabReleaseHandler(h.reg, h.cat, slog.New(slog.DiscardHandler), WithAcquirePublisher(pub))
	return handler(t.Context(), jobs.Job{Type: acquisition.GrabJobType, Payload: payload})
}

// TestAGrabPublishesTheChosenReleaseForSiblings: the grab hands the want and
// the client's transfer id to the publisher, once, and the want reaches QUEUED.
func TestAGrabPublishesTheChosenReleaseForSiblings(t *testing.T) {
	t.Parallel()
	h := newGrabHarness(t)
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	if err := h.grabWith(t, h.selectedID(t), pub); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Name(); got != "QUEUED" {
		t.Fatalf("after a grab the want is %s, want QUEUED", got)
	}
	if len(pub.calls) != 1 || !strings.HasPrefix(pub.calls[0], h.want+"|") {
		t.Fatalf("publisher calls = %v, want one for %s", pub.calls, h.want)
	}
	// A re-run of the same job does not publish twice: the want is QUEUED and
	// the handler skips before it reaches the client.
	if err := h.grabWith(t, h.selectedID(t), pub); err != nil {
		t.Fatal(err)
	}
	if len(pub.calls) != 1 {
		t.Fatalf("a re-run published again: %v", pub.calls)
	}
}

// TestAFailedPublishLeavesTheWantSelectedAndRetries: publishing is between the
// row and the transition on purpose. If it fails the job errors (so it retries)
// and the want stays SELECTED; the retry publishes again and then advances.
func TestAFailedPublishLeavesTheWantSelectedAndRetries(t *testing.T) {
	t.Parallel()
	h := newGrabHarness(t)
	h.fake.Offer("Arrival", offer("good", 2160, "hevc"))
	if err := h.run(t); err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{fail: errors.New("op store is away")}
	if err := h.grabWith(t, h.selectedID(t), pub); err == nil {
		t.Fatal("a failed publish did not fail the job")
	}
	if got := h.state(t).Name(); got != "SELECTED" {
		t.Fatalf("after a failed publish the want is %s, want SELECTED", got)
	}
	pub.fail = nil
	if err := h.grabWith(t, h.selectedID(t), pub); err != nil {
		t.Fatal(err)
	}
	if got := h.state(t).Name(); got != "QUEUED" {
		t.Fatalf("after the retry the want is %s, want QUEUED", got)
	}
	if len(pub.calls) != 2 {
		t.Fatalf("publisher calls = %v, want 2", pub.calls)
	}
}

// fakeFacts and fakeOps stand in for the catalog and the op-log store.
type fakeFacts struct {
	facts catalog.AcquireFacts
	err   error
}

func (f fakeFacts) AcquireFacts(context.Context, string) (catalog.AcquireFacts, error) {
	return f.facts, f.err
}

type fakeOps struct {
	heads    []string
	recorded []string
}

func (o *fakeOps) Heads(context.Context) ([]string, error) { return o.heads, nil }
func (o *fakeOps) RecordOps(_ context.Context, ops []string) error {
	o.recorded = append(o.recorded, ops...)
	return nil
}

func newPublisher(t *testing.T, facts catalog.AcquireFacts) (*CatalogAcquirePublisher, *fakeOps) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	ops := &fakeOps{heads: []string{"sha256:" + strings.Repeat("ab", 32)}}
	return &CatalogAcquirePublisher{
		Facts:  fakeFacts{facts: facts},
		Ops:    ops,
		Signer: priv,
		Site:   "north",
		Now:    func() time.Time { return time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC) },
	}, ops
}

func arrivalFacts() catalog.AcquireFacts {
	return catalog.AcquireFacts{
		ContentType:    "movie",
		WorkKey:        "movie:arrival:2016",
		WorkTitle:      "Arrival",
		Year:           2016,
		Scope:          catalogop.ScopeWork,
		Aspect:         catalogop.AspectPrimary,
		Profile:        "living-room",
		CandidateID:    "infohash:" + fakeInfohash,
		CandidateTitle: "Arrival 2016 2160p",
		Size:           12_000_000_000,
	}
}

// TestThePublishedOpCarriesTheFactsAndNothingSecret: the op a sibling will
// read names the work, the scope, the profile and the infohash; it carries no
// trackers (so no passkey can leak) and is signed by this peer.
func TestThePublishedOpCarriesTheFactsAndNothingSecret(t *testing.T) {
	t.Parallel()
	pub, ops := newPublisher(t, arrivalFacts())
	// The client reports the hash upper-case; the op pins it lower-case.
	if err := pub.Publish(t.Context(), "want-1", strings.ToUpper(fakeInfohash)); err != nil {
		t.Fatal(err)
	}
	if len(ops.recorded) != 1 {
		t.Fatalf("recorded %d ops, want 1", len(ops.recorded))
	}
	op, err := catalogop.Verify(ops.recorded[0])
	if err != nil {
		t.Fatalf("the published op does not verify: %v", err)
	}
	if op.Kind != catalogop.OpAcquire || op.Acquire == nil {
		t.Fatalf("kind = %s", op.Kind)
	}
	a := op.Acquire
	if op.Target.ContentType != "movie" || op.Target.WorkKey != "movie:arrival:2016" {
		t.Errorf("target = %+v", op.Target)
	}
	if a.Infohash != fakeInfohash || a.Profile != "living-room" || a.Scope != catalogop.ScopeWork ||
		a.Aspect != catalogop.AspectPrimary || a.WorkTitle != "Arrival" || a.Year != 2016 ||
		a.Title != "Arrival 2016 2160p" || a.Size != 12_000_000_000 || a.Site != "north" || a.Private {
		t.Errorf("acquire body = %+v", a)
	}
	if len(a.Trackers) != 0 {
		t.Errorf("trackers published: %v", a.Trackers)
	}
	if len(op.Prev) != 1 || op.Prev[0] != ops.heads[0] {
		t.Errorf("prev = %v, want the store's heads", op.Prev)
	}
}

// TestNonTorrentTransfersAreNotPublished: an HTTP enclosure or a usenet
// article has no infohash, so there is nothing a sibling could join.
func TestNonTorrentTransfersAreNotPublished(t *testing.T) {
	t.Parallel()
	pub, ops := newPublisher(t, arrivalFacts())
	for _, id := range []string{"fake:1", "http-7", "", "nzb-abc"} {
		if err := pub.Publish(t.Context(), "want-1", id); err != nil {
			t.Fatalf("transfer %q: %v", id, err)
		}
	}
	if len(ops.recorded) != 0 {
		t.Fatalf("published %d ops for non-torrent transfers", len(ops.recorded))
	}
}

// TestAClientThatTookADifferentReleaseIsNotPublished: when the selected
// candidate's id is an infohash and the client's transfer hash disagrees, the
// publisher refuses rather than telling siblings to fetch bytes this site did
// not choose.
func TestAClientThatTookADifferentReleaseIsNotPublished(t *testing.T) {
	t.Parallel()
	pub, ops := newPublisher(t, arrivalFacts())
	other := strings.Repeat("f", 40)
	if err := pub.Publish(t.Context(), "want-1", other); err == nil {
		t.Fatal("a mismatched infohash was published")
	}
	if len(ops.recorded) != 0 {
		t.Fatalf("published %d ops despite the mismatch", len(ops.recorded))
	}
	// A candidate id that is NOT an infohash (a hashed guid) cannot disagree,
	// so the client's hash is taken as the identity.
	f := arrivalFacts()
	f.CandidateID = "sha256:" + strings.Repeat("0", 64)
	pub, ops = newPublisher(t, f)
	if err := pub.Publish(t.Context(), "want-1", other); err != nil {
		t.Fatal(err)
	}
	if len(ops.recorded) != 1 {
		t.Fatalf("recorded %d ops, want 1", len(ops.recorded))
	}
}

// TestNoSelectionPublishesNothing: a want whose selection went away between
// the grab and now has nothing to tell a sibling, and that is not an error.
func TestNoSelectionPublishesNothing(t *testing.T) {
	t.Parallel()
	pub, ops := newPublisher(t, catalog.AcquireFacts{})
	pub.Facts = fakeFacts{err: catalog.ErrNoSelection}
	if err := pub.Publish(t.Context(), "want-1", fakeInfohash); err != nil {
		t.Fatal(err)
	}
	if len(ops.recorded) != 0 {
		t.Fatalf("published %d ops with no selection", len(ops.recorded))
	}
}

// TestANilPublisherIsSafe: a node without a signer publishes nothing and the
// grab is unaffected.
func TestANilPublisherIsSafe(t *testing.T) {
	t.Parallel()
	var pub *CatalogAcquirePublisher
	if err := pub.Publish(t.Context(), "want-1", fakeInfohash); err != nil {
		t.Fatal(err)
	}
}
