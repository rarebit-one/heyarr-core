package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/hashing"
)

// Blob bytes are ADR-0013's separate contract: a plain, honest byte range over
// an immutable object, shared by playback, remote probing, replication and the
// web seed. The client half of that contract is here.
//
// The resume idiom is `Range: bytes=<offset>-` plus `If-Range: "blake3-<hex>"`.
// The validator does not have to be remembered from a previous response,
// because it is derived from the digest — which is the identity in the URL. So
// a resumed download needs no state file: the bytes already on disk say where
// to start, and the name says what they must be.
//
// A 206 means the server honoured the range and the bytes continue from the
// offset. A 200 means it did not: either the object is not what it was, or an
// intermediary stripped the range. Either way the only correct response is to
// start over, because appending a whole object to a partial one produces a file
// that is the right length for nothing and hashes to garbage.

// BlobContent is an open byte stream.
type BlobContent struct {
	// Body is the bytes. The caller closes it.
	Body io.ReadCloser
	// Offset is where Body starts within the blob.
	Offset int64
	// Total is the blob's full length, or -1 when the server did not say.
	Total int64
	// Resumed reports whether the requested range was honoured (206). When a
	// range was requested and this is false, whatever is already on disk must
	// be discarded.
	Resumed bool
	// ETag is the strong validator the server returned.
	ETag string
}

// ETagFor renders the validator for a hash. It is `"blake3-<hex>"`, derived
// from the digest rather than from the file, so every peer holding these bytes
// advertises the same one.
func ETagFor(h hashing.Hash) string { return `"blake3-` + h.Hex() + `"` }

// StatBlob reads a blob's catalog row.
//
// A malformed identifier is refused here rather than sent. The two mistakes are
// different — "that is not a hash" and "that is a hash and this peer does not
// have it" — and the metadata route answers 404 to both, because a hash that is
// not a hash simply matches no row. Parsing first keeps the difference, and it
// also means nothing unvalidated is turned into a URL path.
func (c *Client) StatBlob(ctx context.Context, hash string) (Blob, error) {
	parsed, err := hashing.Parse(hash)
	if err != nil {
		return Blob{}, fmt.Errorf(
			"%q is not a blob identifier — it must be blake3:<64 lowercase hex characters>", hash)
	}
	var b Blob
	err = c.Get(ctx, "/blobs/"+parsed.String(), nil, &b)
	return b, err
}

// OpenBlobContent opens a blob's bytes, resuming from offset when it is
// non-zero.
func (c *Client) OpenBlobContent(ctx context.Context, hash string, offset int64) (*BlobContent, error) {
	parsed, err := hashing.Parse(hash)
	if err != nil {
		// Refused here rather than sent: nothing unvalidated should be turned
		// into a URL path, and the server would only tell us the same thing
		// after a round trip.
		return nil, fmt.Errorf("%q is not a blob identifier — it must be blake3:<64 lowercase hex characters>", hash)
	}
	req, err := c.newRequest(ctx, http.MethodGet, "/blobs/"+parsed.String()+"/content", nil, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
		req.Header.Set("If-Range", ETagFor(parsed))
	}

	resp, err := c.do(c.stream, req)
	if err != nil {
		return nil, err
	}

	out := &BlobContent{
		Body:  resp.Body,
		Total: -1,
		ETag:  resp.Header.Get("ETag"),
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("the server answered 206 with an unreadable Content-Range %q",
				resp.Header.Get("Content-Range"))
		}
		out.Offset, out.Total, out.Resumed = start, total, true
	default:
		// 200. Whatever the caller already has is not a prefix of this.
		out.Offset, out.Resumed = 0, false
		if n := resp.Header.Get("Content-Length"); n != "" {
			if parsedLen, err := strconv.ParseInt(n, 10, 64); err == nil {
				out.Total = parsedLen
			}
		}
	}
	return out, nil
}

// parseContentRange reads `bytes <start>-<end>/<total>`.
func parseContentRange(v string) (start, total int64, ok bool) {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "bytes"))
	slash := strings.LastIndex(v, "/")
	if slash < 0 {
		return 0, 0, false
	}
	span, sizePart := strings.TrimSpace(v[:slash]), strings.TrimSpace(v[slash+1:])
	dash := strings.Index(span, "-")
	if dash < 0 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(span[:dash]), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	total = -1
	if sizePart != "*" {
		total, err = strconv.ParseInt(sizePart, 10, 64)
		if err != nil {
			return 0, 0, false
		}
	}
	return start, total, true
}

// VerifyResult is what `heyarr blobs verify` learned.
type VerifyResult struct {
	Hash string `json:"hash"`
	// Size is what the catalog says the blob is.
	Size int64 `json:"size"`
	// BytesRead is what the peer actually served.
	BytesRead int64 `json:"bytes_read"`
	// ActualHash is what those bytes hash to. It is reported even when it
	// matches, because a verification that prints nothing on success is one
	// nobody believes on failure.
	ActualHash string `json:"actual_hash"`
	Verified   bool   `json:"verified"`
	// Detail says what is wrong, and is empty when nothing is.
	Detail string `json:"detail,omitempty"`
}

// VerifyBlob downloads a blob and hashes it.
//
// The verification is done here, on the bytes as received, rather than by
// asking the server whether they are fine. That is invariant 1: a destination
// always verifies bytes itself and never trusts a claimed hash. Asking the
// server would confirm only that the server's catalog agrees with itself.
func (c *Client) VerifyBlob(ctx context.Context, hash string) (VerifyResult, error) {
	parsed, err := hashing.Parse(hash)
	if err != nil {
		return VerifyResult{}, fmt.Errorf(
			"%q is not a blob identifier — it must be blake3:<64 lowercase hex characters>", hash)
	}
	meta, err := c.StatBlob(ctx, parsed.String())
	if err != nil {
		return VerifyResult{}, err
	}

	content, err := c.OpenBlobContent(ctx, parsed.String(), 0)
	if err != nil {
		return VerifyResult{}, err
	}
	defer func() { _ = content.Body.Close() }()

	hasher := hashing.New()
	read, err := io.Copy(hasher, content.Body)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("reading the bytes of %s: %w", parsed, err)
	}
	actual := hasher.Sum()

	out := VerifyResult{
		Hash:       parsed.String(),
		Size:       meta.Size,
		BytesRead:  read,
		ActualHash: actual.String(),
		Verified:   actual.Equal(parsed) && read == meta.Size,
	}
	switch {
	case !actual.Equal(parsed):
		out.Detail = "the bytes served do not hash to their own name"
	case read != meta.Size:
		out.Detail = fmt.Sprintf("the catalog records %d bytes and the peer served %d", meta.Size, read)
	}
	return out, nil
}
