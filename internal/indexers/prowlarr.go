package indexers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/acquisition"
	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/transporterr"
)

// The Prowlarr aggregator (ADR-0090).
//
// A KindTorznab provider points at ONE indexer's Torznab feed — Prowlarr's
// per-indexer `/{id}/api`. This client points at Prowlarr itself and speaks its
// own `/api/v1`, so one provider covers EVERY indexer Prowlarr manages, and the
// set is answered LIVE by Prowlarr at search time rather than held in heyarr's
// config: add or remove an indexer in Prowlarr and the next search reflects it.
//
// Why a second client rather than the Torznab one: Prowlarr's aggregate search
// (`/api/v1/search`) answers JSON, not the Torznab XML the sibling client
// parses, and it authenticates with an `X-Api-Key` header rather than an
// `apikey` query parameter. Everything the two share — the ReleaseCandidate
// shape, the id/source preference order, the size-only honesty about attributes,
// the credential-scrubbing of errors — is shared deliberately (this file reuses
// digest, transporterr and the policy mapping), and the wire format is the only
// thing that differs.

const (
	// prowlarrSearchTimeout bounds one aggregate search. Generous, because
	// Prowlarr is itself proxying several trackers behind this one call — the
	// same reason the Torznab client's timeout is generous — and bounded so a
	// search job holding a lease cannot wait forever on a tracker that will
	// never answer.
	prowlarrSearchTimeout = 60 * time.Second
	// prowlarrCheckTimeout bounds a health check. Shorter: it only reads
	// Prowlarr's own indexer list, which does not touch a tracker.
	prowlarrCheckTimeout = 15 * time.Second
	// prowlarrMaxBody bounds what will be read from Prowlarr. An aggregate
	// search across many indexers can be large; this guards a misdirected
	// endpoint streaming something unbounded, not a limit a real response nears.
	prowlarrMaxBody = 32 << 20
)

// ProwlarrOptions configure a Prowlarr aggregator client.
type ProwlarrOptions struct {
	// Name is the provider's name from configuration — the operator's word,
	// used in health, in routing and in every candidate's Provider field.
	Name string
	// Endpoint is Prowlarr's BASE URL (e.g. http://127.0.0.1:9696), not a
	// per-indexer `/{id}/api` path. The `/api/v1/...` routes are composed onto it.
	Endpoint string
	// APIKey is Prowlarr's api key, sent as the `X-Api-Key` header.
	APIKey string
	// HTTPClient is injected by tests. Nil means a client of this package's own
	// making — callers outside a test have no business supplying one.
	HTTPClient *http.Client
	// Now is the injected clock.
	Now func() time.Time
}

// prowlarrClient is one Prowlarr instance, aggregating every indexer it manages.
type prowlarrClient struct {
	name     string
	endpoint string
	apiKey   string
	http     *http.Client
	now      func() time.Time
}

// Compile-time proof that this satisfies the registry's contracts.
var (
	_ providers.Provider = (*prowlarrClient)(nil)
	_ providers.Indexer  = (*prowlarrClient)(nil)
)

// NewProwlarr builds a client, refusing configuration that cannot work.
func NewProwlarr(o ProwlarrOptions) (*prowlarrClient, error) {
	if strings.TrimSpace(o.Name) == "" {
		return nil, errors.New("indexers: a prowlarr provider needs a name")
	}
	if strings.TrimSpace(o.Endpoint) == "" {
		return nil, fmt.Errorf("indexers: prowlarr %q has no endpoint", o.Name)
	}
	if _, err := url.Parse(o.Endpoint); err != nil {
		return nil, fmt.Errorf("indexers: prowlarr %q has an endpoint that is not a URL: %w", o.Name, err)
	}
	c := &prowlarrClient{
		name:     o.Name,
		endpoint: strings.TrimRight(strings.TrimRight(o.Endpoint, "/"), "?&"),
		apiKey:   o.APIKey,
		http:     o.HTTPClient,
		now:      o.Now,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: prowlarrSearchTimeout}
	}
	if c.now == nil {
		c.now = func() time.Time { return time.Now().UTC() }
	}
	return c, nil
}

// Name is the operator's name for this provider.
func (c *prowlarrClient) Name() string { return c.name }

// Capabilities is what this provider can do.
func (c *prowlarrClient) Capabilities() []providers.Capability {
	return []providers.Capability{providers.CapabilityIndexer}
}

// prowlarrIndexer is one row of Prowlarr's `/api/v1/indexer`.
type prowlarrIndexer struct {
	Name   string `json:"name"`
	Enable bool   `json:"enable"`
}

// Check reports reachability AND how many indexers Prowlarr has enabled.
//
// The count is the point (ADR-0090 §2): a Prowlarr that is up but has zero
// enabled indexers is healthy-but-empty — which is WHY a search finds nothing —
// and that is a different, actionable state from "unreachable". Returns a Health
// rather than an error because "unreachable" is a REPORT, not a failed call.
func (c *prowlarrClient) Check(ctx context.Context) providers.Health {
	body, err := c.get(ctx, "/api/v1/indexer", nil, prowlarrCheckTimeout)
	if err != nil {
		return providers.Unhealthy(c.detailFor(err), c.now())
	}
	var idx []prowlarrIndexer
	if err := json.Unmarshal(body, &idx); err != nil {
		return providers.Unhealthy("Prowlarr answered with something that is not its indexer list", c.now())
	}
	enabled := 0
	for _, i := range idx {
		if i.Enable {
			enabled++
		}
	}
	h := providers.Healthy("v1", c.now())
	switch enabled {
	case 0:
		h.Detail = "reachable — Prowlarr, but no indexers are enabled"
	case 1:
		h.Detail = "reachable — Prowlarr, 1 indexer"
	default:
		h.Detail = fmt.Sprintf("reachable — Prowlarr, %d indexers", enabled)
	}
	return h
}

// prowlarrCategory is one Torznab category on a release.
type prowlarrCategory struct {
	ID int `json:"id"`
}

// prowlarrRelease is one row of Prowlarr's `/api/v1/search`.
//
// Only the fields heyarr consumes are declared; Prowlarr sends more and the
// extra is ignored. Size is a pointer so "0 bytes" (a real, if odd, claim) is
// tellable from "absent" — the size-is-the-one-real-attribute honesty the
// Torznab side keeps (attributes.go).
type prowlarrRelease struct {
	Title       string             `json:"title"`
	Size        *int64             `json:"size"`
	GUID        string             `json:"guid"`
	InfoHash    string             `json:"infoHash"`
	DownloadURL string             `json:"downloadUrl"`
	MagnetURL   string             `json:"magnetUrl"`
	Indexer     string             `json:"indexer"`
	Categories  []prowlarrCategory `json:"categories"`
}

// Search fans one query across every indexer Prowlarr manages (§59, §60).
func (c *prowlarrClient) Search(ctx context.Context, q providers.Query) ([]acquisition.ReleaseCandidate, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}

	params := url.Values{
		"query": []string{q.Title},
		"type":  []string{"search"},
	}
	if q.Year > 0 {
		// Appended to the query, as the Torznab client does (search_test): the
		// aggregate `search` type takes a free-text query, and a separate year
		// parameter would be honoured or ignored per underlying indexer — a
		// difference in results nobody could see.
		params.Set("query", fmt.Sprintf("%s %d", q.Title, q.Year))
	}
	if cats := prowlarrCategories(q.ContentType); cats != "" {
		params.Set("categories", cats)
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}

	body, err := c.get(ctx, "/api/v1/search", params, prowlarrSearchTimeout)
	if err != nil {
		return nil, err
	}
	var releases []prowlarrRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, fmt.Errorf("prowlarr: the search answer was not a release list: %w", err)
	}
	return prowlarrCandidates(c.name, releases), nil
}

// prowlarrCategories maps §12's content type onto Torznab category ids, which
// Prowlarr's search takes as a comma-separated list. An unknown or empty content
// type constrains nothing (search everything) — the same absent-means-everything
// reading the Torznab client's functionFor uses.
func prowlarrCategories(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "movie":
		return "2000"
	case "series", "tv":
		return "5000"
	case "music":
		return "3000"
	case "book":
		return "7000"
	default:
		return ""
	}
}

// prowlarrCandidates maps Prowlarr's releases onto the domain's values.
//
// Mirrors indexers.candidates: skip the nameless, derive a stable id that is not
// a position in the response, carry only what the release actually asserts, and
// sort by id so §63's tie-breaks are deterministic.
func prowlarrCandidates(provider string, releases []prowlarrRelease) []acquisition.ReleaseCandidate {
	out := make([]acquisition.ReleaseCandidate, 0, len(releases))
	for _, r := range releases {
		title := strings.TrimSpace(r.Title)
		if title == "" {
			// A release with no name cannot be explained or told from another
			// nameless one — the same drop the Torznab side makes.
			continue
		}
		out = append(out, acquisition.ReleaseCandidate{
			ID:         prowlarrCandidateID(r),
			Title:      title,
			Provider:   provider,
			Attributes: prowlarrAttributes(r),
			Source:     prowlarrSource(r),
		})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// prowlarrCandidateID is stable for a release and independent of its position.
//
// Same preference order and privacy stance as indexers.candidateID: the infohash
// (BitTorrent's own identity, not a credential) used directly; else the guid
// HASHED, because a guid is frequently a magnet URI whose passkey identifies a
// person and this id is stored and returned by the API.
func prowlarrCandidateID(r prowlarrRelease) string {
	if h := strings.TrimSpace(r.InfoHash); h != "" {
		return "infohash:" + strings.ToLower(h)
	}
	if g := strings.TrimSpace(r.GUID); g != "" {
		return "guid:" + digest(g)
	}
	size := ""
	if r.Size != nil {
		size = strconv.FormatInt(*r.Size, 10)
	}
	return "release:" + digest(strings.TrimSpace(r.Title)+"\x00"+size)
}

// prowlarrSource is what to hand a download client, in order of trust: the
// magnet, then the download URL. Both can carry a private-tracker passkey, so
// both are secret.Value. Empty means Prowlarr offered nothing fetchable — a real
// state a grab reports as the indexer's omission (#225).
func prowlarrSource(r prowlarrRelease) secret.Value {
	if m := strings.TrimSpace(r.MagnetURL); m != "" {
		return secret.Value(m)
	}
	if d := strings.TrimSpace(r.DownloadURL); d != "" {
		return secret.Value(d)
	}
	if g := strings.TrimSpace(r.GUID); strings.HasPrefix(g, "magnet:") {
		return secret.Value(g)
	}
	return ""
}

// prowlarrAttributes is everything the release actually asserted.
//
// SIZE and nothing else — the same honest state the Torznab client reaches
// (attributes.go): Prowlarr normalises size across its indexers, but does not
// assert a structured resolution/codec, and a title is a filename written by a
// stranger, not evidence. So a quality rule evaluates to `undetermined` here too,
// which is the truthful report rather than a guess.
func prowlarrAttributes(r prowlarrRelease) acquisition.Attributes {
	if r.Size == nil || *r.Size < 0 {
		return nil
	}
	return acquisition.Attributes{policy.AttrSizeBytes: policy.Num(*r.Size)}
}

// get performs one GET against a Prowlarr `/api/v1` route with the api key on the
// header, and returns the raw body.
func (c *prowlarrClient) get(ctx context.Context, path string, params url.Values, timeout time.Duration) ([]byte, error) {
	target := c.endpoint + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("prowlarr: building the request failed: %w", err)
	}
	// The credential goes on HERE, at the point the request is built, and nowhere
	// else — one line to find when asking where the key goes. A header, not a
	// query param, so it never reaches a URL that an error string might quote.
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("prowlarr: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, prowlarrMaxBody))
	if err != nil {
		return nil, fmt.Errorf("prowlarr: reading the response failed: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		// Named as configuration, not transience: a rejected key does not become
		// right by being asked again, and this is the failure that otherwise
		// presents as "searches return nothing".
		return nil, &prowlarrError{
			status: resp.StatusCode, configuration: true,
			detail: "the API key was rejected — a configuration problem, not a transient one",
		}
	case resp.StatusCode >= http.StatusInternalServerError:
		return nil, &prowlarrError{
			status: resp.StatusCode,
			detail: fmt.Sprintf("Prowlarr answered with a server error (HTTP %d)", resp.StatusCode),
		}
	case resp.StatusCode >= http.StatusBadRequest:
		return nil, &prowlarrError{
			status: resp.StatusCode,
			detail: fmt.Sprintf("Prowlarr refused the request (HTTP %d)", resp.StatusCode),
		}
	}
	return body, nil
}

// prowlarrError is a Prowlarr response that was reached but was not usable.
type prowlarrError struct {
	status        int
	configuration bool
	detail        string
}

func (e *prowlarrError) Error() string { return e.detail }

// detailFor renders an error as something an operator can act on, and never as
// something that contains a credential. The api key is on a header, never in the
// URL, so a transport error cannot quote it — but the classification still reads
// the error's TYPE, never its text, so a timeout, a refused connection and a name
// that will not resolve each get their own word.
func (c *prowlarrClient) detailFor(err error) string {
	var pe *prowlarrError
	if errors.As(err, &pe) {
		return pe.detail
	}
	return transporterr.Classify(err)
}
