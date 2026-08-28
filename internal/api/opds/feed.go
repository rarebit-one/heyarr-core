package opds

import (
	"encoding/xml"
	"net/http"
)

// OPDS 1.2 is an Atom profile (https://specs.opds.io/opds-1.2): a catalog is an
// Atom feed, a publication is an Atom entry, and "download this book" is an
// acquisition link on that entry. It is the format the readers that matter —
// KOReader, Foliate, Marvin, Chunky, Panels — actually speak, which is why this
// adapter emits 1.2 XML rather than the newer 2.0 JSON.
const (
	atomNS = "http://www.w3.org/2005/Atom"
	opdsNS = "http://opds-spec.org/2010/catalog"

	// The two catalog media types. A reader dispatches on `kind`: a navigation
	// feed is a menu, an acquisition feed is a shelf of books.
	navType         = "application/atom+xml;profile=opds-catalog;kind=navigation"
	acquisitionType = "application/atom+xml;profile=opds-catalog;kind=acquisition"

	// Link relations OPDS defines.
	relSelf        = "self"
	relStart       = "start"
	relSubsection  = "subsection"
	relAcquisition = "http://opds-spec.org/acquisition"
)

// feed is an OPDS catalog feed, marshalling to OPDS 1.2 Atom XML.
type feed struct {
	XMLName xml.Name `xml:"feed"`
	Xmlns   string   `xml:"xmlns,attr"`
	Updated string   `xml:"updated"`
	ID      string   `xml:"id"`
	Title   string   `xml:"title"`
	Links   []link   `xml:"link"`
	Entries []entry  `xml:"entry"`
}

type link struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
	Type string `xml:"type,attr"`
}

// entry is one row of a feed: a subsection (in a navigation feed) or a
// publication (in an acquisition feed).
type entry struct {
	Title   string   `xml:"title"`
	ID      string   `xml:"id"`
	Updated string   `xml:"updated"`
	Authors []author `xml:"author,omitempty"`
	Links   []link   `xml:"link"`
	Content *content `xml:"content,omitempty"`
}

type author struct {
	Name string `xml:"name"`
}

type content struct {
	Type string `xml:"type,attr"`
	Text string `xml:",chardata"`
}

// write renders a feed with the OPDS content type the reader expects for its
// kind — a reader that gets the wrong kind will not descend into it.
func writeFeed(w http.ResponseWriter, kind string, f feed) {
	f.Xmlns = atomNS
	w.Header().Set("Content-Type", kind)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(f)
}
