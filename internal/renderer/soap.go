package renderer

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// SOAP, as much of it as one control point needs.
//
// A UPnP action is an HTTP POST carrying a SOAP envelope, with the action name
// repeated in a SOAPAction header that the device matches on. There is no
// dependency here because the shape is fixed and small: three actions with
// arguments, one response type worth parsing, and one fault format. A SOAP
// library would be more code to configure than to write.

// Argument is one named action argument. Order matters — UPnP actions take
// positional arguments dressed as named ones, and a device will reject an
// envelope whose children are in the wrong order — so this is a slice rather
// than a map.
type Argument struct {
	Name  string
	Value string
}

// soapCall invokes one action and returns the raw response body.
func soapCall(ctx context.Context, client *http.Client, svc Service, action string, args []Argument, limit int64) ([]byte, error) {
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	body.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"`)
	body.WriteString(` s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&body, `<u:%s xmlns:u="%s">`, action, svc.Type)
	for _, a := range args {
		fmt.Fprintf(&body, "<%s>", a.Name)
		// Values are escaped because one of them is a URL with a query string
		// and another is DIDL-Lite, which is XML inside an XML element.
		_ = xml.EscapeText(&body, []byte(a.Value))
		fmt.Fprintf(&body, "</%s>", a.Name)
	}
	fmt.Fprintf(&body, `</u:%s></s:Body></s:Envelope>`, action)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, svc.ControlURL, strings.NewReader(body.String()))
	if err != nil {
		return nil, fmt.Errorf("renderer: building %s request: %w", action, err)
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", fmt.Sprintf(`"%s#%s"`, svc.Type, action))

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("renderer: %s: %w", action, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("renderer: reading %s response: %w", action, err)
	}
	if resp.StatusCode != http.StatusOK {
		// A UPnP fault is a 500 with a structured body, and the structured
		// part is the whole value: "701 Transition not available" and "714
		// Illegal MIME-type" are different problems with the same status code,
		// and an operator told only "500" cannot tell them apart.
		if code, desc, ok := parseFault(raw); ok {
			return nil, fmt.Errorf("renderer: %s refused: %s (UPnP error %s)", action, desc, code)
		}
		return nil, fmt.Errorf("renderer: %s returned %s", action, resp.Status)
	}
	return raw, nil
}

// parseFault reads a UPnP error out of a SOAP fault body.
func parseFault(body []byte) (code, description string, ok bool) {
	var fault struct {
		Code        string `xml:"Body>Fault>detail>UPnPError>errorCode"`
		Description string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
	}
	if err := xml.Unmarshal(bytes.TrimSpace(body), &fault); err != nil {
		return "", "", false
	}
	if fault.Code == "" {
		return "", "", false
	}
	if fault.Description == "" {
		fault.Description = "no description given"
	}
	return fault.Code, fault.Description, true
}
