package indexers

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The Torznab wire format, and the one decision that decides whether this
// client works: WHAT COUNTS AS AN ERROR.

// ProtocolError is an error the indexer described in its own response.
//
// Torznab has its own error vocabulary — 100 is an invalid key, 200 a missing
// parameter, 201 an unsupported function for this indexer, 202 an unknown
// function — and it is carried in a document rather than in the status line.
type ProtocolError struct {
	// Code is Torznab's error code.
	Code int
	// Description is the server's own wording, which is the most useful thing
	// an operator can be shown and is never a credential.
	Description string
	// Status is the HTTP status the document arrived with, recorded because
	// the two are INDEPENDENT and a reader will not believe that until they
	// see both.
	Status int
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("the indexer returned error %d (%s), with HTTP %d",
		e.Code, e.Description, e.Status)
}

// Torznab's error codes, as far as this client distinguishes them.
const (
	// errCodeInvalidKey is a wrong or missing API key. A CONFIGURATION
	// problem: retrying it forever is the failure this client exists to
	// avoid.
	errCodeInvalidKey = 100
	// errCodeUnsupportedFunction is this indexer declining a function it does
	// not implement — a capability fact, not a fault.
	errCodeUnsupportedFunction = 201
	// errCodeNoSuchFunction is the server not knowing the function at all.
	errCodeNoSuchFunction = 202
)

// IsConfiguration reports whether this error will still be true on the next
// attempt.
//
// The distinction the health model needs: a wrong key is not a transient
// failure and must be reported as configuration rather than retried until
// somebody looks at a log.
func (e *ProtocolError) IsConfiguration() bool {
	return e.Code == errCodeInvalidKey ||
		e.Code == errCodeUnsupportedFunction ||
		e.Code == errCodeNoSuchFunction
}

// caps is the `t=caps` handshake document.
type caps struct {
	XMLName   xml.Name      `xml:"caps"`
	Server    capsServer    `xml:"server"`
	Limits    capsLimits    `xml:"limits"`
	Searching capsSearching `xml:"searching"`
}

type capsServer struct {
	// Title is the PRODUCT that answered — "Jackett", "Prowlarr". Read for
	// reporting only: nothing in this client may branch on it, because the
	// moment something does, this is a client for two products rather than an
	// implementation of a protocol.
	Title   string `xml:"title,attr"`
	Version string `xml:"version,attr"`
}

type capsLimits struct {
	Default int `xml:"default,attr"`
	Max     int `xml:"max,attr"`
}

type capsSearching struct {
	Search      capsSearch `xml:"search"`
	TVSearch    capsSearch `xml:"tv-search"`
	MovieSearch capsSearch `xml:"movie-search"`
	MusicSearch capsSearch `xml:"music-search"`
	BookSearch  capsSearch `xml:"book-search"`
}

type capsSearch struct {
	Available string `xml:"available,attr"`
}

// supported reads Torznab's yes/no, treating anything else as no.
//
// Absent means NO rather than yes. An indexer that did not say it can do
// something has not said it can, and assuming otherwise produces a search
// that returns nothing and reports success — the exact shape of failure this
// package is built to refuse.
func (c capsSearch) supported() bool {
	return strings.EqualFold(strings.TrimSpace(c.Available), "yes")
}

// feed is a `t=search` result document.
type feed struct {
	XMLName xml.Name `xml:"rss"`
	Channel struct {
		Items []item `xml:"item"`
	} `xml:"channel"`
}

// item is one release.
//
// Note what is NOT here: nothing reads <description>, and nothing parses
// <title>. Extracting attributes from a release title is the standing
// precedent this milestone declines to repeat — M2's HDR detection is a
// substring match and is recorded as a known weakness; a title is worse
// evidence than ffprobe output, not better.
type item struct {
	Title string `xml:"title"`
	GUID  string `xml:"guid"`
	// Size as an ELEMENT. Some servers also carry it as a torznab attr and
	// some carry it in the enclosure's length; all three are read, and they
	// are read in that order of trust.
	Size      string `xml:"size"`
	Enclosure struct {
		URL    string `xml:"url,attr"`
		Length string `xml:"length,attr"`
	} `xml:"enclosure"`
	// Attrs are the torznab:attr elements.
	//
	// Matched on LOCAL NAME with no namespace in the tag, so it works whether
	// the server declares the torznab namespace on the element, on the root,
	// or uses a different prefix for it. Pinning the namespace URI here would
	// be binding to one server's XML habits, which is the mistake this whole
	// package is arranged to avoid.
	Attrs []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"attr"`
}

// attr returns a torznab attribute's value and whether it was present AND
// non-empty.
//
// PRESENT-BUT-EMPTY COUNTS AS ABSENT, and that is not a tidiness decision.
// One of the two captured servers emits `<torznab:attr name="genre" value=""/>`
// on every item — the attribute is there, and it says nothing. Treating that
// as a determined value of "" would hand §63 a confident answer nobody has,
// which is precisely the difference between "could not determine" and a wrong
// answer with no reason attached.
func (i item) attr(name string) (string, bool) {
	for _, a := range i.Attrs {
		if !strings.EqualFold(a.Name, name) {
			continue
		}
		if strings.TrimSpace(a.Value) == "" {
			return "", false
		}
		return a.Value, true
	}
	return "", false
}

// ErrNotTorznab is a body that is not the protocol at all.
//
// Its own error because it is a different operator problem: an endpoint
// answering JSON or HTML on a Torznab path is usually a URL pointing at the
// wrong thing — a removed indexer id, a reverse proxy's error page, a
// product's own REST API — and "could not parse XML" sends somebody looking
// at the client instead of at the address.
var ErrNotTorznab = errors.New("the endpoint did not answer with Torznab XML")

// parse decodes a Torznab response body, whatever kind of document it is.
//
// ---------------------------------------------------------------------------
// THE STATUS CODE DOES NOT DECIDE WHETHER THIS IS AN ERROR. THE BODY DOES.
// ---------------------------------------------------------------------------
//
// Measured against two real servers, on the same request — a search with a
// deliberately wrong API key:
//
//	Jackett   HTTP 200  <error code="100" description="Invalid API Key" />
//	Prowlarr  HTTP 401  (an empty body)
//
// And on an unsupported function:
//
//	Jackett   HTTP 200  <error code="201" ... />
//	Prowlarr  HTTP 400  <error code="202" ... />
//
// So an error document arrives with 200 AND with 400, and an error arrives
// with no document at all. A client that gates parsing on a 2xx misses the
// first; a client that trusts the status line misses the second; and a client
// that checks only the status reads Jackett's invalid key as a successful
// empty search and reports "no releases found" forever.
//
// This is the second time a protocol in this milestone has hidden a failure
// behind a healthy-looking response — the first was a Transmission transfer
// reporting error=0 while its tracker was unreachable — so the rule here is
// to look at everything and trust nothing that has not been read.
func parse(status int, body []byte) (any, error) {
	// The root element is sniffed rather than guessed, because the three
	// document kinds share no wrapper and unmarshalling into the wrong one
	// silently yields a zero value — an empty feed, which reads as "no
	// releases found".
	root, err := rootElement(body)
	if err != nil {
		return nil, err
	}

	switch root {
	case "error":
		var e struct {
			Code        string `xml:"code,attr"`
			Description string `xml:"description,attr"`
		}
		if err := xml.Unmarshal(body, &e); err != nil {
			return nil, fmt.Errorf("the indexer returned an <error> document that "+
				"could not be read: %w", err)
		}
		code, _ := strconv.Atoi(strings.TrimSpace(e.Code))
		return nil, &ProtocolError{Code: code, Description: e.Description, Status: status}
	case "caps":
		var c caps
		if err := xml.Unmarshal(body, &c); err != nil {
			return nil, fmt.Errorf("the capabilities document could not be read: %w", err)
		}
		return &c, nil
	case "rss":
		var f feed
		if err := xml.Unmarshal(body, &f); err != nil {
			return nil, fmt.Errorf("the search results could not be read: %w", err)
		}
		return &f, nil
	default:
		return nil, fmt.Errorf("%w: its root element is <%s>", ErrNotTorznab, root)
	}
}

// rootElement names the first element of a document.
//
// It reports what it found rather than only that it failed: "the endpoint
// answered with <html>" is an address problem an operator can act on in
// seconds, and "unexpected EOF at byte 0" is an afternoon.
func rootElement(body []byte) (string, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		// An empty body is its own case, and it is a real one: one of the two
		// captured servers answers a bad key with 401 and nothing at all.
		return "", fmt.Errorf("%w: it returned an empty body", ErrNotTorznab)
	}
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%w: it contained no elements", ErrNotTorznab)
		}
		if err != nil {
			// Named with the beginning of what arrived, so a JSON body or an
			// HTML error page identifies itself. Bounded, because the body may
			// be a megabyte of proxy boilerplate, and ESCAPED of newlines so
			// one bad response cannot scribble over a log.
			// Both errors are wrapped: a caller asking "is this even
			// Torznab" and a caller asking "what did the decoder object to"
			// are different questions, and joining them lets errors.Is answer
			// each without the other having to be re-derived from prose.
			return "", fmt.Errorf("%w: %w (it begins %q)", ErrNotTorznab, err, excerpt(body))
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local, nil
		}
	}
}

// excerptLimit bounds what a parse failure quotes back.
const excerptLimit = 80

func excerpt(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > excerptLimit {
		return s[:excerptLimit] + "…"
	}
	return s
}
