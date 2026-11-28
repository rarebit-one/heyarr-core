package musicbrainz

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// RecordingCandidate is provider-neutral so callers need not depend on this adapter.
type RecordingCandidate = providers.RecordingCandidate

// RecordingArtist is one credited artist in a recording result.
type RecordingArtist = providers.RecordingArtist

// RecordingRelease is a release associated with a recording result.
type RecordingRelease = providers.RecordingRelease

const maxRecordingResults = 20

// SearchRecordings asks MusicBrainz for recordings matching both a title and an
// artist. It returns candidates, not an automatic identity decision. A search
// result's score is MusicBrainz relevance, not the probability of an audio match.
func (c *Client) SearchRecordings(ctx context.Context, artist, title string) ([]RecordingCandidate, error) {
	artist, title = strings.TrimSpace(artist), strings.TrimSpace(title)
	if artist == "" || title == "" {
		return nil, errors.New("musicbrainz: recording search needs an artist and title")
	}
	query := fmt.Sprintf(`recording:"%s" AND artist:"%s"`, lucenePhrase(title), lucenePhrase(artist))
	path := fmt.Sprintf("%s/recording/?query=%s&fmt=json&limit=%d", c.endpoint, url.QueryEscape(query), maxRecordingResults)
	var body recordingSearchResponse
	if err := c.get(ctx, path, "recording search", &body); err != nil {
		return nil, err
	}
	out := make([]RecordingCandidate, 0, min(len(body.Recordings), maxRecordingResults))
	for _, rec := range body.Recordings {
		if candidate, ok := rec.candidate(); ok {
			out = append(out, candidate)
			if len(out) == maxRecordingResults {
				break
			}
		}
	}
	return out, nil
}

// LookupRecording retrieves the canonical metadata of a recording by its
// MusicBrainz UUID. The caller must still compare it to the requested intent.
func (c *Client) LookupRecording(ctx context.Context, id string) (RecordingCandidate, error) {
	id = strings.TrimSpace(id)
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != strings.ToLower(id) {
		return RecordingCandidate{}, errors.New("musicbrainz: recording ID must be a canonical UUID")
	}
	var body recordingResult
	path := fmt.Sprintf("%s/recording/%s?fmt=json&inc=artist-credits+releases", c.endpoint, parsed.String())
	if err := c.get(ctx, path, "recording lookup", &body); err != nil {
		return RecordingCandidate{}, err
	}
	candidate, ok := body.candidate()
	if !ok || candidate.ID != parsed.String() {
		return RecordingCandidate{}, errors.New("musicbrainz: recording lookup returned invalid or mismatched identity")
	}
	return candidate, nil
}

// Quote both Lucene's quote and escape characters within an exact phrase.
func lucenePhrase(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

type recordingSearchResponse struct {
	Recordings []recordingResult `json:"recordings"`
}

type recordingResult struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Disambiguation string `json:"disambiguation"`
	Length         int64  `json:"length"`
	Score          int    `json:"score"`
	ArtistCredit   []struct {
		Name       string `json:"name"`
		JoinPhrase string `json:"joinphrase"`
		Artist     struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"artist"`
	} `json:"artist-credit"`
	Releases []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Date  string `json:"date"`
	} `json:"releases"`
}

func (r recordingResult) candidate() (RecordingCandidate, bool) {
	id, err := uuid.Parse(strings.TrimSpace(r.ID))
	if err != nil || strings.TrimSpace(r.Title) == "" {
		return RecordingCandidate{}, false
	}
	out := RecordingCandidate{ID: id.String(), Title: strings.TrimSpace(r.Title), Disambiguation: strings.TrimSpace(r.Disambiguation), DurationMS: r.Length, Score: r.Score}
	for _, a := range r.ArtistCredit {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = strings.TrimSpace(a.Artist.Name)
		}
		if name != "" {
			out.Artists = append(out.Artists, RecordingArtist{ID: strings.TrimSpace(a.Artist.ID), Name: name, JoinPhrase: a.JoinPhrase})
		}
	}
	// A search response is bounded by maxBodyBytes; also bound the returned
	// per-recording release list so callers cannot accumulate unbounded entries.
	for _, rel := range r.Releases {
		if len(out.Releases) == maxRecordingResults {
			break
		}
		if strings.TrimSpace(rel.ID) != "" && strings.TrimSpace(rel.Title) != "" {
			out.Releases = append(out.Releases, RecordingRelease{ID: strings.TrimSpace(rel.ID), Title: strings.TrimSpace(rel.Title), Date: strings.TrimSpace(rel.Date)})
		}
	}
	return out, true
}
