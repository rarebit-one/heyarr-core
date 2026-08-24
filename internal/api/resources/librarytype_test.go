//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"net/http"
	"strings"
	"testing"
)

// A library cannot be created with a content type Heyarr does not know.
//
// `content_type` is TEXT with no CHECK, and the create path required it to be
// non-empty and nothing more — so any string got through, and `show` did.
//
// The consequence is not cosmetic. Identify uses the library's type to choose
// which rules may fire; a type no rule declares matches nothing, falls back to
// every rule in registration order, and the movie rules are first. A television
// library declared `show` had its artwork read by `movie/title-year` and grew a
// movie Work that does not exist (#227).
func TestALibraryCannotBeCreatedWithATypeHeyarrDoesNotKnow(t *testing.T) {
	h := newHarness(t).seed()

	for _, ct := range []string{"show", "album", "tv", "film", "Movie", "unknown"} {
		t.Run(ct, func(t *testing.T) {
			resp := h.doStable(http.MethodPost, "/api/v1/libraries",
				strings.NewReader(`{"name":"lib-`+ct+`","content_type":"`+ct+`"}`))
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d for content_type %q, want 400: %s",
					resp.StatusCode, ct, h.body(resp))
			}
			// The refusal names the vocabulary, or the operator has to guess
			// what Heyarr wanted — and guessing is how `show` happened.
			body := string(h.body(resp))
			for _, want := range []string{"movie", "series", "music", "book"} {
				if !strings.Contains(body, want) {
					t.Errorf("the refusal does not offer %q: %s", want, body)
				}
			}
		})
	}
}

// And the four it does know are accepted, or the guard is just an outage.
func TestTheFourContentTypesAreStillAccepted(t *testing.T) {
	h := newHarness(t).seed()

	for _, ct := range []string{"movie", "series", "music", "book"} {
		t.Run(ct, func(t *testing.T) {
			resp := h.doStable(http.MethodPost, "/api/v1/libraries",
				strings.NewReader(`{"name":"ok-`+ct+`","content_type":"`+ct+`"}`))
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("status = %d for content_type %q, want 201: %s",
					resp.StatusCode, ct, h.body(resp))
			}
		})
	}
}
