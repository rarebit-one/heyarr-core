package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/api/problem"
)

// DecodeJSON reads exactly one JSON object from the request body into v,
// reading at most limit bytes.
//
// Unknown fields are rejected rather than ignored. A client that sends
// {"content_typo": "movie"} and gets a 201 has created a library with the wrong
// content type and been told it worked; the same request as a 400 is a typo
// caught in the terminal. The limit is the caller's because what is generous
// differs by route — a handful of short fields versus opaque ciphertext — and an
// unbounded json.Decode on a request body is a memory-exhaustion primitive that
// needs no authentication beyond a write token.
//
// The returned error's message is written for the client: callers answer it
// with problem.BadRequest(err.Error()).
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any, limit int64) error {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if mediaType, _, _ := strings.Cut(ct, ";"); strings.TrimSpace(mediaType) != "application/json" {
			return fmt.Errorf("the request body must be application/json, not %s", mediaType)
		}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return fmt.Errorf("the request body is larger than %d bytes", limit)
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

// WriteJSON renders a successful JSON response, encoded with json.Marshal.
// Errors are never written this way — those are problem documents, written by
// Fail.
func WriteJSON(w http.ResponseWriter, r *http.Request, log *slog.Logger, status int, body any) {
	WriteJSONWith(w, r, log, status, body, json.Marshal)
}

// WriteJSONWith is WriteJSON with the caller's encoder, for a surface whose
// bodies are not plain json.Marshal output (the resources API turns HTML
// escaping off). An encoding failure is logged and answered with a 500 problem
// document; nothing has been written by then, so the status is still ours to
// choose.
func WriteJSONWith(w http.ResponseWriter, r *http.Request, log *slog.Logger, status int, body any, marshal func(any) ([]byte, error)) {
	buf, err := marshal(body)
	if err != nil {
		log.Error("encoding a response failed",
			"request_id", RequestIDFrom(r.Context()), "path", r.URL.Path, "error", err)
		Fail(w, r, problem.Internal())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// #nosec G705 -- the body is JSON produced by encoding/json and served as
	// application/json with nosniff; there is no HTML context to escape into.
	_, _ = w.Write(buf)
}
