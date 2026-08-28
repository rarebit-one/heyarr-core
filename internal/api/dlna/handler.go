package dlna

import (
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/rarebit-one/heyarr-core/internal/api/render"
	"github.com/rarebit-one/heyarr-core/internal/persistence/sqlite"
)

// Prefix is where the DLNA MediaServer's HTTP surface is mounted. Like the
// renderer, pairing and the Subsonic/OPDS adapters it lives OUTSIDE the
// authenticated /api/v1 group: a UPnP control point — a television, a speaker —
// authenticates with nothing at all, and the bytes it fetches ride the
// unauthenticated render route (ADR-0040) for exactly that reason.
const Prefix = "/dlna"

// capabilityTTL bounds how long a res URL a Browse hands out stays fetchable.
// It is generous because a person may browse, walk away and start playback
// minutes later, and short enough that a DIDL-Lite document leaked off the LAN
// stops working the same day.
const capabilityTTL = 6 * time.Hour

// Options builds a Handler.
type Options struct {
	// DB is the controller database, read through its reader pool. The adapter
	// is a read-only PROJECTION of the server-readable catalogue (§70) — it
	// never writes and never touches the encrypted personal-state plane (§72).
	DB *sqlite.DB
	// RenderSecret signs the capability URLs a Browse hands out. It is the SAME
	// secret the render route verifies with, so the URLs this adapter mints are
	// fetchable there and nowhere else (ADR-0040).
	RenderSecret []byte
	// BaseURL is the origin a device dials res URLs against — the advertised
	// address of this node. DIDL-Lite requires absolute URLs, so a device that
	// only got a control URL still knows where the bytes are.
	BaseURL string
	// FriendlyName is what the server calls itself in its device description.
	FriendlyName string
	// UDN is the stable unique device name a control point keys on.
	UDN    string
	Logger *slog.Logger
	Now    func() time.Time
}

// Handler is the DLNA/UPnP ContentDirectory MediaServer adapter (§70, #202).
//
// It answers the SOAP Browse action over a read-only projection of the
// catalogue, handing back DIDL-Lite whose resource URLs are render capabilities
// (ADR-0040). It is the browse/serve half of the video-and-audio client story —
// distinct from internal/renderer, which is the control half that drives a
// device. SSDP LAN advertisement and the real-device proof are a tracked
// follow-up: a control point given the description URL can browse and play
// without discovery, which is what makes this slice provable at all.
type Handler struct {
	reader       *sql.DB
	secret       []byte
	baseURL      string
	friendlyName string
	udn          string
	log          *slog.Logger
	now          func() time.Time
}

// New builds the handler, refusing a mis-wired one at construction.
func New(opts Options) (*Handler, error) {
	if opts.DB == nil {
		return nil, errors.New("dlna: a database is required")
	}
	if len(opts.RenderSecret) == 0 {
		return nil, errors.New("dlna: a render signing secret is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	name := opts.FriendlyName
	if name == "" {
		name = "Heyarr"
	}
	udn := opts.UDN
	if udn == "" {
		udn = "uuid:heyarr-mediaserver"
	}
	return &Handler{
		reader:       opts.DB.Reader(),
		secret:       opts.RenderSecret,
		baseURL:      opts.BaseURL,
		friendlyName: name,
		udn:          udn,
		log:          log.With("component", "dlna"),
		now:          now,
	}, nil
}

// Mount registers the MediaServer's HTTP surface on an UNAUTHENTICATED router
// (see Prefix). Three routes and no more: the device description a control
// point fetches first, the ContentDirectory service description it reads to
// learn the Browse action, and the control endpoint it POSTs the action to.
func (h *Handler) Mount(r chi.Router) {
	r.Get(Prefix+"/description.xml", h.handleDescription)
	r.Get(Prefix+"/ContentDirectory.xml", h.handleSCPD)
	r.Post(Prefix+"/control/ContentDirectory", h.handleControl)
}

func (h *Handler) handleDescription(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	_, _ = w.Write([]byte(deviceDescription(h.friendlyName, h.udn)))
}

func (h *Handler) handleSCPD(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	_, _ = w.Write([]byte(contentDirectorySCPD))
}

// handleControl dispatches a SOAP action. Browse is the only one implemented;
// anything else is a faithful "optional action not implemented" fault rather
// than a silent 404, so a control point learns the difference between a server
// that cannot do a thing and a URL that is not there.
func (h *Handler) handleControl(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeFault(w, 501, "action body unreadable")
		return
	}
	req, ok := parseBrowse(body)
	if !ok {
		// 401 is the UPnP code for "invalid action" — the action this service
		// does not implement, as opposed to a malformed argument (402).
		writeFault(w, 401, "only Browse is implemented")
		return
	}
	h.browse(w, r, req)
}

// renderURL mints a capability for one blob and returns the absolute res URL a
// device fetches it from. The MIME is signed into the capability, so the render
// route serves the bytes as the type the catalogue declared (ADR-0040).
func (h *Handler) renderURL(blobHash, mime string) (string, error) {
	capability := render.Capability{
		BlobHash:  blobHash,
		ExpiresAt: h.now().Add(capabilityTTL),
		MIME:      mime,
	}
	token, err := capability.Sign(h.secret)
	if err != nil {
		return "", err
	}
	return h.baseURL + render.Path(token), nil
}
