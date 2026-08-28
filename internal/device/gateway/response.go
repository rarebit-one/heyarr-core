package gateway

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
)

// The Subsonic protocol constants the gateway answers as.
//
// A stock Subsonic app negotiates against apiVersion, and reads responseType and
// the openSubsonic flag to learn which server it is talking to. The gateway is a
// Subsonic server in its own right — the ONE origin the app is pointed at — so it
// speaks the protocol directly here rather than borrowing the controller-side
// adapter's unexported types. Keeping a separate envelope is deliberate: the
// controller-side adapter (internal/api/subsonic) carries NO personal-state
// shapes by construction (§72), and the playlist envelope is exactly the
// personal-state shape it must not grow. The device owns it instead.
const (
	apiVersion   = "1.16.1"
	responseType = "heyarr-gateway"
	namespace    = "http://subsonic.org/restapi"
)

// Subsonic error codes (a small subset; the protocol defines the full set). The
// same subset the controller-side adapter uses, for the same reason: a client
// distinguishes "your credential is wrong" (40) from "that does not exist" (70)
// from "you asked wrong" (10/0), and little else changes its behaviour.
const (
	errGeneric      = 0
	errMissingParam = 10
	errBadAuth      = 40
	errNotFound     = 70
)

// response is one Subsonic reply, marshalling identically to XML and JSON, in the
// same envelope shape the controller-side adapter uses so a client cannot tell
// which Heyarr server answered. Every payload is a pointer so exactly one is set
// and the rest vanish under omitempty.
type response struct {
	XMLName       xml.Name `json:"-" xml:"subsonic-response"`
	Namespace     string   `json:"-" xml:"xmlns,attr"`
	Status        string   `json:"status" xml:"status,attr"`
	Version       string   `json:"version" xml:"version,attr"`
	Type          string   `json:"type" xml:"type,attr"`
	ServerVersion string   `json:"serverVersion" xml:"serverVersion,attr"`
	OpenSubsonic  bool     `json:"openSubsonic" xml:"openSubsonic,attr"`

	Error     *subError  `json:"error,omitempty" xml:"error,omitempty"`
	Playlists *playlists `json:"playlists,omitempty" xml:"playlists,omitempty"`
	Playlist  *playlist  `json:"playlist,omitempty" xml:"playlist,omitempty"`
}

type subError struct {
	Code    int    `json:"code" xml:"code,attr"`
	Message string `json:"message" xml:"message,attr"`
}

// playlists is the getPlaylists listing: every playlist this device can decrypt,
// without its entries inlined.
type playlists struct {
	Playlist []playlist `json:"playlist" xml:"playlist"`
}

// playlist is one playlist. getPlaylists returns it without Entry; getPlaylist
// returns it with Entry inlined.
type playlist struct {
	ID        string  `json:"id" xml:"id,attr"`
	Name      string  `json:"name" xml:"name,attr"`
	SongCount int     `json:"songCount" xml:"songCount,attr"`
	Entry     []entry `json:"entry,omitempty" xml:"entry,omitempty"`
}

// entry is one playlist item as a Subsonic Child.
//
// The id is the item id the playlist CRDT stores, verbatim — the same opaque
// string a client round-trips to the stream endpoint (which the gateway proxies
// to the controller). This slice does no catalogue enrichment: the personal
// state that lives on the device is the ordered list of item ids, and turning an
// id into a title/album/artist would mean a per-item catalogue read against the
// controller — library metadata that is the controller's to serve, not the
// device's to hold. So title mirrors the id; a first-party client that wants
// full metadata resolves each id through the proxied browse methods. See doc.go.
type entry struct {
	ID    string `json:"id" xml:"id,attr"`
	Title string `json:"title" xml:"title,attr"`
	IsDir bool   `json:"isDir" xml:"isDir,attr"`
}

// ok builds a successful envelope with the server-identifying fields filled.
func (s *Server) ok() *response {
	return &response{
		Namespace:     namespace,
		Status:        "ok",
		Version:       apiVersion,
		Type:          responseType,
		ServerVersion: s.serverVersion,
		OpenSubsonic:  true,
	}
}

// fail builds a failed envelope carrying one error and no payload.
func (s *Server) fail(code int, message string) *response {
	r := s.ok()
	r.Status = "failed"
	r.Error = &subError{Code: code, Message: message}
	return r
}

// write renders a response in the format the request asked for.
//
// The HTTP status is always 200, even on a Subsonic error: the protocol carries
// its own status inside the envelope, and clients read status="failed" there,
// not the HTTP code.
func (s *Server) write(w http.ResponseWriter, format string, r *response) {
	switch format {
	case "json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]*response{"subsonic-response": r})
	default:
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(xml.Header))
		_ = xml.NewEncoder(w).Encode(r)
	}
}
