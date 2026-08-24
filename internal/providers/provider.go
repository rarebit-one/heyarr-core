package providers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
)

// Provider is one configured external service.
//
// # Values in, values out — nothing transport-shaped crosses this line
//
// There is no Client(), no BaseURL(), no RoundTripper and no Do(). A caller
// cannot tell whether this is an HTTP client, a replayed fixture or an
// in-process fake, and that indistinguishability is the whole test strategy
// for the external lane: a real indexer proxies real trackers and can never
// run in CI, so fixtures and fakes are not a convenience, they are the only
// path that will ever exist.
//
// Credentials, retries, timeouts, rate limits and API-version negotiation all
// live BEHIND this interface. A caller that had to know about any of them
// would be a caller a fixture could not stand in for.
type Provider interface {
	// Name is the operator's name for this provider, unique within an
	// instance. It is what appears in health, in routing and in a candidate's
	// Provider field, so it is the operator's word rather than a generated id.
	Name() string

	// Capabilities is what this provider can do. Ordered canonically.
	Capabilities() []Capability

	// Check exercises the provider and reports what it found.
	//
	// EXERCISES, not asserts. A provider that reported itself healthy because
	// it was configured would be advertising something it might not be able to
	// deliver, and work would route to it and then fail — which is worse than
	// advertising nothing at all. The same principle the worker-capability side
	// arrives at from the other direction.
	//
	// It returns a Health rather than an error because "unreachable" is a
	// REPORT, not a failure of the call: the health job wants to record what it
	// found, and an error would push a normal, expected outcome into the same
	// channel as "the health check itself broke".
	Check(ctx context.Context) Health
}

// Indexer searches for releases (§59, §60).
//
// Query in, []acquisition.ReleaseCandidate out. The candidate type is the
// domain's — from M3-04 — rather than a provider-local shape that would have to
// be converted, because a conversion is a second place for an attribute to be
// lost and §63 cannot report on what it never received.
type Indexer interface {
	Provider
	Search(ctx context.Context, q Query) ([]acquisition.ReleaseCandidate, error)
}

// Downloader performs the transfer an acquisition needs (§58).
//
// Declared here so the registry has a contract to route to and M3-10 has one to
// implement; Milestone 3's registry ships no implementation of it. Kept
// deliberately thin — add, observe, remove — because the interesting decisions
// (path mapping, labels, the session handshake) belong inside an implementation
// and would be exactly the transport detail this interface exists to keep out.
type Downloader interface {
	Provider
	// Transfers is everything this client is doing that belongs to Heyarr.
	//
	// Belonging matters: a download client is shared with the operator's own
	// transfers, and something that cannot tell them apart must never be
	// allowed to remove or re-target anything.
	Transfers(ctx context.Context) ([]Transfer, error)

	// Add hands the client a release to fetch and returns the transfer it
	// created.
	//
	// # Why this is on the interface
	//
	// It was not, and the omission was the whole of #225: the comment above
	// said "add, observe, remove", the interface had only observe, and
	// downloads.Client.Add existed with no caller anywhere outside tests
	// because nothing could reach it without holding the concrete type. A want
	// could be searched, scored and SELECTED, and then no code in the process
	// could traverse §64's SELECTED → QUEUED edge.
	//
	// It belongs here rather than in a job holding the concrete client for the
	// same reason Indexer.Search does: §59 makes routing the registry's job,
	// and a registry that can express "search with whatever indexes" and not
	// "fetch with whatever downloads" is not routing the one thing acquisition
	// exists for. The thin-interface argument for keeping it off is real, but
	// it was bought at the price of the capability being unreachable.
	//
	// `source` is a magnet URI or the URL of a .torrent/.nzb, and it is a
	// credential: on a private tracker it carries a passkey. Implementations
	// must Reveal() it exactly where they put it on the wire and nowhere else.
	//
	// Remove is deliberately NOT here. It is on downloads.Client, it has no
	// caller yet, and adding it now would recreate this issue's exact shape in
	// the other direction — an interface method that exists because a comment
	// promised it rather than because anything routes to it.
	Add(ctx context.Context, source secret.Value) (Transfer, error)
}

// Transfer is one external transfer, as a value.
//
// Deliberately not the download client's own struct: identity is the client's
// own identifier (an infohash, not a name — names get renamed and collide), and
// everything else is what Heyarr needs to drive §64's pipeline.
type Transfer struct {
	// ID is the download client's identifier for this transfer.
	ID string
	// Name is what the client calls it, for a human reading a queue.
	Name string
	// Done reports the transfer finished. Never trusted as evidence about
	// bytes — invariant 1 has Heyarr hash what arrived itself — but it is the
	// signal that there is something to hash.
	Done bool
	// BytesTotal and BytesDone drive progress reporting. Zero total means the
	// client has not said.
	BytesTotal int64
	BytesDone  int64
	// Path is where the client says the bytes are, in the CLIENT's filesystem
	// namespace. Translating it into one Heyarr can open is M3-10's path
	// mapping, and it is the most common operational failure in this class of
	// software.
	Path string
	// Error is the client's own failure message, empty when it is fine.
	Error string
}

// Query is what to search for (§60's manual search, and M3-12's job).
//
// A VALUE with no transport in it. A provider turns this into whatever its
// service wants; nothing here knows or cares what that is.
type Query struct {
	// Title is the work's title, already normalised by the caller.
	Title string
	// Year disambiguates, and is zero when unknown. Zero means "do not
	// constrain" rather than "the year 0" — a provider must not send it.
	Year int
	// ContentType is §12's content type: movie, series, music, book.
	//
	// Present because an indexer that can narrow by category returns far less
	// noise, and absent-means-everything is the safe reading.
	ContentType string
	// Limit bounds what a provider returns. Zero means the provider's own
	// default. It exists so a search cannot be made to return a hundred
	// thousand candidates for §63 to score.
	Limit int
}

// Validate refuses a query that cannot mean anything.
func (q Query) Validate() error {
	if strings.TrimSpace(q.Title) == "" {
		return fmt.Errorf("a search needs a title")
	}
	if q.Year < 0 || q.Year > 9999 {
		return fmt.Errorf("a year of %d is not a year", q.Year)
	}
	if q.Limit < 0 {
		return fmt.Errorf("a limit of %d is not a limit", q.Limit)
	}
	return nil
}

// Health is what a Check found.
//
// It is a report rather than a boolean because "unhealthy" with no reason is a
// status nobody can act on, and because the API version is the thing that
// answers "acquisitions stopped after I upgraded the service" in one request
// rather than an afternoon.
type Health struct {
	// Healthy reports whether the provider answered and is usable.
	Healthy bool
	// Detail says what happened, in a few words, for an operator reading
	// GET /api/v1/providers. It says what was observed rather than quoting an
	// error, and it must never contain a credential.
	Detail string
	// Version is the API version the service reported, empty when it did not
	// answer or does not say one.
	//
	// This is what replaces ADR-0023's version PINNING for a service Heyarr
	// does not install: not controlling the version does not mean ignoring it.
	// A version outside the supported range is reported unhealthy NAMING BOTH
	// NUMBERS, and is never a startup failure.
	Version string
	// CheckedAt is when this was observed. Zero means never checked, which is
	// distinct from unhealthy: a provider nobody has asked about yet and one
	// that failed lead to different actions.
	CheckedAt time.Time
}

// Unknown is the health of a provider nothing has checked yet.
//
// Not Healthy:false with an empty detail, which would read as "we looked and it
// is broken". "Nobody has looked" is a different statement and the distinction
// is the same one §56's satisfaction axes make.
func Unknown() Health {
	return Health{Healthy: false, Detail: "not checked yet"}
}

// Healthy builds a healthy report.
func Healthy(version string, at time.Time) Health {
	return Health{Healthy: true, Detail: "reachable", Version: version, CheckedAt: at}
}

// Unhealthy builds an unhealthy report. The detail is required: a status with
// no reason is one nobody can act on.
func Unhealthy(detail string, at time.Time) Health {
	if strings.TrimSpace(detail) == "" {
		detail = "unreachable"
	}
	return Health{Healthy: false, Detail: detail, CheckedAt: at}
}

// Checked reports whether anything has ever asked this provider how it is.
func (h Health) Checked() bool { return !h.CheckedAt.IsZero() }
