package resources

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Pagination is keyset, never OFFSET.
//
// OFFSET pagination asks the database to count past N rows that match *now*.
// Insert a row before the offset between two pages and every later page shifts:
// one row is never returned and one is returned twice. That is not a rare race
// in this system, it is the normal case — a scan writes into a library while
// someone browses it. Keyset pagination asks for "the rows after this exact
// position" instead, so a concurrent insert cannot move the boundary.
//
// The cursor is opaque on purpose: it encodes the sort key of the last row of
// the previous page, and clients that learn to parse it are clients that break
// when the sort key changes.

// cursorVersion is bumped if the encoding changes, so an old cursor is rejected
// rather than silently misread as a position it is not.
const cursorVersion = 1

type cursorPayload struct {
	V int      `json:"v"`
	C string   `json:"c"`
	K []string `json:"k"`
}

// errBadCursor is what every malformed, truncated, foreign or stale cursor
// becomes. The client is told the cursor is unusable and nothing else: the
// contents are ours, and a decoding error message is a description of the
// encoding.
var errBadCursor = errors.New("cursor is not valid here")

// encodeCursor renders a position in a collection. The collection name is
// inside the cursor so that a cursor from /works handed to /jobs is rejected
// rather than interpreted as a position in a different sort order.
func encodeCursor(collection string, key ...string) string {
	buf, err := json.Marshal(cursorPayload{V: cursorVersion, C: collection, K: key})
	if err != nil {
		// cursorPayload is strings and an int; marshalling it cannot fail.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// decodeCursor returns the key parts of a cursor, checking that it belongs to
// this collection and has the arity this collection's sort key needs.
func decodeCursor(collection, raw string, arity int) ([]string, error) {
	buf, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, errBadCursor
	}
	var p cursorPayload
	if err := json.Unmarshal(buf, &p); err != nil {
		return nil, errBadCursor
	}
	if p.V != cursorVersion || p.C != collection || len(p.K) != arity {
		return nil, errBadCursor
	}
	return p.K, nil
}

// Limits. The maximum is a real bound rather than a suggestion: a client asking
// for a million rows is asking the server to hold a million rows in memory.
const (
	defaultLimit = 50
	maxLimit     = 200
)

// query holds the parsed collection parameters.
type query struct {
	limit  int
	cursor []string
	// filters are the values a collection understands, already validated.
	values map[string]string
}

// parseQuery reads limit, cursor and the named filters. Unknown filter values
// are a 400 rather than being ignored: silently returning everything when a
// client asks for state=finished is worse than an error, because the client
// believes the answer.
func parseQuery(r *http.Request, collection string, cursorArity int) (query, error) {
	q := query{limit: defaultLimit, values: map[string]string{}}
	raw := r.URL.Query()

	if v := raw.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return q, fmt.Errorf("limit must be a positive integer, not %q", v)
		}
		if n > maxLimit {
			n = maxLimit
		}
		q.limit = n
	}
	if v := raw.Get("cursor"); v != "" {
		key, err := decodeCursor(collection, v, cursorArity)
		if err != nil {
			return q, fmt.Errorf("the cursor is not usable on %s; start the collection again without one", collection)
		}
		q.cursor = key
	}
	return q, nil
}

// oneOf validates a filter against a closed set.
func oneOf(r *http.Request, name string, allowed ...string) (string, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return "", nil
	}
	for _, a := range allowed {
		if v == a {
			return v, nil
		}
	}
	return "", fmt.Errorf("%s must be one of %s, not %q", name, strings.Join(allowed, ", "), v)
}

// parseIntFilter reads an optional integer filter. Absent is nil; present and
// not an integer is a 400, for the reason oneOf gives — a client that asked
// for year=2O16 believes the unfiltered answer it would otherwise get.
func parseIntFilter(r *http.Request, name string) (*int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s must be an integer, not %q", name, v)
	}
	return &n, nil
}

// page is the envelope every collection returns.
//
// NextCursor is absent — not null, not empty-but-present — on the last page, so
// "keep going while next_cursor is set" is the whole client loop.
type page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// newPage trims the sentinel row and derives the cursor. Collections fetch
// limit+1 rows: whether more exist is then a fact about the result set rather
// than a second COUNT query racing the first.
func newPage[T any](rows []T, limit int, key func(T) []string, collection string) page[T] {
	items := rows
	next := ""
	if len(rows) > limit {
		items = rows[:limit]
		last := items[len(items)-1]
		next = encodeCursor(collection, key(last)...)
	}
	if items == nil {
		// An empty collection is [], never null. A JSON null here makes every
		// client's `for item of items` a null dereference.
		items = []T{}
	}
	return page[T]{Items: items, NextCursor: next}
}

// likePattern turns a user substring into a LIKE pattern, escaping the
// wildcards so that a search for "100%" is a search for "100%" rather than for
// everything.
func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// marshal renders a response body. HTML escaping is off: this is an API, not a
// document, and & in every title with an ampersand makes the golden files
// unreadable for no gain.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
