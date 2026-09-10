package downloads

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The RPC methods, and the wire shapes they decode into.
//
// The shapes are declared here rather than reused from anywhere: they are
// Transmission's, they change when Transmission changes, and a type shared with
// the domain would make a protocol detail a domain concern.

// torrent is one entry from torrent-get.
//
// The field list is exactly what the captured corpus contains, which is exactly
// what the capture script asks for. Adding a field here without adding it there
// produces a zero value that reads as a fact.
type torrent struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	HashString  string   `json:"hashString"`
	Status      int      `json:"status"`
	PercentDone float64  `json:"percentDone"`
	DownloadDir string   `json:"downloadDir"`
	Labels      []string `json:"labels"`
	Error       int      `json:"error"`
	ErrorString string   `json:"errorString"`
	IsFinished  bool     `json:"isFinished"`
	ETA         int64    `json:"eta"`
	TotalSize   int64    `json:"totalSize"`
	// TrackerStats is where a tracker failure ACTUALLY appears. See stall.go —
	// this field is the whole reason that file exists.
	TrackerStats []trackerStat `json:"trackerStats"`
}

type trackerStat struct {
	Announce              string `json:"announce"`
	HasAnnounced          bool   `json:"hasAnnounced"`
	LastAnnounceResult    string `json:"lastAnnounceResult"`
	LastAnnounceSucceeded bool   `json:"lastAnnounceSucceeded"`
}

// torrentFields is what torrent-get is asked for.
//
// Explicit rather than "everything": Transmission's full torrent object is
// large, most of it is peers and pieces, and asking for it over a queue of a
// few thousand is megabytes per poll for fields nothing reads.
var torrentFields = []string{
	"id", "name", "hashString", "status", "percentDone", "downloadDir",
	"labels", "error", "errorString", "trackerStats", "isFinished", "eta", "totalSize",
}

// Transmission's status codes. Only the ones that mean something to §64's
// pipeline are named; the rest are checking and queue states that all mean
// "in flight" for our purposes.
const (
	statusStopped = 0
	statusSeeding = 6
)

// sessionGet reads the instance's own description of itself.
func (c *Client) sessionGet(ctx context.Context) (sessionInfo, error) {
	var raw struct {
		Version              string `json:"version"`
		RPCVersion           int    `json:"rpc-version"`
		RPCVersionMinimum    int    `json:"rpc-version-minimum"`
		DownloadDir          string `json:"download-dir"`
		IncompleteDir        string `json:"incomplete-dir"`
		IncompleteDirEnabled bool   `json:"incomplete-dir-enabled"`
	}
	if err := c.rpc.call(ctx, "session-get", nil, &raw); err != nil {
		return sessionInfo{}, err
	}
	return sessionInfo{
		Version:              raw.Version,
		RPCVersion:           raw.RPCVersion,
		RPCVersionMinimum:    raw.RPCVersionMinimum,
		DownloadDir:          raw.DownloadDir,
		IncompleteDir:        raw.IncompleteDir,
		IncompleteDirEnabled: raw.IncompleteDirEnabled,
		Known:                true,
	}, nil
}

// Transfers is everything this client is doing that BELONGS TO HEYARR.
//
// The filter is the point. See Label: a download client is shared, and a
// transfer without our tag is invisible to everything above this line — not
// merely excluded from mutation, but absent from the list entirely, so that no
// caller can accidentally act on one it was never shown.
func (c *Client) Transfers(ctx context.Context) ([]providers.Transfer, error) {
	torrents, err := c.ours(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]providers.Transfer, 0, len(torrents))
	for _, t := range torrents {
		out = append(out, c.toTransfer(t))
	}
	return out, nil
}

// ours reads the queue and keeps only Heyarr's transfers.
func (c *Client) ours(ctx context.Context) ([]torrent, error) {
	var raw struct {
		Torrents []torrent `json:"torrents"`
	}
	args := map[string]any{"fields": torrentFields}
	if err := c.rpc.call(ctx, "torrent-get", args, &raw); err != nil {
		return nil, err
	}

	out := make([]torrent, 0, len(raw.Torrents))
	for _, t := range raw.Torrents {
		if c.isOurs(t) {
			out = append(out, t)
		}
	}
	return out, nil
}

// isOurs reports whether Heyarr queued this transfer.
//
// Two mechanisms, matching the two ways Heyarr can have tagged it:
//
//   - a LABEL, on any instance at RPC 16 or above;
//   - a download SUBDIRECTORY, the fallback for older ones.
//
// Both are checked regardless of what this instance currently supports,
// because an instance can be upgraded between the transfer being queued and it
// being read — and a transfer that became unrecognisable across a Transmission
// upgrade would be one Heyarr abandons while it continues to consume disk.
func (c *Client) isOurs(t torrent) bool {
	for _, l := range t.Labels {
		if strings.EqualFold(strings.TrimSpace(l), c.label) {
			return true
		}
	}
	// The fallback: our transfers land in <download-dir>/<label>.
	dir := path.Clean(t.DownloadDir)
	return path.Base(dir) == c.label
}

// toTransfer reduces Transmission's shape to the registry's value type.
func (c *Client) toTransfer(t torrent) providers.Transfer {
	done := t.PercentDone >= 1 || t.IsFinished || t.Status == statusSeeding

	out := providers.Transfer{
		// The INFOHASH, never the name. Names get renamed by the client,
		// collide between releases and are not stable across a restart. This
		// is the same conclusion invariant 1 reaches for bytes, one level up:
		// identity is a hash, not a label a human reads.
		ID:         t.HashString,
		Name:       t.Name,
		Done:       done,
		BytesTotal: t.TotalSize,
		BytesDone:  int64(float64(t.TotalSize) * t.PercentDone),
	}

	if trouble, bad := inspect(t); bad {
		// This is where the invisible tracker failure becomes visible to
		// everything above. Without stall.go it would arrive here as an empty
		// string and the transfer would look healthy forever.
		out.Error = string(trouble.Reason) + ": " + trouble.Detail
	}

	// The path is resolved ONLY on completion.
	//
	// With incomplete-dir enabled — which the captured instance has — the
	// bytes are not under downloadDir until the transfer finishes. Reporting a
	// mid-transfer path would hand ingest something that does not exist, and
	// it would look like an ingest bug rather than a timing one.
	if done {
		out.Path = c.resolvePath(t)
	}
	return out
}

// resolvePath translates the client's path into one Heyarr can open.
//
// An UNMAPPED path is returned as-is rather than refused, and that is the right
// default: the common single-machine deployment has Transmission and Heyarr
// sharing a filesystem, and demanding a mapping that says `/downloads` means
// `/downloads` would be ceremony. Whether an unmapped path actually resolves is
// checked at startup, where it can be reported before any bytes move.
func (c *Client) resolvePath(t torrent) string {
	full := path.Join(path.Clean(t.DownloadDir), t.Name)
	if mapped, ok := c.pathMap.Resolve(full); ok {
		return mapped
	}
	return full
}

// Add queues a release, tagged as ours.
//
// Idempotent by construction: Transmission answers a duplicate with
// `torrent-duplicate` and the existing transfer, which this returns rather than
// treating as an error. That matters because the job that calls this WILL be
// re-run (invariant 9), and a second copy of a transfer already downloading is
// the duplicate grab this whole design exists to prevent.
func (c *Client) Add(ctx context.Context, source secret.Value) (providers.Transfer, error) {
	// Reveal() here and nowhere else in this method: this is the point the
	// value goes on the wire — to the indexer as a .torrent fetch, or to
	// Transmission as a magnet. It must not reach the error below, the labels
	// branch, or any log line — on a private tracker it carries a passkey that
	// identifies a person.
	source0 := strings.TrimSpace(source.Reveal())
	if source0 == "" {
		return providers.Transfer{}, errors.New("downloads: nothing to add")
	}

	// Who fetches the .torrent — Heyarr or Transmission — is the whole of #492.
	// See addSource: an http(s) URL is fetched HERE and handed over as base64
	// metainfo, because the download client may not be able to reach the
	// indexer (a loopback-bound Prowlarr, a container network namespace). A
	// magnet has nothing to fetch and is passed through as filename.
	args, err := c.addSource(ctx, source0)
	if err != nil {
		return providers.Transfer{}, err
	}
	if c.session.SupportsLabels() {
		args["labels"] = []string{c.label}
	} else {
		// The fallback, for instances below RPC 16. A subdirectory of the
		// download directory standing in for a label — which is what the *arr
		// stack still does everywhere, because it predates labels existing.
		args["download-dir"] = path.Join(c.downloadDir(), c.label)
	}

	var res struct {
		Added     *torrent `json:"torrent-added"`
		Duplicate *torrent `json:"torrent-duplicate"`
	}
	if err := c.rpc.call(ctx, "torrent-add", args, &res); err != nil {
		return providers.Transfer{}, err
	}
	switch {
	case res.Added != nil:
		return c.toTransfer(*res.Added), nil
	case res.Duplicate != nil:
		// Already there. The caller gets the same value it would have got the
		// first time, so a re-run is indistinguishable from the original.
		return c.toTransfer(*res.Duplicate), nil
	default:
		return providers.Transfer{}, fmt.Errorf(
			"%w: torrent-add reported success but named no transfer", ErrRPCFailure)
	}
}

// addSource decides how the release reaches Transmission's torrent-add, and is
// where #492 is actually fixed.
//
// Transmission's torrent-add takes EITHER a `filename` — which it fetches
// itself, whether that is a magnet, a local path or an http(s) URL — OR a
// base64 `metainfo`, the .torrent bytes handed to it directly. The two are
// alternatives, and choosing between them is choosing WHO fetches the .torrent:
//
//   - A magnet has no file to fetch: the bytes come from the swarm via DHT and
//     trackers, so it is passed through as `filename` unchanged. Nothing Heyarr
//     could do would help, and rewriting it would only risk dropping a tracker.
//
//   - An http(s) URL is the indexer's .torrent download link, and letting
//     Transmission fetch it is the bug: the indexer here is a loopback-bound
//     Prowlarr that Heyarr (host-native) can reach but Transmission (in its own
//     container network namespace) cannot, so its fetch returns "No Response"
//     and every grab parks at SELECTED forever. Heyarr CAN reach the indexer,
//     so Heyarr fetches the .torrent and hands over its bytes as `metainfo`.
//
//     Fetch-then-metainfo is deliberately not "convert to a bare magnet from
//     the infohash": a private tracker embeds a passkey in the .torrent, and a
//     magnet built from the infohash alone would DROP it, so the transfer would
//     never announce. Preserving the bytes preserves the passkey.
//
// Anything else — a local file path an operator configured — is left as
// `filename`, the pre-#492 behaviour, because Transmission opening a local file
// never depended on reaching the indexer.
func (c *Client) addSource(ctx context.Context, source string) (map[string]any, error) {
	if !isHTTPURL(source) {
		return map[string]any{"filename": source}, nil
	}
	blob, err := c.fetchTorrent(ctx, source)
	if err != nil {
		// An http(s) download link that redirects to a magnet is a magnet
		// source in disguise — how a magnet-only tracker behind Prowlarr
		// answers its /download link (a 301 to `magnet:`, not a .torrent).
		// There is nothing to fetch: hand the magnet to Transmission as
		// `filename`, exactly the path a bare magnet already takes.
		var mr *magnetRedirect
		if errors.As(err, &mr) {
			return map[string]any{"filename": mr.magnet}, nil
		}
		return nil, err
	}
	return map[string]any{"metainfo": base64.StdEncoding.EncodeToString(blob)}, nil
}

// magnetRedirect signals that an http(s) .torrent link resolved, via a
// redirect, to a magnet URI. addSource turns it into a `filename` for
// Transmission rather than an error.
//
// It carries the magnet, which on a private tracker embeds a passkey — so its
// Error() names neither the magnet nor the link, and addSource consumes the
// value rather than letting it reach a log line.
type magnetRedirect struct{ magnet string }

func (e *magnetRedirect) Error() string { return "the .torrent link redirected to a magnet" }

// maxTorrentRedirects bounds the redirect chain a .torrent fetch will follow.
// An indexer that needs more than a couple of hops to answer a download link is
// misconfigured; the cap stops a redirect loop from hanging the fetch.
const maxTorrentRedirects = 5

// isMagnet reports whether a redirect Location is a magnet URI.
func isMagnet(loc string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(loc)), "magnet:")
}

// resolveRedirect resolves a (possibly relative) Location against the URL it
// came from, as an http client would.
func resolveRedirect(base, loc string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	l, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(l).String(), nil
}

// isHTTPURL reports whether source is an http(s) URL — the shape Heyarr fetches
// itself rather than hand to Transmission.
//
// The test is the SCHEME, not a `.torrent` suffix: a Torznab indexer's download
// link routinely has none — Prowlarr's is `/<n>/download?apikey=…` — and the
// only reason a torrent client is ever handed an http(s) link is a .torrent
// behind it. A magnet, which starts `magnet:`, is not one and falls through to
// `filename`.
func isHTTPURL(source string) bool {
	lower := strings.ToLower(source)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// maxTorrentBytes bounds one .torrent fetch.
//
// A .torrent is metadata — names, sizes and one SHA-1 per piece — so even a
// large multi-file release is a few megabytes of it. This is generous enough
// never to be met in practice and a bound on what a misbehaving or hostile
// endpoint at a configured URL can make this process allocate.
const maxTorrentBytes = 16 << 20

// fetchTorrent retrieves a .torrent from the indexer over Heyarr's own HTTP
// path, which is the point of #492: Heyarr can reach the indexer.
//
// The credential rides in the URL — a Torznab download link carries its own
// apikey/passkey query, the same self-contained link the plain-HTTP client
// fetches — so the request is issued verbatim and adds no Transmission
// credential of its own. The URL is never named in an error: it is a secret
// that reaches an operator's log through registry.Grab, exactly as source is.
func (c *Client) fetchTorrent(ctx context.Context, rawURL string) ([]byte, error) {
	// Redirects are followed BY HAND rather than by the http client, for one
	// reason: a Prowlarr /download link for a magnet-only tracker answers with a
	// 301 to a `magnet:` URI, which the default client tries to follow and fails
	// on ("unsupported protocol scheme"). Inspecting each hop lets a magnet be
	// caught (returned as *magnetRedirect for addSource to hand to Transmission)
	// while an ordinary http(s) redirect to the actual .torrent is still
	// followed. Transport and Timeout are inherited; only the redirect policy
	// differs, so an injected test client's Transport still routes.
	client := &http.Client{Transport: c.httpc.Transport, Timeout: c.httpc.Timeout}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}

	current := rawURL
	for hop := 0; hop <= maxTorrentRedirects; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current, nil)
		if err != nil {
			return nil, &rpcError{Detail: "building the .torrent fetch request", err: err}
		}
		resp, err := client.Do(req)
		if err != nil {
			// The URL is NOT named: it carries a passkey. "the indexer" is
			// enough to say where the fetch went without disclosing the link.
			return nil, &rpcError{Detail: "could not fetch the .torrent from the indexer", err: err}
		}

		if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
			loc := resp.Header.Get("Location")
			_ = resp.Body.Close()
			if loc == "" {
				return nil, &rpcError{
					Status: resp.StatusCode,
					Detail: "the indexer redirected the .torrent fetch with no location",
				}
			}
			// The destination is a magnet, not another link to fetch.
			if isMagnet(loc) {
				return nil, &magnetRedirect{magnet: loc}
			}
			next, err := resolveRedirect(current, loc)
			if err != nil {
				return nil, &rpcError{Detail: "the indexer redirected the .torrent fetch to an unreadable location", err: err}
			}
			current = next
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			_ = resp.Body.Close()
			return nil, &rpcError{
				Status: resp.StatusCode,
				Detail: fmt.Sprintf("the indexer answered %d fetching the .torrent", resp.StatusCode),
			}
		}

		blob, err := io.ReadAll(io.LimitReader(resp.Body, maxTorrentBytes))
		_ = resp.Body.Close()
		if err != nil {
			return nil, &rpcError{Detail: "reading the .torrent from the indexer", err: err}
		}
		if len(blob) == 0 {
			return nil, &rpcError{Detail: "the indexer returned an empty .torrent"}
		}
		return blob, nil
	}
	return nil, &rpcError{Detail: "the indexer redirected the .torrent fetch too many times"}
}

// Remove takes a transfer out of the client.
//
// # It refuses to touch anything that is not ours
//
// The id is looked up in OUR filtered view first. A caller handing this a
// foreign infohash — from a stale row, from a bug, from anywhere — gets a
// refusal rather than a removal. This is the safety property the label exists
// for, and it is enforced here rather than trusted to callers.
//
// deleteData is separate from removal because "stop tracking this" and "delete
// the bytes" are different decisions, and the *arr ecosystem splits them for
// good reason. Removal must never delete data Heyarr has not yet ingested —
// that is the caller's judgement, and this makes them state it.
func (c *Client) Remove(ctx context.Context, id string, deleteData bool) error {
	ours, err := c.ours(ctx)
	if err != nil {
		return err
	}
	var found *torrent
	for i := range ours {
		if strings.EqualFold(ours[i].HashString, id) {
			found = &ours[i]
			break
		}
	}
	if found == nil {
		// Not ours, or not there. Both are refusals and the message says so
		// without asserting which — Heyarr cannot tell them apart, and
		// claiming it can would be a lie an operator might act on.
		return fmt.Errorf("%w: %s is not a transfer Heyarr queued", ErrNotOurs, id)
	}

	args := map[string]any{
		"ids":               []any{found.ID},
		"delete-local-data": deleteData,
	}
	return c.rpc.call(ctx, "torrent-remove", args, nil)
}

// ErrNotOurs is what an operation on a foreign transfer produces.
//
// A distinct error because it is the safety property firing, and a caller
// seeing it has a bug rather than a transient failure. Retrying will not help
// and should not be attempted.
var ErrNotOurs = errors.New("downloads: that transfer does not belong to Heyarr")

// downloadDir is where the instance said it puts things.
//
// Falls back to a relative "downloads" only when nothing has asked yet, which
// cannot happen in practice: Add is reached through the poll job, which runs
// after a health check. It exists so the fallback path has no nil case rather
// than because it is expected.
func (c *Client) downloadDir() string {
	if c.session.DownloadDir != "" {
		return c.session.DownloadDir
	}
	return "downloads"
}
