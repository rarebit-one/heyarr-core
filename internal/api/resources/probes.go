package resources

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/jobs"
	"github.com/rarebit-one/heyarr-core/internal/media/probe"
)

// BlobProbe is what ffprobe said about a blob's bytes (§29).
//
// It hangs off the blob rather than the asset because a probe describes bytes,
// and bytes are identity (invariant 1): two assets sharing a blob share one
// probe.
type BlobProbe struct {
	BlobHash    string        `json:"blob_hash"`
	Container   string        `json:"container"`
	FormatLong  string        `json:"format_long,omitempty"`
	DurationSec *float64      `json:"duration_seconds"`
	BitrateBPS  *int64        `json:"bitrate_bps"`
	Streams     []ProbeStream `json:"streams"`
	// BytesRead and Materialised are how the probe was obtained, exposed so
	// §29 is auditable in a running deployment rather than only in a test. An
	// instance where every probe materialised is one where remote probing is
	// silently not working, and the only other symptom is that it feels slow.
	BytesRead    int64  `json:"bytes_read"`
	Materialised bool   `json:"materialised"`
	ProbedAt     string `json:"probed_at"`
}

// ProbeStream is one elementary stream, as the container declared it.
type ProbeStream struct {
	Index     int    `json:"index"`
	Type      string `json:"type"`
	Codec     string `json:"codec"`
	Profile   string `json:"profile,omitempty"`
	Level     int    `json:"level,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	FrameRate string `json:"frame_rate,omitempty"`
	// The picture's colour, as ffprobe named it, and its Dolby Vision record
	// (#886). Absent when the container declared none, and on any probe
	// taken before they were recorded — `heyarr probe backfill` re-reads those.
	ColorTransfer  string             `json:"color_transfer,omitempty"`
	ColorPrimaries string             `json:"color_primaries,omitempty"`
	ColorSpace     string             `json:"color_space,omitempty"`
	DolbyVision    *probe.DolbyVision `json:"dolby_vision,omitempty"`
	Channels       int                `json:"channels,omitempty"`
	SampleRate     int                `json:"sample_rate,omitempty"`
	BitrateBPS     int64              `json:"bitrate_bps,omitempty"`
	Language       string             `json:"language,omitempty"`
	Commentary     bool               `json:"commentary,omitempty"`
}

// getBlobProbe serves GET /api/v1/blobs/{hash}/probe.
//
// A blob with no probe is a 404, not an empty 200. "We have not probed this"
// and "this has no streams" are different answers, and a client that cannot
// tell them apart cannot decide whether to wait — which is exactly the state a
// node with no ffprobe leaves every blob in (ADR-0023).
func (a *API) getBlobProbe(w http.ResponseWriter, r *http.Request) {
	hash := httpapi.HashParam(r)

	var (
		p            BlobProbe
		duration     sql.NullFloat64
		bitrate      sql.NullInt64
		streams      string
		materialised int
	)
	err := a.reader.QueryRowContext(r.Context(), `
		SELECT blob_hash, container, format_long, duration_seconds, bitrate_bps,
		       streams, bytes_read, materialised, probed_at
		FROM blob_probes WHERE blob_hash = ?`, hash).
		Scan(&p.BlobHash, &p.Container, &p.FormatLong, &duration, &bitrate,
			&streams, &p.BytesRead, &materialised, &p.ProbedAt)
	if err != nil {
		a.fail(w, r, "blob probe", err)
		return
	}
	if duration.Valid {
		p.DurationSec = &duration.Float64
	}
	if bitrate.Valid {
		p.BitrateBPS = &bitrate.Int64
	}
	p.Materialised = materialised == 1
	if err := json.Unmarshal([]byte(streams), &p.Streams); err != nil {
		a.fail(w, r, "blob probe", err)
		return
	}
	if p.Streams == nil {
		p.Streams = []ProbeStream{}
	}
	a.write(w, r, http.StatusOK, p)
}

// ProbeBackfillRequest is the POST /probes/backfill body. A scope is required,
// for the reason SubtitleBackfillRequest gives: "this one film" must not be
// one forgotten flag from "every video on the node".
type ProbeBackfillRequest struct {
	LibraryID string `json:"library_id"`
	WorkID    string `json:"work_id"`
	All       bool   `json:"all"`
}

// backfillProbes re-probes every managed video in scope whose probe cannot say
// what its picture's dynamic range is (#886).
//
// A probe is taken at ingest and never again: the bytes are immutable, so a
// rescan deduplicates and enqueues nothing. That is right until the probe
// learns to read something new. Transfer characteristics were recorded from
// #885 and Dolby Vision from #886, so a 2160p Dolby Vision remux probed before
// either reads as SDR — and is offered to a screen that cannot show it, and
// re-encoded without a tone-map. This is the operator's lever to re-read those
// once.
//
// "Cannot say" is the filter: no probe at all, or no moving-picture stream
// with a transfer or a Dolby Vision record. A probe that already has either is
// left alone, so a re-run touches only what is still unanswered. That includes
// SDR files whose container declares no colour, which are re-probed each run to
// learn the same nothing again; a probe is a few hundred kilobytes of ranged
// reads, and telling "untagged" from "probed before tags were read" would need
// a probe version nothing records.
func (a *API) backfillProbes(w http.ResponseWriter, r *http.Request) {
	var body ProbeBackfillRequest
	if !decodeOr400(w, r, &body) {
		return
	}
	if body.LibraryID == "" && body.WorkID == "" && !body.All {
		httpapi.Fail(w, r, problem.BadRequest(
			"a scope is required: library_id, work_id, or all=true — refusing to guess between one film and the whole node"))
		return
	}

	blobs, err := a.videoBlobsWithoutColour(r.Context(), body.LibraryID, body.WorkID)
	if err != nil {
		a.fail(w, r, "asset", err)
		return
	}
	enqueued := 0
	for _, b := range blobs {
		if _, err := a.jobs.Enqueue(r.Context(), jobs.EnqueueOptions{
			Type:               probe.JobType,
			Payload:            probe.Payload{BlobHash: b.hash, Size: b.size},
			DedupeKey:          probe.DedupeKey(b.hash),
			RequiredCapability: probe.Capability,
		}); err != nil {
			a.fail(w, r, "job", err)
			return
		}
		enqueued++
	}
	a.write(w, r, http.StatusAccepted, map[string]any{
		"candidates": len(blobs),
		"enqueued":   enqueued,
	})
}

type blobRef struct {
	hash string
	size int64
}

// videoBlobsWithoutColour finds the distinct blobs of managed video assets in
// scope whose probe records neither a transfer nor a Dolby Vision record on
// any picture stream. A blob with no probe row has no streams, so it is
// included by the same predicate. Vault assets are not: their blobs are
// ciphertext.
func (a *API) videoBlobsWithoutColour(ctx context.Context, libraryID, workID string) ([]blobRef, error) {
	where := []string{
		"a.source_class = 'managed'",
		"a.blob_hash IS NOT NULL",
		"a.mime LIKE 'video/%'",
		`NOT EXISTS (
			SELECT 1 FROM blob_probes bp, json_each(bp.streams) j
			WHERE bp.blob_hash = a.blob_hash
			  AND json_extract(j.value, '$.type') = 'video'
			  AND NOT coalesce(json_extract(j.value, '$.attached_picture'), 0)
			  AND (json_extract(j.value, '$.color_transfer') IS NOT NULL
			       OR json_extract(j.value, '$.dolby_vision') IS NOT NULL)
		)`,
	}
	var args []any
	if libraryID != "" {
		where = append(where, "a.library_id = ?")
		args = append(args, libraryID)
	}
	if workID != "" {
		where = append(where, "e.work_id = ?")
		args = append(args, workID)
	}

	//nolint:gosec // the query is assembled only from the literal fragments above; every value is bound
	stmt := `
		SELECT DISTINCT a.blob_hash, b.size
		FROM assets a
		JOIN editions e ON e.id = a.edition_id
		JOIN blobs b ON b.hash = a.blob_hash
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY a.blob_hash`
	rows, err := a.reader.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []blobRef
	for rows.Next() {
		var b blobRef
		if err := rows.Scan(&b.hash, &b.size); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
