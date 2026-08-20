package resources

import (
	"database/sql"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
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
	Index      int    `json:"index"`
	Type       string `json:"type"`
	Codec      string `json:"codec"`
	Profile    string `json:"profile,omitempty"`
	Level      int    `json:"level,omitempty"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	FrameRate  string `json:"frame_rate,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
	BitrateBPS int64  `json:"bitrate_bps,omitempty"`
	Language   string `json:"language,omitempty"`
}

// getBlobProbe serves GET /api/v1/blobs/{hash}/probe.
//
// A blob with no probe is a 404, not an empty 200. "We have not probed this"
// and "this has no streams" are different answers, and a client that cannot
// tell them apart cannot decide whether to wait — which is exactly the state a
// node with no ffprobe leaves every blob in (ADR-0023).
func (a *API) getBlobProbe(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")

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
