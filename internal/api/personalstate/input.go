package personalstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxRequestBody bounds a push. A wrapped key is ~100 bytes and a change's
// ciphertext is a small CRDT delta, so a few megabytes is already generous, and
// an unbounded decode on a request body is a memory-exhaustion primitive a write
// token should not hand out. It is larger than resources' 1 MiB because a change
// carries opaque ciphertext rather than a handful of short fields.
const maxRequestBody = 4 << 20

// decodeJSON reads exactly one JSON object into v, rejecting unknown fields and a
// trailing second document — the same strictness internal/api/resources uses, for
// the same reason: a client that mistypes a field and gets a 201 has stored the
// wrong thing and been told it worked.
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
		return fmt.Errorf("the request body is not valid JSON: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("the request body must contain exactly one JSON object")
	}
	return nil
}
