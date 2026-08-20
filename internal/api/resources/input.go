package resources

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxRequestBody bounds what a client may send to a write endpoint. Every body
// this API accepts is a handful of short fields, so a megabyte is already
// generous — and an unbounded json.Decode on a request body is a memory
// exhaustion primitive that needs no authentication beyond a write token.
const maxRequestBody = 1 << 20

// decodeJSON reads exactly one JSON object into v.
//
// Unknown fields are rejected rather than ignored. A client that sends
// {"content_typo": "movie"} and gets a 201 has created a library with the wrong
// content type and been told it worked; the same request as a 400 is a typo
// caught in the terminal.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mediaType, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mediaType) != "application/json" {
			return fmt.Errorf("the request body must be application/json, not %s", mediaType)
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return fmt.Errorf("the request body is larger than %d bytes", maxRequestBody)
		}
		if errors.Is(err, io.EOF) {
			return errors.New("the request body is empty")
		}
		// The decoder's message names the offending field, which is the useful
		// part, and never contains anything the client did not send.
		return fmt.Errorf("the request body is not valid JSON: %w", err)
	}
	// A second document in the same body means the client sent something other
	// than what it thinks it sent.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("the request body must contain exactly one JSON object")
	}
	return nil
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
