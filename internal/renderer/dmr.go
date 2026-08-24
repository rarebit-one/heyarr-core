// Package renderer talks to UPnP MediaRenderers on the local network (§68).
//
// # What this is for
//
// Milestone 2 can plan a playback and hand back a URL, and nothing on a home
// network can consume one: a television, a speaker or a projector is given a
// URL and told to fetch it, and none of them can send an Authorization header
// or read a JSON plan. Heyarr's answer to "play this in the living room" has
// so far assumed a bespoke client that does not exist.
//
// A UPnP MediaRenderer — DLNA's DMR — is the one thing on a home network that
// already speaks a documented protocol for exactly this, and the two devices
// this package was written against both implement it: a 2022 Samsung
// television (DMR-1.50, AVTransport:1) and a Devialet Phantom II speaker
// (DMR-1.51, AVTransport:2). Neither needs an app installed to be driven.
//
// # This package is the edge, and only the edge
//
// It does discovery, XML and SOAP. Every judgement about what a renderer's
// answer MEANS lives in internal/domain/playback, which cannot import net or
// os (ADR-0006/0007). That split is what lets the vocabulary mapping be tested
// against captured fixtures from real hardware with nothing on the network.
package renderer

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
)

// maxDescription bounds a device description read.
//
// Descriptions are a few kilobytes; the largest seen in testing is 3.3 KB. The
// bound exists because this reads from an unauthenticated device on a LAN that
// Heyarr does not administer, and an unbounded io.ReadAll against a hostile or
// simply broken responder is an out-of-memory waiting to happen.
const maxDescription = 256 << 10

// maxProtocolInfo bounds a GetProtocolInfo response.
//
// These are genuinely large — the Samsung answers with 292 entries in 29 KB —
// so the bound is much higher than the description's and still nowhere near a
// legitimate answer.
const maxProtocolInfo = 4 << 20

// Renderer is a MediaRenderer found on the network.
//
// It is not a Device (§68) and must not be confused with one: a Device is a
// capability profile the controller stores, and this is a thing on the LAN
// that may or may not still be there. Discovery produces these; registering
// one as a Device is a separate, deliberate step, because a household should
// not silently acquire a catalog entry for every guest's phone that happens to
// answer an M-SEARCH.
type Renderer struct {
	// UDN is the device's stable unique identifier — a uuid: URN. It survives
	// a DHCP lease change, which the location URL does not, so it is what a
	// registered Device should key on.
	UDN          string
	FriendlyName string
	Manufacturer string
	ModelName    string
	// Location is the description URL the device advertised. Everything else
	// here was read from it.
	Location string
	// AVTransport is where transport commands go: SetAVTransportURI, Play,
	// Pause, Seek, Stop.
	AVTransport Service
	// ConnectionManager is where GetProtocolInfo goes.
	ConnectionManager Service
	// Profile is what the renderer said it can play, mapped into Heyarr's
	// vocabulary. Zero until FetchProfile fills it in — describing a device
	// and asking what it accepts are two round trips, and a caller listing
	// what is on the network should not have to pay for the second.
	Profile playback.DeviceProfile
}

// Service is one SOAP service on a renderer.
type Service struct {
	// Type is the full service URN, which carries the version:
	// urn:schemas-upnp-org:service:AVTransport:1. The version is not cosmetic
	// — it must be echoed back in the SOAPAction header and the request body,
	// and a renderer that advertises :2 will reject an action sent as :1.
	Type string
	// ControlURL is absolute. The description carries it relative, and
	// resolving it at parse time means nothing downstream has to remember to.
	ControlURL string
}

// Found reports whether the description advertised this service.
func (s Service) Found() bool { return s.Type != "" && s.ControlURL != "" }

// Describe fetches and parses a device description.
func Describe(ctx context.Context, client *http.Client, location string) (Renderer, error) {
	base, err := url.Parse(location)
	if err != nil {
		return Renderer{}, fmt.Errorf("renderer: parsing location %q: %w", location, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, location, nil)
	if err != nil {
		return Renderer{}, fmt.Errorf("renderer: building description request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return Renderer{}, fmt.Errorf("renderer: fetching description: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Renderer{}, fmt.Errorf("renderer: description returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDescription))
	if err != nil {
		return Renderer{}, fmt.Errorf("renderer: reading description: %w", err)
	}
	r, err := parseDescription(body, base)
	if err != nil {
		return Renderer{}, err
	}
	r.Location = location
	return r, nil
}

// deviceDescription mirrors the subset of UPnP's device description this needs.
//
// Namespaces are matched loosely — the element name without its namespace —
// because vendors disagree about which they declare and a strict match drops
// real devices. Samsung mixes in three Microsoft namespaces and one of its
// own; Devialet's Rygel build declares a DLNA namespace Samsung does not.
type deviceDescription struct {
	Device struct {
		DeviceType   string `xml:"deviceType"`
		FriendlyName string `xml:"friendlyName"`
		Manufacturer string `xml:"manufacturer"`
		ModelName    string `xml:"modelName"`
		UDN          string `xml:"UDN"`
		ServiceList  struct {
			Services []struct {
				ServiceType string `xml:"serviceType"`
				ControlURL  string `xml:"controlURL"`
			} `xml:"service"`
		} `xml:"serviceList"`
	} `xml:"device"`
}

// ErrNotRenderer is returned for a UPnP device that is not a MediaRenderer.
//
// An SSDP sweep of an ordinary home network turns up routers, printers and
// NAS boxes advertising UPnP for entirely unrelated reasons. Refusing them by
// name rather than failing to parse them keeps discovery's logs readable.
var ErrNotRenderer = errors.New("renderer: not a MediaRenderer")

func parseDescription(body []byte, base *url.URL) (Renderer, error) {
	var d deviceDescription
	if err := xml.Unmarshal(body, &d); err != nil {
		return Renderer{}, fmt.Errorf("renderer: parsing description: %w", err)
	}
	if !strings.Contains(d.Device.DeviceType, ":device:MediaRenderer:") {
		return Renderer{}, fmt.Errorf("%w: %s", ErrNotRenderer, d.Device.DeviceType)
	}

	r := Renderer{
		UDN:          strings.TrimSpace(d.Device.UDN),
		FriendlyName: strings.TrimSpace(d.Device.FriendlyName),
		Manufacturer: strings.TrimSpace(d.Device.Manufacturer),
		ModelName:    strings.TrimSpace(d.Device.ModelName),
	}
	for _, s := range d.Device.ServiceList.Services {
		svc := Service{Type: strings.TrimSpace(s.ServiceType)}
		ref, err := url.Parse(strings.TrimSpace(s.ControlURL))
		if err != nil {
			continue
		}
		svc.ControlURL = base.ResolveReference(ref).String()

		switch {
		case strings.Contains(svc.Type, ":service:AVTransport:"):
			r.AVTransport = svc
		case strings.Contains(svc.Type, ":service:ConnectionManager:"):
			r.ConnectionManager = svc
		}
	}
	if !r.AVTransport.Found() {
		// A renderer with no AVTransport cannot be told to play anything. It
		// is a legitimate UPnP device and useless to Heyarr, so it is refused
		// here rather than discovered and then failing at the first Play.
		return Renderer{}, fmt.Errorf("%w: no AVTransport service", ErrNotRenderer)
	}
	return r, nil
}

// FetchProfile asks the renderer what it can play and maps the answer into a
// DeviceProfile (§68).
//
// This is the call that removes the assumption in
// internal/api/resources/devices.go that a television cannot be interrogated.
func FetchProfile(ctx context.Context, client *http.Client, r Renderer) (playback.DeviceProfile, error) {
	if !r.ConnectionManager.Found() {
		return playback.DeviceProfile{}, errors.New("renderer: no ConnectionManager service")
	}
	body, err := soapCall(ctx, client, r.ConnectionManager, "GetProtocolInfo", nil, maxProtocolInfo)
	if err != nil {
		return playback.DeviceProfile{}, err
	}
	var envelope struct {
		Sink string `xml:"Body>GetProtocolInfoResponse>Sink"`
	}
	if err := xml.Unmarshal(body, &envelope); err != nil {
		return playback.DeviceProfile{}, fmt.Errorf("renderer: parsing GetProtocolInfo: %w", err)
	}
	// Source is deliberately ignored. It is what the device can SERVE, and a
	// renderer that also serves is a server Heyarr has no interest in being a
	// client of.
	return playback.ProfileFromProtocolInfo(envelope.Sink), nil
}
