package followed

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/desired"
)

// Type is the kind of source a subscription follows.
//
// # Inferred and stored, never a caller's knob
//
// The follow surface is source-agnostic: a caller says "follow this" with an
// identity (a URL, a series id), and the system INFERS the type and routes to
// the matching feed adapter. The type is recorded because the poll loop needs
// to know which adapter to ask, not because the caller chose it. This mirrors
// the provider layer, where "which provider answers" is the registry's decision
// and never the caller's.
//
// # The set is open, and only TV is wired in Phase 1
//
// All four of M12's source types are named here so the vocabulary is fixed and
// the later phases are additions rather than renames — the same reason
// providers.Capability names `metadata` before anything implements it. Phase 1
// implements only TypeTVSeries; the other three are the shape their phases fill
// in.
type Type string

const (
	// TypeTVSeries follows a TV series: its feed adapter enumerates episodes and
	// air dates, and each episode is projected as one item-scoped want the
	// existing Torznab-search + torrent-grab pipeline acquires. Phase 1.
	TypeTVSeries Type = "tv_series"
	// TypePodcast follows a podcast: the RSS feed adapter enumerates entries and
	// the existing KindHTTP downloader fetches each enclosure. Phase 2.
	TypePodcast Type = "podcast"
	// TypeYouTubeChannel follows a channel: the channel-RSS feed adapter
	// enumerates videos and a yt-dlp downloader fetches each. Phase 3.
	TypeYouTubeChannel Type = "youtube_channel"
	// TypeRSSFeed follows a web feed: articles are captured as single-file HTML
	// Document assets. Phase 4.
	TypeRSSFeed Type = "rss_feed"
)

// Types lists every source type, in a stable order — it appears in errors and
// in API responses, and an order that depends on map iteration is one nobody
// can diff.
func Types() []Type {
	return []Type{TypeTVSeries, TypePodcast, TypeYouTubeChannel, TypeRSSFeed}
}

// Implemented reports whether a feed adapter for this type exists yet. Phase 1
// shipped TV; Phase 2 added podcast (RSS enclosure → the existing KindHTTP
// downloader); Phase 3 added YouTube (channel RSS → the KindYtDlp downloader,
// ADR-0062); Phase 4 adds generic RSS/Atom web feeds (articles → the KindWebCapture
// client, ADR-0063). Every declared type is now implemented; the method and its
// Validate guard remain so that a FUTURE type added to Types() before its adapter
// exists is refused loudly rather than silently going unpolled.
func (t Type) Implemented() bool {
	return t == TypeTVSeries || t == TypePodcast || t == TypeYouTubeChannel || t == TypeRSSFeed
}

// ParseType validates a source type from configuration or the wire. An unknown
// type is refused rather than ignored, for the reason providers.ParseCapability
// gives: a silently dropped type produces a source that is configured, healthy
// and never polled, which presents as "nothing ever gets archived" and is
// nobody's first guess.
func ParseType(s string) (Type, error) {
	normalised := Type(strings.ToLower(strings.TrimSpace(s)))
	for _, v := range Types() {
		if v == normalised {
			return v, nil
		}
	}
	out := make([]string, 0, len(Types()))
	for _, v := range Types() {
		out = append(out, string(v))
	}
	return "", fmt.Errorf("%q is not a source type — it must be one of %s",
		s, strings.Join(out, ", "))
}

// Backfill is how much of a source's back-catalogue to pull on the first poll.
type Backfill string

const (
	// BackfillFromNow archives only items emitted after the source is followed —
	// the conservative default. A channel with a thousand old videos is a large
	// first grab, and "follow from here" is what an operator usually means.
	BackfillFromNow Backfill = "from_now"
	// BackfillFull walks the whole back-catalogue into wants. Deliberate, and a
	// real capacity commitment.
	BackfillFull Backfill = "full"
)

// Backfills lists every backfill policy, in a stable order.
func Backfills() []Backfill { return []Backfill{BackfillFromNow, BackfillFull} }

// ParseBackfill validates a backfill policy from the wire.
func ParseBackfill(s string) (Backfill, error) {
	switch Backfill(strings.TrimSpace(s)) {
	case BackfillFromNow:
		return BackfillFromNow, nil
	case BackfillFull:
		return BackfillFull, nil
	default:
		return "", fmt.Errorf("backfill must be one of from_now, full, not %q", s)
	}
}

// maxReason bounds the free-text note, as it does on a DesiredItem.
const maxReason = 500

// Source is a FollowedSource: a standing subscription to archive everything a
// source emits, under one policy. It is control-plane state, single-writer,
// modelled on desired.Item.
type Source struct {
	ID string

	// WorkID is the Work every projected item belongs to — the series, the
	// podcast, the channel. Always set: an Item belongs to a Work (§11), the
	// semantic anchor that exists whether or not any bytes do, and it is what a
	// projected item-scoped want anchors to.
	WorkID string

	// Type is the inferred source type. See Type — it is stored so the poll
	// loop knows which feed adapter to ask, not chosen by the caller.
	Type Type

	// FeedRef is the source-stable handle the feed adapter resolves to enumerate
	// items: a TVDB series id, a podcast feed URL, a channel id. It is OPAQUE to
	// this package — the domain does not parse it, the adapter does — which is
	// what keeps the control plane source-agnostic. Required: a source with
	// nothing to poll is not a source.
	FeedRef string

	// QualityProfileID is the standard every projected item is measured against
	// (§62) — the profile the whole subscription archives at. Required, for the
	// reason a DesiredItem's is: "this should exist" with no statement of what
	// counts as existing cannot be evaluated (§56), and every item this source
	// projects inherits it.
	QualityProfileID string

	// Monitor is carried onto every projected want: whether to keep looking for
	// a better copy of each item once one is held (§60). It is the subscription
	// default, not a statement about any one item.
	Monitor bool

	// Backfill is how much history to pull on the first poll. Empty defaults to
	// BackfillFromNow in Validate.
	Backfill Backfill

	// Reason is free text an operator may attach — "Kate watches this". Never
	// interpreted; carried onto projected wants so a want six months later can
	// say where it came from.
	Reason string
}

// Validate checks a subscription, returning the first problem with enough
// context to fix it, and normalising as it goes.
func (s *Source) Validate() error {
	s.WorkID = strings.TrimSpace(s.WorkID)
	s.FeedRef = strings.TrimSpace(s.FeedRef)
	s.QualityProfileID = strings.TrimSpace(s.QualityProfileID)
	s.Reason = strings.TrimSpace(s.Reason)

	if _, err := ParseType(string(s.Type)); err != nil {
		return err
	}
	if !s.Type.Implemented() {
		// Refused rather than stored-and-ignored: a subscription that will never
		// be polled must fail loudly at creation, not sit looking healthy.
		return fmt.Errorf("following a %s source is not implemented yet — "+
			"heyarr follows tv_series and podcast so far", s.Type)
	}
	if s.WorkID == "" {
		return errors.New("a followed source must name the work its items belong to")
	}
	if s.FeedRef == "" {
		return errors.New("a followed source must carry a feed reference to poll")
	}
	if s.QualityProfileID == "" {
		return errors.New("a followed source must name a quality profile — " +
			"every item it archives is measured against it (§56)")
	}

	if s.Backfill == "" {
		s.Backfill = BackfillFromNow
	}
	if _, err := ParseBackfill(string(s.Backfill)); err != nil {
		return err
	}

	if len(s.Reason) > maxReason {
		return fmt.Errorf("the reason is %d characters, past the limit of %d",
			len(s.Reason), maxReason)
	}
	return nil
}

// FeedItem is one thing a feed adapter enumerated: the source-stable identity
// plus what the feed knows about it. It is a VALUE with no transport in it — a
// feed adapter turns a round-trip into a slice of these, and nothing downstream
// knows or cares what the round-trip was.
type FeedItem struct {
	// Key is the source-stable identity the feed adapter supplies — an "S02E05",
	// a podcast GUID, a YouTube video id, an RSS entry GUID. It is the item_key
	// that dedupes items across polls: the poll loop diffs a feed's items by
	// Key, and a Key it has seen is not a new item.
	Key string
	// Title is human-facing, for a queue a person reads.
	Title string
	// PublishedAt is when the source emitted it — an air date, a publish date.
	// Zero when the source does not say, which is distinct from the zero time
	// meaning anything else.
	PublishedAt time.Time
	// Attributes is what the feed knows about the item — a season/episode
	// number, a duration, an enclosure-URL hint. Per-item, per-type facts, held
	// as a map so a new source type is not a new column (the same rule the Item
	// entity's attributes follow, ADR-0056).
	Attributes map[string]string
}

// AttrEnclosureURL is the Attributes key a feed adapter uses to carry an item's
// direct-fetch URL — a podcast episode's <enclosure> (§55, M12 Phase 2).
//
// It is the seam between a NON-SEARCH source and acquisition. A TV episode has
// no bytes location until an indexer is searched, so it carries no enclosure and
// the search pipeline finds its release. A podcast episode's bytes location is
// exactly what the feed already handed us, so the adapter records it here and the
// poll projects it straight onto the want as a direct release the existing
// KindHTTP downloader fetches — there is nothing to search for. The key lives on
// the neutral FeedItem, not in any adapter, so any future direct-URL source
// (Phase 4's captured articles) reuses the same seam.
const AttrEnclosureURL = "enclosure_url"

// YtDlpSourceScheme prefixes an enclosure URL a feed adapter knows must be
// fetched by running yt-dlp rather than by a plain HTTP GET (§58, M12 Phase 3,
// ADR-0062).
//
// It is the seam that keeps the routing decision on the SOURCE's shape, the way
// KindHTTP routes on the http(s) scheme and a torrent client on `magnet:`. A
// YouTube video's bytes location is the watch page, which is itself an http URL —
// so if the adapter recorded it bare, the plain-HTTP download client would claim
// it and fetch the HTML page instead of the video. Tagging it here (the
// KindYoutube adapter writes YtDlpSourceScheme+watchURL into AttrEnclosureURL)
// makes the plain-HTTP client refuse it — its scheme is no longer http — and the
// KindYtDlp client claim it, with no dependence on the order the two happen to be
// registered in. The tag lives here, beside AttrEnclosureURL, because this is the
// one place the non-search-source-to-acquisition seam is defined; both the feed
// adapter that writes it and the download client that strips it read it from here.
const YtDlpSourceScheme = "ytdlp:"

// WebCaptureSourceScheme prefixes an enclosure URL a feed adapter knows must be
// archived by capturing the page into a self-contained single-file HTML rather
// than fetched as a file (§58, M12 Phase 4, ADR-0063).
//
// It is the same seam as YtDlpSourceScheme, for the same reason: a web article's
// address is an http URL, so if the adapter recorded it bare the plain-HTTP
// download client would claim it and store the raw, dependency-laden page.
// Tagging it here (the KindWebFeed adapter writes WebCaptureSourceScheme+articleURL
// into AttrEnclosureURL) makes the plain-HTTP client refuse it — its scheme is no
// longer http — and the KindWebCapture client claim it, independent of the order
// the clients are registered in.
const WebCaptureSourceScheme = "webcapture:"

// EnclosureURL is the item's direct-fetch URL, if the feed adapter supplied one.
//
// A non-empty return is what tells the poll loop this item is acquired directly
// (the feed IS the discovery) rather than by searching an indexer — see
// AttrEnclosureURL. An item without one falls through to the search pipeline, so
// the two source shapes need no type switch at the projection site.
func (i FeedItem) EnclosureURL() string {
	return strings.TrimSpace(i.Attributes[AttrEnclosureURL])
}

// Validate refuses a feed item that cannot be diffed or projected.
func (i *FeedItem) Validate() error {
	i.Key = strings.TrimSpace(i.Key)
	i.Title = strings.TrimSpace(i.Title)
	if i.Key == "" {
		// Without a stable key an item cannot be deduped across polls, so every
		// poll would re-project it and the archive would fill with duplicate
		// wants. A feed adapter that cannot supply one has not identified the
		// item.
		return errors.New("a feed item must carry a source-stable key")
	}
	return nil
}

// ProjectWant turns a resolved Item into the per-item DesiredItem this source
// wants archived. This is the heart of following, and it is pure: the feed
// enumerates FeedItems, the edge upserts each as a byte-less Item row (ADR-0056)
// and hands back its id, and this projects that id onto an item-scoped want the
// existing acquisition pipeline then drives — search, evaluate, grab, verify,
// ingest — untouched.
//
// The want inherits the subscription's profile, monitoring and reason. It is an
// ordinary desired.Item; nothing about it says "a source projected me", which is
// exactly the point — the pipeline stays source-agnostic.
func (s Source) ProjectWant(itemID string) desired.Item {
	return desired.Item{
		Scope:            desired.ScopeItem,
		WorkID:           s.WorkID,
		ItemID:           strings.TrimSpace(itemID),
		QualityProfileID: s.QualityProfileID,
		Monitor:          s.Monitor,
		Reason:           s.Reason,
	}
}
