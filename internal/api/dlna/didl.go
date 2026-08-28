package dlna

import "encoding/xml"

// DIDL-Lite is the metadata language a UPnP ContentDirectory answers Browse in
// (spec §70). It is XML nested INSIDE the SOAP response's Result element as an
// escaped string, so these types marshal to a self-contained document that the
// SOAP layer then escapes whole.
//
// The shape is deliberately small: a container (a browsable folder) and an item
// (a playable thing with a resource URL). Neither reshapes Heyarr's model — a
// container is a projection of a content-type grouping and an item is a
// projection of one Asset, exactly as the OpenSubsonic and OPDS adapters
// project the same rows into their own vocabularies.

// didlLite is the root <DIDL-Lite> element with the namespaces a control point
// expects. A device rejects a document missing the upnp/dc namespaces.
type didlLite struct {
	XMLName    xml.Name    `xml:"DIDL-Lite"`
	XMLNS      string      `xml:"xmlns,attr"`
	XMLNSDC    string      `xml:"xmlns:dc,attr"`
	XMLNSUPnP  string      `xml:"xmlns:upnp,attr"`
	Containers []container `xml:"container"`
	Items      []item      `xml:"item"`
}

func newDIDL() *didlLite {
	return &didlLite{
		XMLNS:     "urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/",
		XMLNSDC:   "http://purl.org/dc/elements/1.1/",
		XMLNSUPnP: "urn:schemas-upnp-org:metadata-1-0/upnp/",
	}
}

// container is a browsable folder: the root, or a content-type grouping.
type container struct {
	ID         string `xml:"id,attr"`
	ParentID   string `xml:"parentID,attr"`
	Restricted int    `xml:"restricted,attr"`
	ChildCount int    `xml:"childCount,attr"`
	Title      string `xml:"dc:title"`
	Class      string `xml:"upnp:class"`
}

// item is one playable Asset. res carries the bytes' location — a render
// capability URL (ADR-0040), because the device fetching it has no credential
// to present.
type item struct {
	ID         string `xml:"id,attr"`
	ParentID   string `xml:"parentID,attr"`
	Restricted int    `xml:"restricted,attr"`
	Title      string `xml:"dc:title"`
	Class      string `xml:"upnp:class"`
	Date       string `xml:"dc:date,omitempty"`
	Res        res    `xml:"res"`
}

// res is a resource: what to fetch, how big it is, and — in protocolInfo — the
// transport and MIME a device matches against before it will accept the item.
type res struct {
	ProtocolInfo string `xml:"protocolInfo,attr"`
	Size         int64  `xml:"size,attr,omitempty"`
	URL          string `xml:",chardata"`
}

// The UPnP classes this adapter emits. A device chooses an icon and a player
// from the class, so a movie must not be announced as audio.
const (
	classStorageFolder = "object.container.storageFolder"
	classVideoItem     = "object.item.videoItem"
	classAudioItem     = "object.item.audioItem.musicTrack"
	classItem          = "object.item"
)

// classFor maps a Heyarr content type to the UPnP class a control point renders
// it as. Unknown types fall back to the generic item class rather than being
// dropped — a browsable thing with an honest resource is better than a silent
// omission.
func classFor(contentType string) string {
	switch contentType {
	case "movie", "series":
		return classVideoItem
	case "music":
		return classAudioItem
	default:
		return classItem
	}
}

// marshal renders the DIDL-Lite document as the string the SOAP Result carries.
func (d *didlLite) marshal() (string, error) {
	out, err := xml.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
