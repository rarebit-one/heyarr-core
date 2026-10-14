// Package webfeedparse is a shared RSS/Atom parser used by both the
// providers/webfeed adapter (article enumeration) and the downloads/webcapture
// client (feed-content fallback for blocked article pages). It is a
// pure-parsing package with no imports from either of those packages, so
// neither coupling direction is inverted.
//
// Both the sniff and the full decode use a lenient XML decoder (Strict=false,
// Entity=HTMLEntity) so that a feed containing a raw & in element text — a
// common real-world authoring defect — is tolerated rather than rejected with
// a permanent XML syntax error. AutoClose is intentionally NOT set: the HTML
// auto-close list includes <link>, which would empty the RSS/Atom <link>
// element that carries the article URL.
package webfeedparse

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// Entry is the neutral shape both RSS and Atom feeds map onto.
type Entry struct {
	// Key is the source-stable deduplication identity: an RSS guid or Atom id,
	// falling back to the article link.
	Key string
	// Title is the human-readable headline.
	Title string
	// Link is the article's canonical URL.
	Link string
	// Published is the post date, UTC; zero when absent or unparseable.
	Published time.Time
	// Content is the full article body in HTML form, when the feed provides it
	// (RSS content:encoded or Atom <content type="html">). It is empty when the
	// feed carries only a summary or description — the caller must treat an
	// empty Content as unavailable, not as a zero-length article.
	Content string
}

// ParseFeed sniffs the document's root element and reads it as RSS or Atom,
// mapping each entry onto a flat Entry slice.
//
// Sniffing rather than trying both keeps a malformed feed an honest error: a
// document that is neither is reported as such, not silently read as an empty
// feed that would look like a publication with no articles.
func ParseFeed(raw []byte) ([]Entry, error) {
	root, err := rootElement(raw)
	if err != nil {
		return nil, err
	}
	switch root {
	case "rss", "rdf":
		var doc rssFeed
		if err := lenientDecode(raw, &doc); err != nil {
			return nil, fmt.Errorf("webfeedparse: parsing the RSS feed: %w", err)
		}
		items := doc.channelItems()
		out := make([]Entry, 0, len(items))
		for _, it := range items {
			out = append(out, Entry{
				Key:       firstNonEmpty(it.GUID.Value, it.Link),
				Title:     it.Title,
				Link:      it.Link,
				Published: parseTime(it.PubDate, it.Date),
				Content:   it.ContentEncoded,
			})
		}
		return out, nil
	case "feed":
		var doc atomFeed
		if err := lenientDecode(raw, &doc); err != nil {
			return nil, fmt.Errorf("webfeedparse: parsing the Atom feed: %w", err)
		}
		out := make([]Entry, 0, len(doc.Entries))
		for _, e := range doc.Entries {
			content := ""
			// Atom content type="" is technically plain text per spec, but
			// publishers routinely omit the attribute on HTML content. We
			// include "html" and "" content; "xhtml" is an XHTML DOM fragment,
			// not a raw HTML string, so we skip it.
			switch strings.ToLower(strings.TrimSpace(e.Content.Type)) {
			case "html", "":
				content = e.Content.Value
			}
			out = append(out, Entry{
				Key:       firstNonEmpty(e.ID, alternateHref(e)),
				Title:     e.Title,
				Link:      alternateHref(e),
				Published: parseTime(e.Published, e.Updated),
				Content:   content,
			})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("webfeedparse: the document is neither RSS nor Atom (root <%s>)", root)
	}
}

// lenientDecode unmarshals raw XML with a forgiving decoder.
func lenientDecode(raw []byte, v any) error {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	return dec.Decode(v)
}

// rootElement returns the local name of the document's first XML element.
func rootElement(raw []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("webfeedparse: the feed is not well-formed XML: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return strings.ToLower(se.Name.Local), nil
		}
	}
}

// alternateHref returns the article URL from an Atom entry's links: first an
// explicit rel="alternate", then a rel-less link (which Atom defines as
// alternate).
func alternateHref(e atomEntry) string {
	for _, l := range e.Links {
		if l.Rel == "alternate" {
			if h := strings.TrimSpace(l.Href); h != "" {
				return h
			}
		}
	}
	for _, l := range e.Links {
		if l.Rel == "" {
			if h := strings.TrimSpace(l.Href); h != "" {
				return h
			}
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// parseTime parses the first of the supplied timestamps that reads, trying RSS's
// RFC 822 layouts and Atom/Dublin-Core's RFC 3339.
func parseTime(vals ...string) time.Time {
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		for _, layout := range []string{
			time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822,
			time.RFC3339, time.RFC3339Nano, "2006-01-02T15:04:05Z07:00",
		} {
			if t, err := time.Parse(layout, v); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

// Wire shapes — unexported: nothing outside this package couples to RSS/Atom XML.

type rssFeed struct {
	Channel rssChannel `xml:"channel"`
	// RSS 1.0/RDF puts <item> as a sibling of <channel>, not inside it.
	Items []rssItem `xml:"item"`
}

type rssChannel struct {
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	Title string  `xml:"title"`
	Link  string  `xml:"link"`
	GUID  rssGUID `xml:"guid"`
	// PubDate is the RSS 2.0 publication date; Date is dc:date (RSS 1.0/RDF),
	// matched by local element name so the dc: namespace prefix is irrelevant.
	PubDate string `xml:"pubDate"`
	Date    string `xml:"date"`
	// ContentEncoded is the full article HTML in <content:encoded>; matched by
	// local name "encoded" — the content: namespace prefix is ignored.
	ContentEncoded string `xml:"encoded"`
}

type rssGUID struct {
	Value string `xml:",chardata"`
}

type atomFeed struct {
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID        string      `xml:"id"`
	Title     string      `xml:"title"`
	Published string      `xml:"published"`
	Updated   string      `xml:"updated"`
	Links     []atomLink  `xml:"link"`
	Content   atomContent `xml:"content"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Href string `xml:"href,attr"`
}

type atomContent struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}

// channelItems returns RSS items from whichever location they appear in (RSS
// 2.0 nests them inside <channel>; RSS 1.0/RDF places them as siblings).
func (f rssFeed) channelItems() []rssItem {
	if len(f.Channel.Items) > 0 {
		return f.Channel.Items
	}
	return f.Items
}
