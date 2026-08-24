package downloads

import (
	"context"
	"errors"
	"fmt"
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
	// value goes on the wire to Transmission. It must not reach the error
	// below, the labels branch, or any log line — on a private tracker it
	// carries a passkey that identifies a person.
	filename := strings.TrimSpace(source.Reveal())
	if filename == "" {
		return providers.Transfer{}, errors.New("downloads: nothing to add")
	}

	args := map[string]any{"filename": filename}
	if c.session.SupportsLabels() {
		args["labels"] = []string{c.label}
	} else {
		// The fallback, for instances below RPC 16. A subdirectory of the
		// download directory standing in for a label — which is what the *arr
		// stack still does everywhere, because it predates labels existing.
		args["download-dir"] = path.Join(c.downloadDir(), c.label)
	}

	var raw struct {
		Added     *torrent `json:"torrent-added"`
		Duplicate *torrent `json:"torrent-duplicate"`
	}
	if err := c.rpc.call(ctx, "torrent-add", args, &raw); err != nil {
		return providers.Transfer{}, err
	}
	switch {
	case raw.Added != nil:
		return c.toTransfer(*raw.Added), nil
	case raw.Duplicate != nil:
		// Already there. The caller gets the same value it would have got the
		// first time, so a re-run is indistinguishable from the original.
		return c.toTransfer(*raw.Duplicate), nil
	default:
		return providers.Transfer{}, fmt.Errorf(
			"%w: torrent-add reported success but named no transfer", ErrRPCFailure)
	}
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
