package resources

import (
	"fmt"
	"net/http"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// maxRequestBody bounds what a client may send to a write endpoint. Every body
// this API accepts is a handful of short fields, so a megabyte is already
// generous — and an unbounded json.Decode on a request body is a memory
// exhaustion primitive that needs no authentication beyond a write token.
const maxRequestBody = 1 << 20

// decodeOr400 decodes the request body into v with httpapi.DecodeJSON's
// strictness and this API's size bound. When it cannot, it has already answered
// with a 400 naming what was wrong, and the handler returns.
func decodeOr400(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := httpapi.DecodeJSON(w, r, v, maxRequestBody); err != nil {
		httpapi.Fail(w, r, problem.BadRequest(err.Error()))
		return false
	}
	return true
}

// required checks a mandatory string field.
func required(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

// inSet checks a field against a closed set, defaulting when empty.
func inSet(name, value, fallback string, allowed ...string) (string, error) {
	if value == "" {
		return fallback, nil
	}
	for _, a := range allowed {
		if value == a {
			return value, nil
		}
	}
	return "", fmt.Errorf("%s must be one of %s, not %q", name, strings.Join(allowed, ", "), value)
}
