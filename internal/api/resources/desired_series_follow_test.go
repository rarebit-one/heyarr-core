// Every HTTP response in this file is closed by the t.Cleanup the harness
// registers, which bodyclose cannot see through.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"net/http"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// ADR-0089: wanting a whole series IS following it. A work-scoped want on a
// series work resolves the series' metadata id and establishes a tv_series
// subscription — the follow spine reached from the want door — rather than a
// dead-end work-scoped want. These tests pin that bridge, its graceful fallback,
// its idempotency, and the cases that must NOT bridge.

func seriesDiscoveryRegistry(t *testing.T, title, externalID string) *providers.Registry {
	t.Helper()
	reg := providers.New(nil)
	fake := providers.NewFake("fake-tmdb", providers.CapabilityMetadata).
		OfferDiscovery(title, providers.DiscoveryCandidate{
			Title: title, Year: 2015, ExternalID: externalID, Type: followed.TypeTVSeries,
		})
	if err := reg.Register(fake); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestWantingASeriesEstablishesAFollow(t *testing.T) {
	reg := seriesDiscoveryRegistry(t, "The Expanse", "280619")
	h := newHarness(t, withProviders(reg)).seed()

	resp := postDesired(t, h, `{
		"work": {"content_type":"series","title":"The Expanse","year":2015},
		"quality_profile": "living-room",
		"reason": "want the whole show"
	}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, h.body(resp))
	}
	got := decodeDesired(t, h, resp)

	// The want came back as a SUBSCRIPTION, not a one-off want.
	if _, isWant := got["scope"]; isWant {
		t.Fatalf("a series want returned a one-off want, not a follow: %v", got)
	}
	if got["type"] != "tv_series" {
		t.Errorf("type = %v, want tv_series", got["type"])
	}
	if got["feed_ref"] != "280619" {
		t.Errorf("feed_ref = %v, want the resolved metadata id", got["feed_ref"])
	}
	if got["backfill"] != string(followed.BackfillFull) {
		t.Errorf("backfill = %v, want full (all aired episodes, ADR-0089 §3)", got["backfill"])
	}
	if got["monitor"] != true {
		t.Errorf("monitor = %v, want the want's monitor carried over", got["monitor"])
	}
	if got["quality_profile_id"] == "" || got["quality_profile_id"] == nil {
		t.Errorf("quality profile did not carry over: %v", got)
	}

	workID, _ := got["work_id"].(string)
	if workID == "" {
		t.Fatal("the follow must anchor to the series work")
	}
	if n := h.countRows(t, `SELECT count(*) FROM follow_sources WHERE work_id = ?`, workID); n != 1 {
		t.Errorf("follow_sources for the series = %d, want exactly one", n)
	}
	// The bare work-scoped want is SUBSUMED — the follow's per-episode wants are
	// the real desire now, not an abstract series-level row.
	if n := h.countRows(t,
		`SELECT count(*) FROM desired_items WHERE work_id = ? AND scope = 'work'`, workID); n != 0 {
		t.Errorf("a work-scoped want lingered beside the follow (%d); it should be subsumed", n)
	}
}

func TestWantingASeriesWithoutAProviderFallsBackToAWant(t *testing.T) {
	// No metadata provider configured: the series cannot be resolved, so the want
	// degrades to today's one-off want rather than erroring (ADR-0089 §2).
	h := newHarness(t, withProviders(providers.New(nil))).seed()

	resp := postDesired(t, h, `{
		"work": {"content_type":"series","title":"The Expanse","year":2015},
		"quality_profile": "living-room"
	}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, h.body(resp))
	}
	got := decodeDesired(t, h, resp)

	if got["scope"] != "work" {
		t.Fatalf("expected a one-off work-scoped want as fallback, got %v", got)
	}
	workID, _ := got["work_id"].(string)
	if workID == "" {
		t.Fatal("the fallback want must still anchor to a work")
	}
	if n := h.countRows(t, `SELECT count(*) FROM follow_sources WHERE work_id = ?`, workID); n != 0 {
		t.Errorf("no follow should exist without a provider, found %d", n)
	}
	if n := h.countRows(t, `SELECT count(*) FROM desired_items WHERE work_id = ?`, workID); n != 1 {
		t.Errorf("the fallback want row = %d, want one", n)
	}
}

func TestWantingASeriesAlreadyFollowedIsIdempotent(t *testing.T) {
	reg := seriesDiscoveryRegistry(t, "The Expanse", "280619")
	h := newHarness(t, withProviders(reg)).seed()

	body := `{"work":{"content_type":"series","title":"The Expanse","year":2015},"quality_profile":"living-room"}`
	first := postDesired(t, h, body)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first want = %d: %s", first.StatusCode, h.body(first))
	}
	workID, _ := decodeDesired(t, h, first)["work_id"].(string)

	second := postDesired(t, h, body)
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("second want = %d: %s", second.StatusCode, h.body(second))
	}
	if id2, _ := decodeDesired(t, h, second)["work_id"].(string); id2 != workID {
		t.Errorf("second want converged on a different work: %q vs %q", id2, workID)
	}
	if n := h.countRows(t, `SELECT count(*) FROM follow_sources WHERE work_id = ?`, workID); n != 1 {
		t.Errorf("wanting a followed series again duplicated the subscription: %d follows, want one", n)
	}
}

func TestWantingAMovieDoesNotEstablishAFollow(t *testing.T) {
	// A provider IS present — proving it is the content type, not the absence of a
	// provider, that keeps a movie a one-off want (a movie is not a subscription).
	reg := seriesDiscoveryRegistry(t, "The Conversation", "999")
	h := newHarness(t, withProviders(reg)).seed()

	resp := postDesired(t, h, `{
		"work": {"content_type":"movie","title":"The Conversation","year":1974},
		"quality_profile": "living-room"
	}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d: %s", resp.StatusCode, h.body(resp))
	}
	got := decodeDesired(t, h, resp)
	if got["scope"] != "work" {
		t.Fatalf("a movie want must stay a one-off want, got %v", got)
	}
	workID, _ := got["work_id"].(string)
	if n := h.countRows(t, `SELECT count(*) FROM follow_sources WHERE work_id = ?`, workID); n != 0 {
		t.Errorf("a movie want established a follow (%d); only series bridge", n)
	}
}
