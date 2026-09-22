package resources

import (
	"context"
	"errors"

	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
)

// AcquisitionStatusView is where a want is in §64's pipeline, plus the in-flight
// transfer behind it when one exists.
//
// It answers "what is this want actually doing right now" — the question that
// otherwise needed a look inside the download client: which release was chosen
// (its name carries the resolution and size), how far it has downloaded, and
// what, if anything, is wrong.
type AcquisitionStatusView struct {
	DesiredItemID string `json:"desired_item_id"`
	// Phase is the raw pipeline phase (idle, searching, candidates_found,
	// selected, queued, downloading, verifying, ingesting).
	Phase string `json:"phase"`
	// State is the §64 display name — the phase rendered for a reader.
	State     string `json:"state"`
	Managed   bool   `json:"managed"`
	Content   string `json:"content"`
	Placement string `json:"placement"`
	Detail    string `json:"detail,omitempty"`
	// Transfer is the download in flight. Absent when nothing is downloading —
	// a want that is idle, still searching, or already satisfied has none.
	Transfer *AcquisitionTransfer `json:"transfer,omitempty"`
}

// AcquisitionTransfer is the download client's side of an in-flight acquisition.
type AcquisitionTransfer struct {
	Provider string `json:"provider"`
	// ExternalID is the client's own identifier — an infohash for a torrent.
	ExternalID string `json:"external_id"`
	// ReleaseName is what was grabbed, verbatim. It is where the resolution,
	// codec and size live, so "why is this 9 GB / why is it 2160p" is answered
	// here rather than by opening the client.
	ReleaseName string  `json:"release_name"`
	RemotePath  string  `json:"remote_path,omitempty"`
	LocalPath   string  `json:"local_path,omitempty"`
	BytesTotal  int64   `json:"bytes_total"`
	BytesDone   int64   `json:"bytes_done"`
	PercentDone float64 `json:"percent_done"`
	// Trouble is the client's last error for this transfer, empty when healthy.
	Trouble string `json:"trouble,omitempty"`
}

// AcquisitionStatus reports a want's pipeline phase and its in-flight transfer.
//
// The phase state and the transfer row are two tables (state advances on §64
// edges; progress is refreshed by the download poll), joined here so a caller
// sees one answer. A want with no transfer is normal, not an error.
func (a *API) AcquisitionStatus(ctx context.Context, id string) (AcquisitionStatusView, error) {
	if a.catalog == nil {
		return AcquisitionStatusView{}, errors.New("resources: no catalog is wired, so " +
			"acquisition status cannot be read")
	}
	// Resolve the want first, so an unknown id is a clean client fault ("no
	// desired item with that identifier") rather than an empty status.
	if _, err := desiredByID(ctx, a.reader, id); err != nil {
		return AcquisitionStatusView{}, err
	}

	rec, err := a.catalog.Acquisition(ctx, id)
	if err != nil {
		return AcquisitionStatusView{}, err
	}
	out := AcquisitionStatusView{
		DesiredItemID: id,
		Phase:         string(rec.State.Phase),
		State:         rec.State.Name(),
		Managed:       rec.State.Managed,
		Content:       string(rec.State.Content),
		Placement:     string(rec.State.Placement),
		Detail:        rec.Detail,
	}

	tr, err := a.catalog.AcquisitionFor(ctx, id)
	switch {
	case errors.Is(err, catalog.ErrNoAcquisitionRow):
		// No transfer in flight — leave Transfer nil.
	case err != nil:
		return AcquisitionStatusView{}, err
	default:
		var pct float64
		if tr.BytesTotal > 0 {
			pct = float64(tr.BytesDone) / float64(tr.BytesTotal) * 100
		}
		out.Transfer = &AcquisitionTransfer{
			Provider:    tr.Provider,
			ExternalID:  tr.ExternalID,
			ReleaseName: tr.ExternalName,
			RemotePath:  tr.RemotePath,
			LocalPath:   tr.LocalPath,
			BytesTotal:  tr.BytesTotal,
			BytesDone:   tr.BytesDone,
			PercentDone: pct,
			Trouble:     tr.Trouble,
		}
	}
	return out, nil
}
