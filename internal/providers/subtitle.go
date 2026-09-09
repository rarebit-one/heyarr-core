package providers

import (
	"context"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
)

// SubtitleProvider fetches a subtitle for content the library already holds
// (ADR-0085, M12 Phase 5).
//
// # This is the capability CapabilitySubtitle routes to
//
// It is the sibling of FeedProvider and Indexer — an optional capability a
// provider declares by implementing, discovered by type assertion beside the
// base Provider — and it exists because "a subtitle that exists nowhere" is a
// desired-state gap the caption path (ADR-0040, ADR-0084) cannot fill from bytes
// it does not have. A subtitle provider answers a question no metadata or
// indexer provider does: "which subtitle files exist for this exact release, and
// where do I fetch the chosen one."
//
// # Values in, values out — the same line the rest of this package draws
//
// A SubtitleQuery in (external ids and/or a moviehash the caller already holds
// about the video, plus the languages wanted), SubtitleCandidates out; then a
// file id back in, a SubtitleLink out. No transport, no credential and no
// pagination cross this line, exactly as none crosses Indexer or FeedProvider: a
// caller must not be able to tell a live OpenSubtitles adapter from a replayed
// fixture, because a subtitle service is as un-reproducible in CI as an indexer
// is (ADR-0026) and fixtures are the only test it will ever have.
//
// # Two calls, not one, and why
//
// FindSubtitle and ResolveSubtitle are separate because the resolution is what
// a provider like OpenSubtitles charges against a daily download quota: a search
// is cheap and cacheable, a resolve spends the budget. Keeping them apart lets a
// caller rank and choose among candidates (the adapter's own ranking, ADR-0060
// §3) and spend one download on the winner, rather than resolving every
// candidate to compare them. It is also what lets step 3's acquisition record
// the chosen file as a direct release before the download link — a per-fetch
// temporary URL — is ever minted.
type SubtitleProvider interface {
	Provider

	// FindSubtitle returns the subtitle files that match the query, as neutral
	// candidates. An empty result is a modelled outcome — nothing matched — not
	// an error; an error is reserved for a call that could not be made (the
	// service was unreachable, or the credential was rejected), which the caller
	// must see rather than read as "this release has no subtitles".
	FindSubtitle(ctx context.Context, q SubtitleQuery) ([]SubtitleCandidate, error)

	// ResolveSubtitle turns a candidate's FileID into the URL its bytes can be
	// fetched from, plus what the quota has left. It is the call that spends a
	// download against the provider's budget, so a caller makes it once, for the
	// candidate it chose.
	ResolveSubtitle(ctx context.Context, fileID string) (SubtitleLink, error)
}

// SubtitleQuery is what to look a subtitle up by (ADR-0085).
//
// A VALUE with no transport in it. It carries what heyarr already knows about a
// held video — its external ids, the season/episode for a series, and the
// OpenSubtitles-style moviehash a caller can compute from the video bytes — and
// the languages wanted. A provider uses whatever subset it supports; nothing
// here knows or cares which. The hash is the strongest signal (it matches a
// subtitle synced to this exact release), and the ids are the fallback when the
// bytes have not been hashed.
type SubtitleQuery struct {
	// IMDBID and TMDBID identify the WORK (or the episode, for a series) at an
	// external service, without leading "tt" or other prefixes — the digits the
	// provider's API takes. Either, both, or neither may be set.
	IMDBID string
	TMDBID string
	// Season and Episode narrow a series query to one episode. Zero means "not a
	// series query" rather than "season zero"; a provider must not send a zero it
	// was not given.
	Season  int
	Episode int
	// MovieHash is the OpenSubtitles hash of the video file, when the caller has
	// computed it. It matches a subtitle timed to this exact release and is the
	// strongest match a query can carry; empty means the caller is matching by id
	// alone.
	MovieHash string
	// Languages is the wanted languages as ISO-639-1 codes (en first), in
	// preference order. Empty means the provider's default, which no caller should
	// rely on — a subtitle want always names its language (ADR-0085).
	Languages []string
}

// SubtitleCandidate is one subtitle file a search matched (ADR-0085).
//
// The neutral counterpart to an indexer's ReleaseCandidate: a provider-agnostic
// value a caller ranks and chooses among, carrying the FileID a resolve acts on.
// The ranking signals (HearingImpaired, DownloadCount, Release) are here so the
// adapter — the identity authority for its source (ADR-0060 §3) — can pick the
// best file; heyarr's quality profile does not gate a subtitle.
type SubtitleCandidate struct {
	// FileID is the provider-native identity of the subtitle FILE, the value
	// ResolveSubtitle takes. It is the file, not the "subtitle" grouping a service
	// may wrap several files in — a resolve fetches one file's bytes.
	FileID string
	// Language is the subtitle's language as an ISO-639-1 code, so a caller can
	// confirm the match against the want's language and attach it under the right
	// filename (ADR-0084's <stem>.<lang>.srt shape).
	Language string
	// Release is the release name the subtitle was uploaded for, when the service
	// gives one — a human signal for "is this timed to my copy", decoration a
	// caller may show, never an identity.
	Release string
	// HearingImpaired reports the subtitle carries SDH/CC annotations. A ranking
	// signal (most callers prefer a clean subtitle), not a filter here.
	HearingImpaired bool
	// DownloadCount is how often the service has served this file, a rough trust
	// signal the adapter ranks by. Zero means the service did not say.
	DownloadCount int
	// Format is the subtitle format the service reports (srt, ass, …), lower-case,
	// so a caller can confirm it is a text format the caption path serves before
	// spending a download. Empty means the service did not say.
	Format string
}

// SubtitleLink is where a chosen subtitle's bytes are, plus what the quota has
// left (ADR-0085).
//
// It is the resolve's output: the URL step 3 hands to the KindHTTP download
// client (ADR-0060), the filename the service suggests, and the quota counters an
// operator and the want's re-poll cadence must respect. The URL is a secret.Value
// because a provider like OpenSubtitles returns a per-fetch temporary link
// carrying a token tied to the account — it must stay out of every log and out of
// the fixture corpus, exactly as an indexer's passkey-bearing source does
// (providers.Downloader.Add).
type SubtitleLink struct {
	// URL is the direct http(s) link the subtitle bytes are fetched from. A
	// secret because it carries a per-fetch token; Reveal() it only where it is
	// put on the wire.
	URL secret.Value
	// FileName is the name the service suggests for the file, when it gives one —
	// a hint for the RelPath step 3 ingests under, so identification assigns
	// RoleSubtitle and the right language. Empty means the caller names it from
	// the candidate's language and the video's stem.
	FileName string
	// Remaining is how many downloads the account's quota has left after this one,
	// as the service reported it; negative means the service did not say. The
	// signal a rate limiter and the want's back-off read so a followed series does
	// not exhaust the budget in one poll.
	Remaining int
	// ResetsAt is when the download quota next resets, zero when the service did
	// not say. It is what turns "quota exhausted" into a definite "try again
	// after", rather than a blind retry that keeps 40x-ing.
	ResetsAt time.Time
}
