package providers

import (
	"context"

	"github.com/rarebit-one/heyarr-core/internal/domain/followed"
)

// FeedProvider enumerates the items a followed source emits (M12).
//
// # This is the metadata provider capability.go reserved
//
// CapabilityMetadata has existed since Milestone 3 "with nothing implementing
// it" precisely so that the first metadata provider would be an addition to
// configuration rather than a change to the registry (see capability.go). This
// is the contract that capability finally routes to: a FeedProvider advertises
// CapabilityMetadata, and a followed source's poll loop asks it "what items does
// this source have now?".
//
// # Values in, values out — the same line the rest of this package draws
//
// A ref in (the source-stable handle a FollowedSource carries — a TVDB series
// id, a podcast feed URL, a channel id; opaque to the caller, parsed by the
// adapter), a slice of followed.FeedItem out. No transport, no credential and no
// pagination cross this line, exactly as none crosses Indexer or Downloader: a
// caller must not be able to tell a live TVDB adapter from a replayed fixture,
// because a metadata service is as un-reproducible in CI as an indexer is
// (ADR-0026) and fixtures are the only test it will ever have.
//
// # Deliberately source-agnostic and provider-agnostic
//
// Enumerate returns the neutral followed.FeedItem, never a TVDB- or
// TMDB-shaped value, so the source's Type — not the choice of metadata service
// — is what a projection reads, and a second implementation (TMDB) slots in
// behind this interface without the poll loop changing. The caller expresses
// which source; the registry routes to whichever CapabilityMetadata provider
// answers for it.
type FeedProvider interface {
	Provider

	// Enumerate returns the items the source identified by ref currently has,
	// as neutral FeedItems. It is the feed adapter's whole job: for a TV series,
	// "which episodes should exist and when did/will they air"; for a podcast,
	// the entries; for a channel, the videos.
	//
	// It returns an error rather than a Health because, unlike Check, a failed
	// enumeration is a failure of the call the poll loop must see and retry —
	// the loop decides the hold-off, and folding "unreachable" into an empty
	// slice would silently report a source as having emitted nothing.
	Enumerate(ctx context.Context, ref string) ([]followed.FeedItem, error)
}
