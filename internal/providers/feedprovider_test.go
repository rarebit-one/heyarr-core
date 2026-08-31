package providers

import (
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
)

// The metadata capability's routing and its fake, the sibling of the indexer
// and download tests (M12). A fake feed provider must be substitutable for a
// real one everywhere a caller looks, or ADR-0058's values-in-values-out
// interface has failed at its one job.

func TestFakeEnumeratesWhatItWasOffered(t *testing.T) {
	f := NewFake("fake-tvdb", CapabilityMetadata)
	f.OfferFeed("tvdb:1",
		followed.FeedItem{Key: "S01E01", Title: "Pilot"},
		followed.FeedItem{Key: "S01E02", Title: "Second"})

	items, err := f.Enumerate(t.Context(), "tvdb:1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("enumerated %d items, want 2", len(items))
	}
	if f.Enumerations() != 1 {
		t.Errorf("Enumerations = %d, want 1", f.Enumerations())
	}
	// An unoffered ref is an empty feed, not an error — a real feed for a series
	// with no episodes yet behaves the same.
	empty, err := f.Enumerate(t.Context(), "tvdb:unknown")
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("an unoffered ref returned %d items, want 0", len(empty))
	}
}

func TestFakeEnumerateFailsWhenToldTo(t *testing.T) {
	f := NewFake("fake-tvdb", CapabilityMetadata)
	f.FailWith(errTest)
	if _, err := f.Enumerate(t.Context(), "tvdb:1"); err == nil {
		t.Fatal("a failing feed adapter must surface its error, not report an empty feed")
	}
}

func TestFeedProvidersRoutesOnlyMetadataProviders(t *testing.T) {
	reg := New(func() time.Time { return time.Unix(0, 0) })
	feed := NewFake("fake-tvdb", CapabilityMetadata)
	indexer := NewFake("fake-indexer", CapabilityIndexer)
	if err := reg.Register(feed); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(indexer); err != nil {
		t.Fatal(err)
	}

	feeds := reg.FeedProviders()
	if len(feeds) != 1 {
		t.Fatalf("FeedProviders returned %d, want 1 — only the metadata provider routes", len(feeds))
	}
	if feeds[0].Name() != "fake-tvdb" {
		t.Errorf("routed to %q, want fake-tvdb", feeds[0].Name())
	}
	if !reg.Has(CapabilityMetadata) {
		t.Error("Has(metadata) is false with a metadata provider registered")
	}
}

// A TVDB provider needs a key and defaults to the metadata capability, and its
// endpoint is optional because the client knows the well-known v4 base URL (M12).
func TestTVDBValidatesWithAKeyAndNoEndpoint(t *testing.T) {
	resolved, err := Validate([]Entry{{
		Name: "the-tvdb", Type: string(KindTVDB), APIKey: Secret("a-key"),
	}})
	if err != nil {
		t.Fatalf("a tvdb entry with a key and no endpoint should validate: %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolved %d entries, want 1", len(resolved))
	}
	if !hasCapability(resolved[0].Capabilities, CapabilityMetadata) {
		t.Errorf("a tvdb provider defaults to %v, want metadata", resolved[0].Capabilities)
	}
}

// A TVDB provider with no credential is refused at startup, not left to 401 on
// its first poll hours later.
func TestTVDBWithoutACredentialIsRefused(t *testing.T) {
	_, err := Validate([]Entry{{Name: "the-tvdb", Type: string(KindTVDB)}})
	if err == nil {
		t.Fatal("a tvdb provider with no credential must be refused")
	}
}

var errTest = errTestType("boom")

type errTestType string

func (e errTestType) Error() string { return string(e) }
