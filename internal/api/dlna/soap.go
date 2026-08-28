package dlna

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
)

// The server half of UPnP SOAP. internal/renderer speaks the CLIENT half — it
// POSTs actions to a device — so this is its mirror: it RECEIVES an action POST
// and answers it. The shape is fixed and small (one action worth parsing, one
// response, one fault), so like the renderer it is hand-written rather than
// pulled from a SOAP library that would be more configuration than code.

const contentDirectoryType = "urn:schemas-upnp-org:service:ContentDirectory:1"

// browseRequest is the one action this service implements. The field names are
// the UPnP argument names; a control point sends them as child elements of the
// action element.
type browseRequest struct {
	ObjectID       string
	BrowseFlag     string
	Filter         string
	StartingIndex  int
	RequestedCount int
	SortCriteria   string
}

// parseBrowse pulls a Browse action out of a SOAP envelope. It is deliberately
// tolerant of the envelope's namespaces and whitespace — control points differ
// — and strict only about the one element it needs.
func parseBrowse(body []byte) (browseRequest, bool) {
	var env struct {
		Browse struct {
			ObjectID       string `xml:"ObjectID"`
			BrowseFlag     string `xml:"BrowseFlag"`
			Filter         string `xml:"Filter"`
			StartingIndex  int    `xml:"StartingIndex"`
			RequestedCount int    `xml:"RequestedCount"`
			SortCriteria   string `xml:"SortCriteria"`
		} `xml:"Body>Browse"`
	}
	if err := xml.Unmarshal(body, &env); err != nil {
		return browseRequest{}, false
	}
	b := env.Browse
	if b.ObjectID == "" && b.BrowseFlag == "" {
		return browseRequest{}, false
	}
	return browseRequest(b), true
}

// browseResponse is a Browse reply. Result is the DIDL-Lite document as a
// string; encoding/xml escapes it into the element, which is exactly the
// double-encoding UPnP requires (DIDL-Lite is XML carried as text).
type browseResponse struct {
	Result         string
	NumberReturned int
	TotalMatches   int
	UpdateID       int
}

// writeBrowseResponse renders a successful BrowseResponse envelope.
func writeBrowseResponse(w http.ResponseWriter, r browseResponse) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"`)
	b.WriteString(` s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:BrowseResponse xmlns:u="%s">`, contentDirectoryType)
	b.WriteString("<Result>")
	_ = xml.EscapeText(&b, []byte(r.Result))
	b.WriteString("</Result>")
	fmt.Fprintf(&b, "<NumberReturned>%d</NumberReturned>", r.NumberReturned)
	fmt.Fprintf(&b, "<TotalMatches>%d</TotalMatches>", r.TotalMatches)
	fmt.Fprintf(&b, "<UpdateID>%d</UpdateID>", r.UpdateID)
	b.WriteString(`</u:BrowseResponse></s:Body></s:Envelope>`)

	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// writeFault renders a SOAP fault carrying a UPnP error code. A control point
// reads the UPnPError, not the HTTP status, so the status is 500 as the SOAP
// binding requires and the meaning is in the body.
func writeFault(w http.ResponseWriter, code int, reason string) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"`)
	b.WriteString(` s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	b.WriteString(`<s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring>`)
	b.WriteString(`<detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">`)
	fmt.Fprintf(&b, "<errorCode>%d</errorCode><errorDescription>", code)
	_ = xml.EscapeText(&b, []byte(reason))
	b.WriteString(`</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)

	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(b.String()))
}
