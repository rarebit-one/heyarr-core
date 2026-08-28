package subsonic

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
)

// The protocol constants Heyarr answers as.
//
// apiVersion is the Subsonic REST protocol version this adapter implements, not
// Heyarr's own version — a client negotiates against it. 1.16.1 is the last
// Subsonic release and the floor every OpenSubsonic client expects.
//
// responseType and the openSubsonic flag are the two OpenSubsonic handshake
// fields (https://opensubsonic.netlify.app): a client reads `type` to learn
// which server it is really talking to, and `openSubsonic="true"` to know it may
// ask for the extension list. serverVersion is filled from the build at
// construction, so it is not a constant here.
const (
	apiVersion   = "1.16.1"
	responseType = "heyarr"

	// namespace is the XML namespace every Subsonic response carries. Clients
	// that parse XML strictly reject a response without it.
	namespace = "http://subsonic.org/restapi"

	// ignoredArticles is echoed in the artist index so a client sorts "The
	// Cartographers" under C, not T, the same way the server grouped it. It is
	// the Subsonic default set; the catalogue's own normalisation (sort_title)
	// is the authority for where a name actually lands.
	ignoredArticles = "The El La Los Las Le Les"
)

// Subsonic error codes (§70 maps onto these; the protocol defines the set).
//
// The adapter uses a deliberately small subset: a real client distinguishes
// "your credential is wrong" (40) from "that does not exist" (70) from "you
// asked wrong" (10/0), and little else changes its behaviour. Codes it will
// never usefully act on are collapsed into 0.
const (
	errGeneric       = 0
	errMissingParam  = 10
	errBadAuth       = 40
	errNotAuthorized = 50
	errNotFound      = 70
)

// response is one Subsonic reply, marshalling identically to XML and JSON.
//
// Every payload is a pointer so exactly one is set and the rest vanish under
// omitempty — a Subsonic response carries its single result inline on the
// envelope, not under a "data" key. Status is "ok" or "failed"; a failed
// response carries Error and nothing else.
type response struct {
	XMLName       xml.Name `json:"-" xml:"subsonic-response"`
	Namespace     string   `json:"-" xml:"xmlns,attr"`
	Status        string   `json:"status" xml:"status,attr"`
	Version       string   `json:"version" xml:"version,attr"`
	Type          string   `json:"type" xml:"type,attr"`
	ServerVersion string   `json:"serverVersion" xml:"serverVersion,attr"`
	OpenSubsonic  bool     `json:"openSubsonic" xml:"openSubsonic,attr"`

	Error        *subError     `json:"error,omitempty" xml:"error,omitempty"`
	License      *license      `json:"license,omitempty" xml:"license,omitempty"`
	MusicFolders *musicFolders `json:"musicFolders,omitempty" xml:"musicFolders,omitempty"`
	Artists      *artistsID3   `json:"artists,omitempty" xml:"artists,omitempty"`
	Artist       *artistID3    `json:"artist,omitempty" xml:"artist,omitempty"`
	AlbumList2   *albumList2   `json:"albumList2,omitempty" xml:"albumList2,omitempty"`
	Album        *albumID3     `json:"album,omitempty" xml:"album,omitempty"`
	// A pointer, not a bare slice: the extension list must be PRESENT even when
	// empty (that presence is how a client confirms OpenSubsonic support), and a
	// bare empty slice under omitempty would vanish. Nil pointer omits it from
	// every other response.
	OpenSubsonicExtensions *[]osExtension `json:"openSubsonicExtensions,omitempty" xml:"openSubsonicExtensions,omitempty"`
}

type subError struct {
	Code    int    `json:"code" xml:"code,attr"`
	Message string `json:"message" xml:"message,attr"`
}

type license struct {
	Valid bool `json:"valid" xml:"valid,attr"`
}

type musicFolders struct {
	MusicFolder []musicFolder `json:"musicFolder" xml:"musicFolder"`
}

type musicFolder struct {
	ID   int    `json:"id" xml:"id,attr"`
	Name string `json:"name" xml:"name,attr"`
}

// artistsID3 is the top-level browse index (getArtists), grouped into letter
// buckets the way the Subsonic clients render a sidebar.
type artistsID3 struct {
	IgnoredArticles string        `json:"ignoredArticles" xml:"ignoredArticles,attr"`
	Index           []artistIndex `json:"index" xml:"index"`
}

type artistIndex struct {
	Name   string          `json:"name" xml:"name,attr"`
	Artist []artistID3Item `json:"artist" xml:"artist"`
}

// artistID3Item is an artist as it appears in a listing (no albums inlined).
type artistID3Item struct {
	ID         string `json:"id" xml:"id,attr"`
	Name       string `json:"name" xml:"name,attr"`
	AlbumCount int    `json:"albumCount" xml:"albumCount,attr"`
}

// artistID3 is one artist with its albums inlined (getArtist).
type artistID3 struct {
	ID         string     `json:"id" xml:"id,attr"`
	Name       string     `json:"name" xml:"name,attr"`
	AlbumCount int        `json:"albumCount" xml:"albumCount,attr"`
	Album      []albumID3 `json:"album,omitempty" xml:"album,omitempty"`
}

type albumList2 struct {
	Album []albumID3 `json:"album" xml:"album"`
}

// albumID3 is an album, with its songs inlined only when returned by getAlbum.
type albumID3 struct {
	ID        string `json:"id" xml:"id,attr"`
	Name      string `json:"name" xml:"name,attr"`
	Artist    string `json:"artist,omitempty" xml:"artist,attr,omitempty"`
	ArtistID  string `json:"artistId,omitempty" xml:"artistId,attr,omitempty"`
	SongCount int    `json:"songCount" xml:"songCount,attr"`
	Duration  int    `json:"duration" xml:"duration,attr"`
	Year      int    `json:"year,omitempty" xml:"year,attr,omitempty"`
	Song      []song `json:"song,omitempty" xml:"song,omitempty"`
}

// song is a Child in Subsonic terms: one streamable track.
type song struct {
	ID          string `json:"id" xml:"id,attr"`
	Parent      string `json:"parent,omitempty" xml:"parent,attr,omitempty"`
	IsDir       bool   `json:"isDir" xml:"isDir,attr"`
	Title       string `json:"title" xml:"title,attr"`
	Album       string `json:"album,omitempty" xml:"album,attr,omitempty"`
	Artist      string `json:"artist,omitempty" xml:"artist,attr,omitempty"`
	Track       int    `json:"track,omitempty" xml:"track,attr,omitempty"`
	DiscNumber  int    `json:"discNumber,omitempty" xml:"discNumber,attr,omitempty"`
	Year        int    `json:"year,omitempty" xml:"year,attr,omitempty"`
	AlbumID     string `json:"albumId,omitempty" xml:"albumId,attr,omitempty"`
	ArtistID    string `json:"artistId,omitempty" xml:"artistId,attr,omitempty"`
	Size        int64  `json:"size,omitempty" xml:"size,attr,omitempty"`
	ContentType string `json:"contentType,omitempty" xml:"contentType,attr,omitempty"`
	Suffix      string `json:"suffix,omitempty" xml:"suffix,attr,omitempty"`
	Duration    int    `json:"duration,omitempty" xml:"duration,attr,omitempty"`
	BitRate     int    `json:"bitRate,omitempty" xml:"bitRate,attr,omitempty"`
	Type        string `json:"type,omitempty" xml:"type,attr,omitempty"`
}

type osExtension struct {
	Name     string `json:"name" xml:"name,attr"`
	Versions []int  `json:"versions" xml:"versions"`
}

// ok builds a successful envelope with the server-identifying fields filled.
func (h *Handler) ok() *response {
	return &response{
		Namespace:     namespace,
		Status:        "ok",
		Version:       apiVersion,
		Type:          responseType,
		ServerVersion: h.serverVersion,
		OpenSubsonic:  true,
	}
}

// fail builds a failed envelope carrying one error and no payload.
func (h *Handler) fail(code int, message string) *response {
	r := h.ok()
	r.Status = "failed"
	r.Error = &subError{Code: code, Message: message}
	return r
}

// write renders a response in the format the request asked for.
//
// The HTTP status is always 200, even on a Subsonic error: the protocol carries
// its own status inside the envelope, and clients read `status="failed"` there,
// not the HTTP code. Returning 401/404 breaks clients that only inspect the
// body — the whole reason Subsonic put status in the envelope.
func (h *Handler) write(w http.ResponseWriter, format string, r *response) {
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
