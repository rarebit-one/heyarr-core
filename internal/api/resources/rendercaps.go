package resources

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	httpapi "github.com/rarebit-one/heyarr-core/internal/api/http"
	"github.com/rarebit-one/heyarr-core/internal/api/problem"
	"github.com/rarebit-one/heyarr-core/internal/guest"
)

// Render capabilities for a key-holding client (ADR-0040, extended by ADR-0112).
//
// A first-party phone authenticates every read with a Device possession proof
// it signs on demand — and a locked phone cannot sign: its device key is
// usable only while the phone is unlocked. Audio playing under the lock screen
// therefore loses the ability to fetch its next range the moment the last
// proof is retired. The phone is, for as long as it stays locked, exactly the
// device ADR-0040 was written for: one that cannot send an Authorization
// header. So it is given what that device is given — a capability per track,
// minted while it CAN still authenticate, at the time the queue is built.
//
// This is not POST /playback. That route opens a consumption session, emits
// an event and mints a playback token per call; an album queue of twelve
// tracks would be twelve "plays" nobody made and twelve whole-library tokens.
// Minting a capability changes no state, so it needs only `read` — the same
// floor that already lets this caller fetch these bytes — and writes nothing.

// maxRenderCapabilities bounds one request. A queue longer than this mints in
// pages; a request this size is already several hours of audio, past the
// capability's own lifetime.
const maxRenderCapabilities = 200

// RenderCapabilitiesRequest names the assets to mint capabilities for.
type RenderCapabilitiesRequest struct {
	AssetIDs []string `json:"asset_ids"`
}

// RenderCapability is one asset's answer: a URL or a reason, never neither
// and never both — the same contract as StartPlaybackResponse.RenderURL.
type RenderCapability struct {
	AssetID           string     `json:"asset_id"`
	RenderURL         string     `json:"render_url,omitempty"`
	RenderUnavailable string     `json:"render_unavailable,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

// RenderCapabilitiesResponse answers in request order, one entry per asset id.
type RenderCapabilitiesResponse struct {
	Capabilities []RenderCapability `json:"capabilities"`
}

// renderNotConfigured is the reason given when this node mints nothing at all.
// Unlike POST /playback, which may answer with neither a URL nor a reason, a
// caller of this route asked for nothing else, and silence would leave it
// unable to tell "this node does not do this" from "something broke".
const renderNotConfigured = "this node has no render secret or public origin, so it mints no capabilities"

func (a *API) mintRenderCapabilities(w http.ResponseWriter, r *http.Request) {
	var body RenderCapabilitiesRequest
	if !decodeOr400(w, r, &body) {
		return
	}
	if len(body.AssetIDs) == 0 {
		httpapi.Fail(w, r, problem.BadRequest("asset_ids is required"))
		return
	}
	if len(body.AssetIDs) > maxRenderCapabilities {
		httpapi.Fail(w, r, problem.BadRequest(
			"asset_ids names more than "+strconv.Itoa(maxRenderCapabilities)+" assets; mint in pages"))
		return
	}

	ctx := r.Context()
	now := a.now().UTC()
	expires := now.Add(renderCapabilityTTL)
	out := RenderCapabilitiesResponse{Capabilities: make([]RenderCapability, 0, len(body.AssetIDs))}
	for _, assetID := range body.AssetIDs {
		capability := RenderCapability{AssetID: assetID}
		switch {
		case assetID == "":
			capability.RenderUnavailable = "an empty asset id names nothing"
		case len(a.renderSecret) == 0 || a.renderPublicBaseURL == "":
			capability.RenderUnavailable = renderNotConfigured
		default:
			url, unavailable, err := a.renderCapabilityFor(r, assetID, now)
			if err != nil {
				// A database failure is not a per-asset answer: the caller
				// cannot act on it, and a partial list would hide it.
				httpapi.Fail(w, r, a.problemFor(ctx, "render capability", err))
				return
			}
			capability.RenderURL, capability.RenderUnavailable = url, unavailable
			if url != "" {
				capability.ExpiresAt = &expires
			}
		}
		out.Capabilities = append(out.Capabilities, capability)
	}
	a.write(w, r, http.StatusOK, out)
}

// renderCapabilityFor mints one asset's capability against the PUBLIC origin
// — the one this caller already reaches the node at (ADR-0079: "browsers,
// phones and the login keep public_origin") — never the plain-HTTP render
// listener a television is pointed at, which a phone off the LAN cannot reach.
//
// An asset that does not exist is an answer, not an error; only a failure the
// caller cannot act on comes back as err.
func (a *API) renderCapabilityFor(r *http.Request, assetID string, now time.Time) (url, unavailable string, err error) {
	ctx := r.Context()
	var (
		blobHash    sql.NullString
		sourceClass string
	)
	err = a.reader.QueryRowContext(ctx,
		`SELECT blob_hash, source_class FROM assets WHERE id = ?`, assetID).Scan(&blobHash, &sourceClass)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "no asset with that identifier", nil
	}
	if err != nil {
		return "", "", err
	}
	// The guest content boundary (ADR-0074), exactly as GET /assets/{id} draws
	// it: a non-guest-visible asset does not exist for a Guest — the same answer
	// as an unknown id. A capability is an unauthenticated URL, so minting one
	// here would hand a Guest the bytes it cannot even see listed.
	if id, ok := httpapi.IdentityFrom(ctx); ok && id.Guest && !guest.Visible(sourceClass) {
		return "", "no asset with that identifier", nil
	}
	if !blobHash.Valid || blobHash.String == "" {
		// A linked asset has no blob (ADR-0020), so there is nothing to sign.
		return "", "this asset has no bytes on any peer", nil
	}
	route, err := a.routeBlob(ctx, blobHash.String)
	if err != nil {
		return "", "", err
	}
	// No usable replica: a capability signed here would verify and then fetch
	// nothing. POST /playback never gets this far either — its plan for an
	// unrouted blob is unplayable — so say why instead of signing a dead URL.
	if !route.Found {
		return "", route.Refusal(), nil
	}
	url, unavailable = a.renderURLAt(ctx, a.renderPublicBaseURL, route, blobHash.String, assetID, now)
	return url, unavailable, nil
}
