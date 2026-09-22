package httpapi

import (
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
)

// HashParam reads the {hash} path parameter and percent-decodes it.
//
// A content-address is `blake3:<hex>` and the ':' is a reserved character
// (RFC 3986 gen-delim). Go's net/http client leaves it literal in the request
// target, but a spec-conformant client — notably the JVM's
// java.net.http.HttpClient, which the desktop vault-sync daemon uses — encodes
// it to %3A. chi routes on the RAW target, so chi.URLParam would hand a
// downstream hashing.Parse a colon-less "blake3%3A…" and it would 400 the
// request as a malformed id ("has no algorithm prefix").
//
// Decoding the segment here makes the server accept both spellings — %3A and a
// literal ':' are equivalent (RFC 3986 §2.1) — so any conformant client
// interoperates. A literal ':' decodes to itself, so the Go CLI and peer
// clients are unaffected; only the previously-rejected encoded form changes.
func HashParam(r *http.Request) string {
	raw := chi.URLParam(r, "hash")
	if dec, err := url.PathUnescape(raw); err == nil {
		return dec
	}
	return raw
}
