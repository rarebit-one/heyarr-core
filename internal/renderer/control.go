package renderer

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// AVTransport, the half of UPnP that actually plays something (§68).
//
// Five verbs and two questions. The verbs are what a person means by play,
// pause, resume, stop and scrub; the questions are what feeds Heyarr's
// consumption session — which until now had a complete state machine
// (internal/domain/playback/session.go) and nothing in the system able to
// produce a single transition for it, because there was no player.
//
// # Loading and playing are two calls, and the order is not negotiable
//
// SetAVTransportURI hands the renderer a URL; Play starts it. A renderer that
// is told to Play with no URI answers UPnP error 701, and one that is handed a
// URI it cannot fetch fails at Play rather than at load — which is why Start
// does both and reports which of them failed.

// TransportState is what a renderer says it is doing.
//
// These are UPnP's own strings rather than Heyarr's session states. Mapping
// between them is a domain decision and does not belong at this edge, where
// the job is to report faithfully what the device said.
type TransportState string

const (
	StatePlaying        TransportState = "PLAYING"
	StatePausedPlayback TransportState = "PAUSED_PLAYBACK"
	StateStopped        TransportState = "STOPPED"
	StateTransitioning  TransportState = "TRANSITIONING"
	StateNoMediaPresent TransportState = "NO_MEDIA_PRESENT"
)

// Playing reports whether the renderer is actively consuming.
//
// TRANSITIONING counts. A Samsung sits in it for a second or two while it
// switches away from whatever app was on screen, and a caller that treats it
// as "not playing" will conclude a playback failed that is about to start.
// That mistake was made against real hardware before it was made here.
func (s TransportState) Playing() bool {
	return s == StatePlaying || s == StateTransitioning
}

// Position is where a renderer is in what it is playing.
type Position struct {
	// Elapsed is how far in. Renderers report H:MM:SS, sometimes with
	// fractional seconds, and occasionally as the literal "NOT_IMPLEMENTED".
	Elapsed time.Duration
	// Duration is the track length, or zero when the renderer does not know
	// it. Zero is common and is not an error: it is what a device reports
	// before it has parsed enough of the stream, and what it reports forever
	// for a live source.
	Duration time.Duration
	// URI is what the renderer believes it is playing. Worth checking before
	// recording progress against a session — a renderer that has moved on to
	// something else, or been driven by another app, will happily report a
	// position for content Heyarr never asked for.
	URI string
}

// Controller drives one renderer.
type Controller struct {
	client   *http.Client
	renderer Renderer
}

// NewController builds a controller for a renderer.
func NewController(client *http.Client, r Renderer) (*Controller, error) {
	if !r.AVTransport.Found() {
		return nil, fmt.Errorf("renderer: %s has no AVTransport service", r.FriendlyName)
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &Controller{client: client, renderer: r}, nil
}

// maxControlResponse bounds a control response. These are small — a position
// report is a few hundred bytes — and the bound is here for the same reason as
// the others: the responder is an appliance on a LAN Heyarr does not run.
const maxControlResponse = 256 << 10

// Start loads a URL and plays it.
//
// # Why the metadata is not optional
//
// The DIDL-Lite passed as CurrentURIMetaData is what a renderer uses to decide
// whether it will even attempt the content, and what it shows on screen while
// it buffers. A renderer handed an empty metadata string will often accept the
// URI and then refuse to play it, with no error at either end.
func (c *Controller) Start(ctx context.Context, url, title, mime string) error {
	if err := c.SetURI(ctx, url, title, mime); err != nil {
		return err
	}
	return c.Play(ctx)
}

// SetURI loads content without playing it.
func (c *Controller) SetURI(ctx context.Context, url, title, mime string) error {
	_, err := c.act(ctx, "SetAVTransportURI",
		Argument{Name: "InstanceID", Value: "0"},
		Argument{Name: "CurrentURI", Value: url},
		Argument{Name: "CurrentURIMetaData", Value: didl(url, title, mime)},
	)
	return err
}

// Play starts or resumes.
func (c *Controller) Play(ctx context.Context) error {
	_, err := c.act(ctx, "Play",
		Argument{Name: "InstanceID", Value: "0"},
		// Speed is a string and "1" is normal. It is not a float: "1.0" is
		// rejected by renderers that compare it literally against their
		// advertised speeds.
		Argument{Name: "Speed", Value: "1"},
	)
	return err
}

// Pause holds position.
func (c *Controller) Pause(ctx context.Context) error {
	_, err := c.act(ctx, "Pause", Argument{Name: "InstanceID", Value: "0"})
	return err
}

// Stop ends playback and releases the content.
func (c *Controller) Stop(ctx context.Context) error {
	_, err := c.act(ctx, "Stop", Argument{Name: "InstanceID", Value: "0"})
	return err
}

// Seek jumps to an absolute offset from the start.
//
// REL_TIME is the unit, which despite the name is an absolute position within
// the track rather than a delta from the current one. ABS_TIME exists and
// means something different (a wall-clock position in a broadcast), and
// choosing it here would produce a seek that works on some devices and does
// nothing on others.
func (c *Controller) Seek(ctx context.Context, to time.Duration) error {
	if to < 0 {
		to = 0
	}
	_, err := c.act(ctx, "Seek",
		Argument{Name: "InstanceID", Value: "0"},
		Argument{Name: "Unit", Value: "REL_TIME"},
		Argument{Name: "Target", Value: formatDuration(to)},
	)
	return err
}

// State asks what the renderer is doing.
func (c *Controller) State(ctx context.Context) (TransportState, error) {
	body, err := c.act(ctx, "GetTransportInfo", Argument{Name: "InstanceID", Value: "0"})
	if err != nil {
		return "", err
	}
	var out struct {
		State string `xml:"Body>GetTransportInfoResponse>CurrentTransportState"`
	}
	if err := xml.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("renderer: parsing GetTransportInfo: %w", err)
	}
	return TransportState(strings.TrimSpace(out.State)), nil
}

// Position asks where the renderer is.
func (c *Controller) Position(ctx context.Context) (Position, error) {
	body, err := c.act(ctx, "GetPositionInfo", Argument{Name: "InstanceID", Value: "0"})
	if err != nil {
		return Position{}, err
	}
	var out struct {
		RelTime  string `xml:"Body>GetPositionInfoResponse>RelTime"`
		Duration string `xml:"Body>GetPositionInfoResponse>TrackDuration"`
		URI      string `xml:"Body>GetPositionInfoResponse>TrackURI"`
	}
	if err := xml.Unmarshal(body, &out); err != nil {
		return Position{}, fmt.Errorf("renderer: parsing GetPositionInfo: %w", err)
	}
	return Position{
		Elapsed:  parseDuration(out.RelTime),
		Duration: parseDuration(out.Duration),
		URI:      strings.TrimSpace(out.URI),
	}, nil
}

func (c *Controller) act(ctx context.Context, action string, args ...Argument) ([]byte, error) {
	return soapCall(ctx, c.client, c.renderer.AVTransport, action, args, maxControlResponse)
}

// didl builds the CurrentURIMetaData document.
//
// It is XML inside an XML element, so it is escaped once here and again by the
// SOAP encoder — which looks wrong and is correct. The alternative, a CDATA
// section, is not accepted by every renderer.
//
// # DLNA.ORG_PN is absent on purpose
//
// The protocolInfo here claims the media type and nothing more specific. A
// profile name would name an exact codec-container-level triple, and a WRONG
// one is worse than none: the renderer believes it and then fails on content
// that does not match. Verified against a Samsung QN85B, which plays content
// offered as `http-get:*:video/mp4:*`.
func didl(url, title, mime string) string {
	if title == "" {
		title = "Heyarr"
	}
	class := "object.item.videoItem"
	if strings.HasPrefix(mime, "audio/") {
		class = "object.item.audioItem.musicTrack"
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	var b strings.Builder
	b.WriteString(`<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/"`)
	b.WriteString(` xmlns:dc="http://purl.org/dc/elements/1.1/"`)
	b.WriteString(` xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`)
	b.WriteString(`<item id="1" parentID="0" restricted="1">`)
	fmt.Fprintf(&b, `<dc:title>%s</dc:title>`, html.EscapeString(title))
	fmt.Fprintf(&b, `<upnp:class>%s</upnp:class>`, class)
	fmt.Fprintf(&b, `<res protocolInfo="http-get:*:%s:*">%s</res>`,
		html.EscapeString(mime), html.EscapeString(url))
	b.WriteString(`</item></DIDL-Lite>`)
	return b.String()
}

// formatDuration renders H:MM:SS, which is what UPnP's time type is.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d / time.Second)
	return fmt.Sprintf("%d:%02d:%02d", total/3600, (total%3600)/60, total%60)
}

// parseDuration reads UPnP's time type, tolerantly.
//
// Zero is returned for everything it cannot read, and that is deliberate: the
// literal "NOT_IMPLEMENTED" is a legal answer that real devices give, and so
// is an empty string, and neither is a reason to fail a progress poll. A
// caller that needs to tell "zero" from "unknown" should be asking whether the
// renderer is playing, not parsing this harder.
func parseDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" || s == "NOT_IMPLEMENTED" {
		return 0
	}
	// Fractional seconds are legal — "0:00:12.500" — and are dropped rather
	// than parsed. Nothing here needs millisecond progress, and a parser that
	// handled them would be a parser with a second failure mode.
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		s = s[:dot]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	// A two-part value is MM:SS, which some renderers send for short content.
	if len(parts) == 2 {
		parts = append([]string{"0"}, parts...)
	}
	var total time.Duration
	for i, unit := range []time.Duration{time.Hour, time.Minute, time.Second} {
		n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err != nil || n < 0 {
			return 0
		}
		total += time.Duration(n) * unit
	}
	return total
}
