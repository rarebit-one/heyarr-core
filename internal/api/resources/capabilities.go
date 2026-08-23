package resources

import (
	"net/http"
	"sort"
	"time"
)

// The fleet capability view over HTTP (§6, §75, ADR-0039, M5-112).
//
// # The question this endpoint exists to answer
//
// ADR-0023 named the gap in its own consequences: "`/api/v1/system` describes
// the node answering the request, not the fleet. […] A fleet-wide view needs
// worker capability advertisement, which is tracked separately rather than
// invented here." This is that view.
//
// `/api/v1/system` still answers what the CONTROLLER resolved, which under
// `heyarr all` is the whole story and in a split deployment is one datum. This
// answers what every WORKER has proven, which is the datum that decides where a
// transcode actually goes.
//
// # Only live advertisements appear
//
// A worker that has died stops appearing here within its TTL, and nothing in
// this file has to know that: the catalog filters expired rows out, because an
// advertisement is only meaningful for as long as its author promised. An
// endpoint that showed stale rows with a "last seen" column would be inviting
// an operator to route work to a machine that is switched off.

// CapabilityHolder is one worker's live advertisement.
type CapabilityHolder struct {
	// WorkerID is the advertising worker, which is also the string that appears
	// as a job's lease owner — so a stuck job and a fleet entry name the same
	// thing.
	WorkerID string `json:"worker_id"`
	// PeerID and PeerName say which NODE it runs on, which is the unit the
	// question is asked about.
	PeerID   string `json:"peer_id,omitempty"`
	PeerName string `json:"peer_name,omitempty"`
	// Capabilities are the exact strings a job's required_capability is matched
	// against, sorted.
	Capabilities []HeldCapability `json:"capabilities"`
	// ExpiresAt is when this advertisement stops being honoured unless the
	// worker renews it. Present so an operator can tell a worker that is about
	// to fall out of the fleet from one that just arrived.
	ExpiresAt time.Time `json:"expires_at"`
}

// HeldCapability is one proven capability.
type HeldCapability struct {
	Name string `json:"name"`
	// Source says how it was established — `binary`, `probe` or `service` —
	// which is what says whether it is re-verified. A `probe` capability was
	// EXERCISED: a real encode of a handful of frames. A `binary` one was
	// resolved at startup and is not re-resolved (ADR-0023).
	Source string `json:"source"`
	// Detail is a few words for an operator: which encoder proved it.
	Detail string `json:"detail,omitempty"`
	// ProvedAt is when the proof was taken, NOT when the row was written. The
	// difference matters: a re-advertisement that reused a cached proof would
	// otherwise look freshly verified.
	ProvedAt time.Time `json:"proved_at"`
}

// CapabilitiesResponse is the GET /api/v1/capabilities body.
type CapabilitiesResponse struct {
	// Capability echoes the ?capability= filter, so a client holding a response
	// knows which question it answers.
	Capability string `json:"capability,omitempty"`
	// Holders is every live advertisement, one entry per worker.
	Holders []CapabilityHolder `json:"holders"`
	// Available is the union across the fleet: every capability at least one
	// live worker holds. It is stated rather than left to a client to compute,
	// because computing it means knowing that an expired holder contributes
	// nothing — and a dashboard that got that wrong would offer a transcode
	// nothing can run.
	Available []string `json:"available"`
}

// listCapabilities is GET /api/v1/capabilities.
//
// `?capability=` filters to holders of one capability, and the match is EXACT.
// `ffmpeg` is a prefix of `ffmpeg.encoder.hevc`, so a prefix match would answer
// "which nodes can encode AV1" with every node that merely has FFmpeg
// installed — which is the failure mode this whole feature exists to prevent,
// arriving through the read path instead of the probe.
func (a *API) listCapabilities(w http.ResponseWriter, r *http.Request) {
	only := r.URL.Query().Get("capability")

	out := CapabilitiesResponse{
		Capability: only,
		// Non-nil even when empty. A fleet where nothing has advertised is a
		// real answer and must marshal as [] rather than null, which reads as
		// "we could not find out".
		Holders:   []CapabilityHolder{},
		Available: []string{},
	}
	if a.catalog == nil {
		a.write(w, r, http.StatusOK, out)
		return
	}

	fleet, err := a.catalog.FleetCapabilities(r.Context(), only)
	if err != nil {
		a.fail(w, r, "capability", err)
		return
	}

	union := map[string]bool{}
	for _, ad := range fleet {
		holder := CapabilityHolder{
			WorkerID:     ad.WorkerID,
			PeerID:       ad.PeerID,
			PeerName:     ad.PeerName,
			Capabilities: make([]HeldCapability, 0, len(ad.Held)),
			ExpiresAt:    ad.ExpiresAt,
		}
		for _, h := range ad.Held {
			union[h.Name] = true
			holder.Capabilities = append(holder.Capabilities, HeldCapability{
				Name:     h.Name,
				Source:   string(h.Source),
				Detail:   h.Detail,
				ProvedAt: h.ProvedAt,
			})
		}
		out.Holders = append(out.Holders, holder)
	}

	for name := range union {
		out.Available = append(out.Available, name)
	}
	sort.Strings(out.Available)

	a.write(w, r, http.StatusOK, out)
}
