package resources

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/persistence/catalog"
	"github.com/rarebit-one/heyarr-core/internal/providers"
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
	// FanOut is where a want fan-out created came from and what became of it
	// here (ADR-0119, #799). Absent for a want this site created itself.
	FanOut *AcquisitionFanOut `json:"fan_out,omitempty"`
	// Published is the origin's side of fan-out: the acquire op this site
	// published for the want, and the siblings that reported they could not
	// source it (#799). Absent when this site published nothing for it.
	Published *AcquisitionPublished `json:"published,omitempty"`
}

// AcquisitionPublished is the acquire op this site signed for a want, and what
// came back from its siblings on the op-log.
//
// What a sibling did with the op is recorded only at that sibling; read it
// there (get_acquisition_status, list_fan_out). The one answer that travels
// back is an `unsourced` report, which is what UnsourcedBy shows.
type AcquisitionPublished struct {
	OpHash      string `json:"op_hash"`
	Infohash    string `json:"infohash"`
	ReleaseName string `json:"release_name"`
	Scope       string `json:"scope"`
	Aspect      string `json:"aspect"`
	Private     bool   `json:"private"`
	SelectedAt  string `json:"selected_at"`
	// UnsourcedBy are the sibling sites that reported they can resolve no
	// source for the release, so this site holds it for them to pull.
	UnsourcedBy []AcquisitionUnsourced `json:"unsourced_by,omitempty"`
	// Holds are the hold ops this site published for the want's work: bytes
	// it holds for a sibling to pull over the peer surface.
	Holds []AcquisitionPublishedHold `json:"holds,omitempty"`
}

// AcquisitionUnsourced is one sibling's unsourced report.
type AcquisitionUnsourced struct {
	Site       string `json:"site"`
	OpHash     string `json:"op_hash"`
	ReportedAt string `json:"reported_at"`
}

// AcquisitionPublishedHold is one hold op this site signed.
type AcquisitionPublishedHold struct {
	OpHash   string `json:"op_hash"`
	BlobHash string `json:"blob_hash"`
	Scope    string `json:"scope"`
	HeldAt   string `json:"held_at"`
}

// AcquisitionFanOut is the sibling's acquire op behind a want and this site's
// outcomes for it: the answer to "why didn't this site get X".
type AcquisitionFanOut struct {
	// OriginSite is the site that chose the release.
	OriginSite string `json:"origin_site"`
	// OpHash is the acquire op's hash.
	OpHash      string `json:"op_hash"`
	Infohash    string `json:"infohash"`
	ReleaseName string `json:"release_name"`
	Scope       string `json:"scope"`
	Aspect      string `json:"aspect"`
	// Outcome is what this site did with the op: applied, held, deferred,
	// tombstoned, scope_unresolved or no_site_source.
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail,omitempty"`
	AppliedAt string `json:"applied_at"`
	// ReportedUnsourced is whether this site told the origin it can resolve
	// no source for the release (an `unsourced` op), so the origin holds it
	// for this site to pull (ADR-0119 #800 amendment).
	ReportedUnsourced bool `json:"reported_unsourced"`
	// PeerPull is this site's outcome for each hold op naming the want's
	// work. Absent when none does.
	PeerPull []AcquisitionHold `json:"peer_pull,omitempty"`
}

// AcquisitionHold is what this site did with one hold op, as
// get_replica_status reports it.
type AcquisitionHold struct {
	OpHash    string `json:"op_hash"`
	BlobHash  string `json:"blob_hash"`
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail,omitempty"`
	Library   string `json:"library,omitempty"`
	AppliedAt string `json:"applied_at"`
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
	// Stopped reports that the client has held the transfer stopped for
	// longer than StoppedGrace (#883). Absent while it runs, and for a stop
	// too recent to be more than a poll catching up. It is not trouble: the
	// want is still waiting on this transfer, and nothing fails it.
	Stopped *AcquisitionStopped `json:"stopped,omitempty"`
}

// AcquisitionStopped is why a transfer is stopped at its client, and since when.
type AcquisitionStopped struct {
	// Reason is "held" — added stopped by the client's own default and never
	// run — or "after_run" — stopped after it ran, by an operator or the
	// client.
	Reason string `json:"reason"`
	Since  string `json:"since"`
	// Detail says it in a sentence, with what to do about it.
	Detail string `json:"detail"`
}

// StoppedGrace is how long a transfer must stay stopped before status reports
// it. A held transfer is started by the next poll when its client can start
// one (#882), so a stop younger than a few polls is the poll catching up, not
// something to show.
const StoppedGrace = 10 * time.Minute

// stoppedView renders a stopped transfer for status, nil when it is running
// or has not been stopped for StoppedGrace.
func stoppedView(tr catalog.Acquisition, now time.Time) *AcquisitionStopped {
	if tr.Stopped == "" || tr.StoppedSince.IsZero() || now.Sub(tr.StoppedSince) < StoppedGrace {
		return nil
	}
	out := &AcquisitionStopped{
		Reason: string(tr.Stopped),
		Since:  tr.StoppedSince.UTC().Format(time.RFC3339),
	}
	switch tr.Stopped {
	case providers.StoppedHeld:
		out.Detail = "held stopped at " + tr.Provider + " since it was added, by the client's own " +
			"default, and it has not started — start it in the client, or set the client to " +
			"start the transfers it is given"
	default:
		out.Detail = "stopped at " + tr.Provider + " after it ran — resume it in the client, " +
			"or select another release for this want"
	}
	return out
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
			Stopped:     stoppedView(tr, a.now()),
		}
	}

	fo, found, err := a.catalog.FanOutStatusFor(ctx, id)
	if err != nil {
		return AcquisitionStatusView{}, err
	}
	if found {
		out.FanOut = &AcquisitionFanOut{
			OriginSite: fo.Site, OpHash: fo.OpHash, Infohash: fo.Infohash, ReleaseName: fo.ReleaseTitle,
			Scope: fo.Scope, Aspect: fo.Aspect, Outcome: string(fo.Outcome), Detail: fo.Detail,
			AppliedAt: fo.AppliedAt.UTC().Format(time.RFC3339), ReportedUnsourced: fo.Reported,
		}
		for _, h := range fo.Holds {
			out.FanOut.PeerPull = append(out.FanOut.PeerPull, AcquisitionHold{
				OpHash: h.OpHash, BlobHash: h.BlobHash, Outcome: string(h.Outcome), Detail: h.Detail,
				Library: fo.HoldLibraries[h.OpHash], AppliedAt: h.AppliedAt.UTC().Format(time.RFC3339),
			})
		}
	}

	pub, found, err := a.catalog.PublishedFor(ctx, id)
	if err != nil {
		return AcquisitionStatusView{}, err
	}
	if found {
		out.Published = &AcquisitionPublished{
			OpHash: pub.OpHash, Infohash: pub.Infohash, ReleaseName: pub.ReleaseTitle,
			Scope: pub.Scope, Aspect: pub.Aspect, Private: pub.Private,
			SelectedAt: pub.SelectedAt.UTC().Format(time.RFC3339),
		}
		for _, r := range pub.Unsourced {
			out.Published.UnsourcedBy = append(out.Published.UnsourcedBy, AcquisitionUnsourced{
				Site: r.Site, OpHash: r.OpHash, ReportedAt: r.ReportedAt.UTC().Format(time.RFC3339),
			})
		}
		for _, h := range pub.Holds {
			out.Published.Holds = append(out.Published.Holds, AcquisitionPublishedHold{
				OpHash: h.OpHash, BlobHash: h.Blob, Scope: h.Scope, HeldAt: h.HeldAt.UTC().Format(time.RFC3339),
			})
		}
	}
	return out, nil
}

// getAcquisitionStatus is GET /api/v1/desired/{id}/acquisition — a shell over
// AcquisitionStatus, the same read get_acquisition_status gives an agent.
func (a *API) getAcquisitionStatus(w http.ResponseWriter, r *http.Request) {
	out, err := a.AcquisitionStatus(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		a.fail(w, r, "desired item", err)
		return
	}
	a.write(w, r, http.StatusOK, out)
}
