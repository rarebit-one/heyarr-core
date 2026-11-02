package tmdb

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/enrichmatch"
	"github.com/rarebit-one/heyarr-core/internal/providers/httpjson"
)

// The enrich half of the TMDB adapter (ADR-0122; ADR-0087's named revisit: "a
// movie/series poster from TMDB via the same beat"). A held film or series was
// identified from its filename at ingest — a title and usually a year — and
// nothing has ever asked an authority what it is. This answers that question
// keyed on the Work, the way MusicBrainz does for an album: the canonical ids,
// the overview and genres, and the poster AND backdrop the enrich job stores on
// the node, so every client is served the same artwork from the catalogue rather
// than each looking it up and caching bytes of its own.
//
// # A strong key first, a validated title search second
//
// A Work that already carries a tmdb id (wanted from discovery, or enriched
// before) is fetched by that id. One carrying an imdb id, or a series carrying
// the tvdb id its follow recorded (ADR-0058), is resolved through TMDB's /find
// — one call, no guessing. Only a Work with no id at all is title-searched, and
// a search hit is never trusted on rank alone: TMDB's /search/movie answers
// SOMETHING for almost any string, so the hit must share the query's words
// (enrichmatch.TokenSetRatio, the scale every enrich adapter scores on) and,
// when both sides know a year, agree on it to within a year. A hit that fails
// either test is the modelled "nothing matched", which the worker backs off on
// — a wrong poster on a film is a visible error, a bare one is not.
//
// # It decorates a video Work; it never renames it
//
// EnrichResult.Title and Author are left EMPTY for a film or series — "no
// correction offered" in the worker's vocabulary — so ADR-0088's display-identity
// correction never runs on video. A film's ingest identity comes from a
// scene-name parse that is usually right, a person can correct it by hand
// (PATCH /works), and there is no override marker on a title the way there is on
// a music artist; the honest stance is additive only (ADR-0087 §4), with the
// authority's title and year kept as provenance in the Work's attributes.

// videoKind is TMDB's own path segment for a content kind: "movie" for a film,
// "tv" for a series. It is how the one Enrich body serves both without a
// branch per endpoint.
type videoKind string

const (
	kindMovie videoKind = "movie"
	kindTV    videoKind = "tv"
)

// videoKindFor maps a held Work's content type to TMDB's kind. ok is false for a
// content type this adapter does not serve (a book, an album).
func videoKindFor(contentType string) (videoKind, bool) {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "movie":
		return kindMovie, true
	case "series":
		return kindTV, true
	}
	return "", false
}

// enrichMinMatch is the token-set overlap a title search hit must reach to be
// accepted at all. 0.75 — three of every four words of the shorter title present
// in the other — is the same scale the worker's identity-correction threshold is
// calibrated on (ADR-0088), so "confident enough to decorate" here means the
// same thing "confident enough to rename" means for a book.
const enrichMinMatch = 0.75

// yearSlipPenalty is taken off a hit whose year is ONE off the Work's. A release
// that premiered in December and went wide in January, or a series whose first
// air date straddles a new year, is the ordinary cause and not a mismatch; two
// or more years apart is a different film and the hit is refused outright.
const yearSlipPenalty = 0.1

// Artwork sizes requested from TMDB's image CDN for STORAGE, as opposed to the
// w500/w1280 discovery hands a client to render straight from the service.
// Stored bytes are the source every thumbnail bucket derives from (ADR-0109),
// so the poster is a generous w780 rather than the full original (which runs to
// 2000×3000 and several MB for a card that is never shown that large); the
// backdrop keeps w1280, a detail hero's natural size.
const (
	posterSize   = "w780"
	backdropSize = "w1280"
)

// ServesContentType reports that this adapter enriches films and series.
// Books are Open Library's and albums MusicBrainz's (ADR-0087).
func (c *Client) ServesContentType(ct string) bool {
	_, ok := videoKindFor(ct)
	return ok
}

// Enrich looks a held film or series up and returns what TMDB knows about it
// (providers.EnrichProvider). The bool is false with a nil error when nothing
// acceptable matched — the Work is unknown to TMDB, or every hit failed the
// title/year validation — which the worker backs off on rather than treating as
// an error. An error is a call that could not be made.
func (c *Client) Enrich(ctx context.Context, q providers.EnrichQuery) (providers.EnrichResult, bool, error) {
	kind, ok := videoKindFor(q.ContentType)
	if !ok {
		return providers.EnrichResult{}, false, nil
	}

	id, confidence, found, err := c.resolveVideo(ctx, kind, q)
	if err != nil || !found {
		return providers.EnrichResult{}, false, err
	}

	detail, found, err := c.videoDetail(ctx, kind, id)
	if err != nil || !found {
		return providers.EnrichResult{}, false, err
	}

	ids := map[string]string{"tmdb": strconv.FormatInt(detail.ID, 10)}
	if imdb := strings.TrimSpace(firstNonEmpty(detail.ExternalIDs.IMDBID, detail.IMDBID)); imdb != "" {
		ids["imdb"] = imdb
	}
	if kind == kindTV && detail.ExternalIDs.TVDBID != nil && *detail.ExternalIDs.TVDBID > 0 {
		ids["tvdb"] = strconv.FormatInt(*detail.ExternalIDs.TVDBID, 10)
	}

	genres := make([]string, 0, len(detail.Genres))
	for _, g := range detail.Genres {
		if name := strings.TrimSpace(g.Name); name != "" {
			genres = append(genres, name)
		}
	}

	return providers.EnrichResult{
		ExternalIDs: ids,
		CoverURL:    secret.Value(imageURL(detail.PosterPath, posterSize)),
		BackdropURL: secret.Value(imageURL(detail.BackdropPath, backdropSize)),
		Overview:    strings.TrimSpace(detail.Overview),
		Genres:      genres,
		Year:        parseYearFromDate(detail.date(kind)),
		// Title and Author deliberately empty: a video Work is decorated, never
		// renamed (see the file comment).
		Confidence: confidence,
	}, true, nil
}

// resolveVideo turns the Work into a TMDB id: by a recorded tmdb id, by /find on
// a recorded imdb or tvdb id, or by a validated title search, in that order of
// trust. confidence is 1 for a keyed resolution — the id IS the match — and the
// validated score for a search.
func (c *Client) resolveVideo(ctx context.Context, kind videoKind, q providers.EnrichQuery) (id string, confidence float64, found bool, err error) {
	if tmdb := strings.TrimSpace(q.ExternalIDs["tmdb"]); tmdb != "" {
		return tmdb, 1, true, nil
	}
	if imdb := strings.TrimSpace(q.ExternalIDs["imdb"]); imdb != "" {
		id, found, err := c.find(ctx, kind, imdb, "imdb_id")
		if err != nil || found {
			return id, 1, found, err
		}
	}
	if kind == kindTV {
		if tvdb := strings.TrimSpace(q.ExternalIDs["tvdb"]); tvdb != "" {
			id, found, err := c.find(ctx, kind, tvdb, "tvdb_id")
			if err != nil || found {
				return id, 1, found, err
			}
		}
	}
	return c.searchVideo(ctx, kind, strings.TrimSpace(q.Title), q.Year)
}

// find resolves an external id through TMDB's /find, which answers with the
// matching movies and series in separate lists; only the list for the Work's
// kind is read, so a movie's imdb id never resolves to a same-named series.
func (c *Client) find(ctx context.Context, kind videoKind, externalID, source string) (string, bool, error) {
	path := fmt.Sprintf("%s/find/%s?external_source=%s", c.endpoint, pathSeg(externalID), source)
	var body findResponse
	if err := c.get(ctx, path, "find", &body); err != nil {
		if isNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	var hits []videoHit
	if kind == kindMovie {
		hits = body.MovieResults
	} else {
		hits = body.TVResults
	}
	for _, h := range hits {
		if h.ID != 0 {
			return strconv.FormatInt(h.ID, 10), true, nil
		}
	}
	return "", false, nil
}

// searchVideo title-searches TMDB and returns the best hit that passes
// validation, or found=false when none does. Hits are scored by how much of the
// shorter title the other contains (TokenSetRatio), a one-year slip costs a
// little and a larger gap refuses the hit outright; ties — "Alien" against
// "Alien" and "Alien: Covenant" when the Work has no year — go to the hit with
// the fewest words the query never had (Jaccard), then to TMDB's own order.
func (c *Client) searchVideo(ctx context.Context, kind videoKind, title string, year int) (string, float64, bool, error) {
	if title == "" {
		return "", 0, false, nil
	}
	path := fmt.Sprintf("%s/search/%s?query=%s&page=1", c.endpoint, kind, queryEscape(title))
	var body videoSearchResponse
	if err := c.get(ctx, path, "search-"+string(kind), &body); err != nil {
		return "", 0, false, err
	}

	bestID, bestScore, bestTie := "", 0.0, -1.0
	for _, hit := range body.Results {
		if hit.ID == 0 {
			continue
		}
		score, ok := scoreHit(title, year, hit.title(kind), hit.originalTitle(kind), parseYearFromDate(hit.date(kind)))
		if !ok {
			continue
		}
		tie := enrichmatch.Jaccard(title, hit.title(kind))
		if score > bestScore || (score == bestScore && tie > bestTie) {
			bestID, bestScore, bestTie = strconv.FormatInt(hit.ID, 10), score, tie
		}
	}
	if bestID == "" {
		return "", 0, false, nil
	}
	return bestID, bestScore, true, nil
}

// scoreHit validates one search hit against the Work and returns its match
// strength. ok is false when the hit is refused: too few shared words, or a year
// more than one apart when both are known. A Work or hit with no year is not
// constrained on year — zero means "did not say", not the year 0.
func scoreHit(queryTitle string, queryYear int, title, originalTitle string, hitYear int) (float64, bool) {
	score := enrichmatch.TokenSetRatio(queryTitle, title)
	if alt := enrichmatch.TokenSetRatio(queryTitle, originalTitle); alt > score {
		score = alt
	}
	if queryYear != 0 && hitYear != 0 {
		gap := queryYear - hitYear
		if gap < 0 {
			gap = -gap
		}
		switch gap {
		case 0:
		case 1:
			score -= yearSlipPenalty
		default:
			return 0, false
		}
	}
	if score < enrichMinMatch {
		return score, false
	}
	return score, true
}

// videoDetail fetches /movie/{id} or /tv/{id} with the external ids appended in
// the same call (append_to_response), so a match costs one request for its
// overview, genres, artwork paths and imdb/tvdb ids together. A 404 is the
// modelled "no such title" — a recorded id that no longer resolves — not a
// transport error.
func (c *Client) videoDetail(ctx context.Context, kind videoKind, id string) (videoDetail, bool, error) {
	path := fmt.Sprintf("%s/%s/%s?append_to_response=external_ids", c.endpoint, kind, pathSeg(id))
	var detail videoDetail
	if err := c.get(ctx, path, string(kind), &detail); err != nil {
		if isNotFound(err) {
			return videoDetail{}, false, nil
		}
		return videoDetail{}, false, err
	}
	if detail.ID == 0 {
		return videoDetail{}, false, nil
	}
	return detail, true, nil
}

// isNotFound reports whether a call failed because TMDB has no such resource.
func isNotFound(err error) bool {
	var he *httpjson.Error
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// The wire shapes for the enrich calls, kept unexported like the rest. A movie
// and a series name the same facts differently (title/name, release_date/
// first_air_date, original_title/original_name), so one struct carries both
// spellings and the kind-aware accessors read the right one — the alternative,
// two parallel structs and two parallel code paths, is how a field gets read on
// one side and silently stays empty on the other.

// videoSearchResponse is /search/movie's or /search/tv's body.
type videoSearchResponse struct {
	Results []videoHit `json:"results"`
}

// videoHit is one search or /find hit.
type videoHit struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	Name          string `json:"name"`
	OriginalTitle string `json:"original_title"`
	OriginalName  string `json:"original_name"`
	ReleaseDate   string `json:"release_date"`
	FirstAirDate  string `json:"first_air_date"`
}

func (h videoHit) title(kind videoKind) string {
	if kind == kindMovie {
		return h.Title
	}
	return h.Name
}

func (h videoHit) originalTitle(kind videoKind) string {
	if kind == kindMovie {
		return h.OriginalTitle
	}
	return h.OriginalName
}

func (h videoHit) date(kind videoKind) string {
	if kind == kindMovie {
		return h.ReleaseDate
	}
	return h.FirstAirDate
}

// findResponse is /find/{id}'s body: the matches, split by what they are.
type findResponse struct {
	MovieResults []videoHit `json:"movie_results"`
	TVResults    []videoHit `json:"tv_results"`
}

// videoDetail is /movie/{id}'s or /tv/{id}'s body with external_ids appended.
// A movie carries imdb_id at the top level as well as under external_ids; a
// series only under external_ids, beside tvdb_id (a NUMBER, or null).
type videoDetail struct {
	ID           int64         `json:"id"`
	Title        string        `json:"title"`
	Name         string        `json:"name"`
	ReleaseDate  string        `json:"release_date"`
	FirstAirDate string        `json:"first_air_date"`
	Overview     string        `json:"overview"`
	PosterPath   string        `json:"poster_path"`
	BackdropPath string        `json:"backdrop_path"`
	Genres       []genre       `json:"genres"`
	IMDBID       string        `json:"imdb_id"`
	ExternalIDs  videoExternal `json:"external_ids"`
}

func (d videoDetail) date(kind videoKind) string {
	if kind == kindMovie {
		return d.ReleaseDate
	}
	return d.FirstAirDate
}

type genre struct {
	Name string `json:"name"`
}

type videoExternal struct {
	IMDBID string `json:"imdb_id"`
	TVDBID *int64 `json:"tvdb_id"`
}
