package client

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// Page is the envelope every collection returns. NextCursor is absent on the
// last page, so "keep going while next_cursor is set" is the whole loop.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// DefaultPageSize is what List asks for per request. It is the server's maximum
// (maxLimit in internal/api/resources), because the round trip dominates: 200
// rows of JSON is nothing, and a page size of 50 turns a 40 000-work library
// into 800 requests instead of 200.
const DefaultPageSize = 200

// ListOptions configure a collection walk.
type ListOptions struct {
	// Query carries the collection's filters. limit and cursor are set by List
	// and anything the caller put there under those names is overwritten.
	Query url.Values
	// Limit stops the walk early. Zero means every row.
	Limit int
	// PageSize is how many rows one request asks for. Zero means
	// DefaultPageSize. It exists so that pagination itself can be tested
	// against a small fixture rather than by seeding 201 rows.
	PageSize int
}

// List walks a collection to exhaustion, following the opaque cursor.
//
// This loop is the whole reason the CLI can be trusted on a real library. A
// `list` command that issues one request and prints what came back is not a
// listing — it is the first page, silently, and the difference only shows up
// when someone concludes that a work is missing because it sorted 51st.
//
// The cursor is opaque and stays opaque: it is echoed back exactly as received.
// Parsing it would couple the CLI to the server's sort key, which is precisely
// what the encoding exists to prevent.
func List[T any](ctx context.Context, c *Client, path string, opts ListOptions) ([]T, error) {
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = DefaultPageSize
	}

	out := []T{}
	cursor := ""
	seen := map[string]bool{}

	for {
		want := pageSize
		if opts.Limit > 0 {
			remaining := opts.Limit - len(out)
			if remaining <= 0 {
				return out, nil
			}
			if remaining < want {
				want = remaining
			}
		}

		q := url.Values{}
		for k, vs := range opts.Query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		q.Set("limit", strconv.Itoa(want))
		if cursor != "" {
			q.Set("cursor", cursor)
		}

		var page Page[T]
		if err := c.Get(ctx, path, q, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)

		if page.NextCursor == "" {
			return out, nil
		}
		if opts.Limit > 0 && len(out) >= opts.Limit {
			return out[:opts.Limit], nil
		}
		// A server that returns the same cursor twice would spin this loop
		// forever, accumulating rows until the process died. It should not
		// happen; a client that hangs when it does is not a good client.
		if seen[page.NextCursor] {
			return nil, fmt.Errorf("the server repeated a pagination cursor on %s after %d items — "+
				"stopping rather than looping", path, len(out))
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}
