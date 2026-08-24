package renderer

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
)

// The fixtures in testdata/dmr are verbatim responses from two real devices on
// a home network — a Samsung QA55QN85BAKXXS television and a Devialet Phantom
// II 95 dB — captured 2026-08-24. Serial numbers, the screen-mirroring MAC and
// the device UUIDs were replaced with obvious placeholders; nothing structural
// was touched.
//
// ADR-0026's reasoning about indexers applies here for the same reason: these
// responses cannot be reproduced on demand. A television is not available to
// CI, it answers differently in standby, and a firmware update changes what it
// says. The fixture is the only way this mapping gets tested at all.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "dmr", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return b
}

func TestParseDescription(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		file        string
		base        string
		wantName    string
		wantModel   string
		wantVendor  string
		wantAVType  string
		wantAVURL   string
		wantCMFound bool
	}{
		{
			name:        "samsung television, AVTransport:1, relative control URLs",
			file:        "samsung-qn85b-description.xml",
			base:        "http://192.0.2.61:9197/dmr",
			wantName:    "Samsung QN85BA 55",
			wantModel:   "QA55QN85BAKXXS",
			wantVendor:  "Samsung Electronics",
			wantAVType:  "urn:schemas-upnp-org:service:AVTransport:1",
			wantAVURL:   "http://192.0.2.61:9197/upnp/control/AVTransport1",
			wantCMFound: true,
		},
		{
			name:        "devialet speaker, AVTransport:2, a different URL layout",
			file:        "devialet-phantom2-description.xml",
			base:        "http://192.0.2.69:45317/00000000-0000-4000-8000-00000000dm02.xml",
			wantName:    "Phantom II 95 dB-a98d",
			wantModel:   "Phantom II 95 dB",
			wantVendor:  "Devialet",
			wantAVType:  "urn:schemas-upnp-org:service:AVTransport:2",
			wantAVURL:   "http://192.0.2.69:45317/Control/DvltRygelRendererPlugin/RygelAVTransport",
			wantCMFound: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base, err := url.Parse(tc.base)
			if err != nil {
				t.Fatalf("parsing base: %v", err)
			}
			r, err := parseDescription(fixture(t, tc.file), base)
			if err != nil {
				t.Fatalf("parseDescription: %v", err)
			}
			if r.FriendlyName != tc.wantName {
				t.Errorf("FriendlyName = %q, want %q", r.FriendlyName, tc.wantName)
			}
			if r.ModelName != tc.wantModel {
				t.Errorf("ModelName = %q, want %q", r.ModelName, tc.wantModel)
			}
			if r.Manufacturer != tc.wantVendor {
				t.Errorf("Manufacturer = %q, want %q", r.Manufacturer, tc.wantVendor)
			}
			// The service version is load-bearing: it goes back out in the
			// SOAPAction header, and these two devices disagree about it.
			if r.AVTransport.Type != tc.wantAVType {
				t.Errorf("AVTransport.Type = %q, want %q", r.AVTransport.Type, tc.wantAVType)
			}
			if r.AVTransport.ControlURL != tc.wantAVURL {
				t.Errorf("AVTransport.ControlURL = %q, want %q", r.AVTransport.ControlURL, tc.wantAVURL)
			}
			if r.ConnectionManager.Found() != tc.wantCMFound {
				t.Errorf("ConnectionManager.Found() = %v, want %v", r.ConnectionManager.Found(), tc.wantCMFound)
			}
			if r.UDN == "" {
				t.Error("UDN is empty; it is what a registered Device keys on")
			}
		})
	}
}

func TestParseDescriptionRejectsNonRenderers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "a UPnP device that is not a renderer",
			body: `<root xmlns="urn:schemas-upnp-org:device-1-0"><device>` +
				`<deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>` +
				`</device></root>`,
		},
		{
			// Discovery must not hand back something Play will fail on.
			name: "a renderer with no AVTransport",
			body: `<root xmlns="urn:schemas-upnp-org:device-1-0"><device>` +
				`<deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType>` +
				`<serviceList><service>` +
				`<serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>` +
				`<controlURL>/ctl</controlURL>` +
				`</service></serviceList></device></root>`,
		},
	}

	base, _ := url.Parse("http://192.0.2.1:80/desc.xml")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseDescription([]byte(tc.body), base); !errors.Is(err, ErrNotRenderer) {
				t.Fatalf("err = %v, want ErrNotRenderer", err)
			}
		})
	}
}

// TestProfileFromRealHardware is the point of the whole package: what the
// planner would be told about two devices that exist.
func TestProfileFromRealHardware(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		file            string
		wantContainers  []string
		wantVideoCodecs []string
		wantAudioCodecs []string
		absentVideo     []string
	}{
		{
			name: "samsung television",
			file: "samsung-qn85b-protocolinfo.xml",
			// mkv, webm and hevc are the interesting ones: they appear ONLY in
			// the trailing wildcard entries with no DLNA.ORG_PN. A reader that
			// honoured the specification and ignored unprofiled entries would
			// conclude this 4K set cannot play Matroska or HEVC.
			wantContainers:  []string{"mp4", "mkv", "webm", "avi", "ts"},
			wantVideoCodecs: []string{"h264", "hevc", "mpeg2video"},
			wantAudioCodecs: []string{"aac", "ac3", "mp3"},
		},
		{
			name:            "devialet speaker",
			file:            "devialet-phantom2-protocolinfo.xml",
			wantContainers:  []string{"mp3", "flac", "wav", "ogg"},
			wantAudioCodecs: []string{"mp3", "flac", "aac"},
			// A speaker must not come back claiming video support, or the
			// planner will happily route a film to it.
			absentVideo: []string{"h264", "hevc", "mpeg2video", "vc1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var envelope struct {
				Sink string `xml:"Body>GetProtocolInfoResponse>Sink"`
			}
			if err := xml.Unmarshal(fixture(t, tc.file), &envelope); err != nil {
				t.Fatalf("parsing fixture: %v", err)
			}
			if envelope.Sink == "" {
				t.Fatal("fixture yielded an empty Sink")
			}
			got := playback.ProfileFromProtocolInfo(envelope.Sink)

			assertAll(t, "container", got.Containers, tc.wantContainers)
			assertAll(t, "video codec", got.VideoCodecs, tc.wantVideoCodecs)
			assertAll(t, "audio codec", got.AudioCodecs, tc.wantAudioCodecs)
			for _, absent := range tc.absentVideo {
				if slices.Contains(got.VideoCodecs, absent) {
					t.Errorf("video codec %q was derived, and this device has no screen", absent)
				}
			}
			// A zero maximum is "no limit stated". protocolInfo carries no
			// resolution, so inventing one here would make the planner refuse
			// content the device would have played.
			if got.MaxWidth != 0 || got.MaxHeight != 0 || got.MaxBitrateBPS != 0 || got.SupportsHDR {
				t.Errorf("limits were invented from protocolInfo: %+v", got)
			}
		})
	}
}

// TestProfileFeedsThePlanner closes the loop: a derived profile has to produce
// the right decision, not just the right strings.
func TestProfileFeedsThePlanner(t *testing.T) {
	t.Parallel()

	var envelope struct {
		Sink string `xml:"Body>GetProtocolInfoResponse>Sink"`
	}
	if err := xml.Unmarshal(fixture(t, "samsung-qn85b-protocolinfo.xml"), &envelope); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	tv := playback.ProfileFromProtocolInfo(envelope.Sink)
	local := []playback.Replica{{PeerID: "peer-1", Local: true}}

	tests := []struct {
		name  string
		media playback.MediaProfile
		want  playback.Decision
	}{
		{
			name: "an h264 mp4 plays as it is",
			media: playback.MediaProfile{
				Known: true, Container: "mov,mp4,m4a,3gp,3g2,mj2",
				VideoCodec: "h264", AudioCodec: "aac", Width: 1920, Height: 1080,
			},
			want: playback.DecisionDirect,
		},
		{
			// The case the wildcard entries decide. ffprobe reports Matroska as
			// "matroska,webm" and the device declares "mkv"; containerAliases
			// bridges them, and video/x-mkv is why "mkv" is in the profile.
			name: "an hevc mkv plays as it is",
			media: playback.MediaProfile{
				Known: true, Container: "matroska,webm",
				VideoCodec: "hevc", AudioCodec: "ac3", Width: 3840, Height: 2160,
			},
			want: playback.DecisionDirect,
		},
		{
			name: "a codec the television never mentions is transcoded",
			media: playback.MediaProfile{
				Known: true, Container: "matroska,webm",
				VideoCodec: "av1", AudioCodec: "opus", Width: 3840, Height: 2160,
			},
			want: playback.DecisionTranscode,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := playback.Choose(tc.media, tv, local)
			if plan.Decision != tc.want {
				t.Fatalf("decision = %q (reasons %+v), want %q", plan.Decision, plan.Reasons, tc.want)
			}
		})
	}
}

func TestFetchProfileOverHTTP(t *testing.T) {
	t.Parallel()

	body := fixture(t, "devialet-phantom2-protocolinfo.xml")
	var gotAction string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAction = r.Header.Get("SOAPAction")
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	rend := Renderer{ConnectionManager: Service{
		Type:       "urn:schemas-upnp-org:service:ConnectionManager:2",
		ControlURL: srv.URL,
	}}
	profile, err := FetchProfile(context.Background(), srv.Client(), rend)
	if err != nil {
		t.Fatalf("FetchProfile: %v", err)
	}
	// The version in the SOAPAction has to match the service, or a renderer
	// advertising :2 rejects the call.
	const wantAction = `"urn:schemas-upnp-org:service:ConnectionManager:2#GetProtocolInfo"`
	if gotAction != wantAction {
		t.Errorf("SOAPAction = %s, want %s", gotAction, wantAction)
	}
	if !slices.Contains(profile.Containers, "flac") {
		t.Errorf("containers = %v, want flac among them", profile.Containers)
	}
}

func TestSOAPFaultIsReported(t *testing.T) {
	t.Parallel()

	// 714 is what a renderer answers when it will not accept the content type
	// it was handed, and it is the fault this system is most likely to meet.
	const fault = `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">` +
		`<s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring>` +
		`<detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">` +
		`<errorCode>714</errorCode><errorDescription>Illegal MIME-type</errorDescription>` +
		`</UPnPError></detail></s:Fault></s:Body></s:Envelope>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(fault))
	}))
	defer srv.Close()

	rend := Renderer{ConnectionManager: Service{
		Type:       "urn:schemas-upnp-org:service:ConnectionManager:1",
		ControlURL: srv.URL,
	}}
	_, err := FetchProfile(context.Background(), srv.Client(), rend)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"714", "Illegal MIME-type"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLocationOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		datagram string
		want     string
	}{
		{
			name: "a well-formed reply",
			datagram: "HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=1800\r\n" +
				"LOCATION: http://192.0.2.61:9197/dmr\r\n" +
				"SERVER: Samsung-Linux/4.1, UPnP/1.0\r\n\r\n",
			want: "http://192.0.2.61:9197/dmr",
		},
		{
			name:     "a NOTIFY with no location",
			datagram: "NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1900\r\n\r\n",
			want:     "",
		},
		{name: "an empty datagram", datagram: "", want: ""},
		{name: "a status line and nothing else", datagram: "HTTP/1.1 200 OK\r\n\r\n", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := locationOf([]byte(tc.datagram)); got != tc.want {
				t.Errorf("locationOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// assertAll reports every missing value rather than the first, because a
// mapping change usually drops several at once and fixing them one test run at
// a time is slow.
func assertAll(t *testing.T, kind string, got, want []string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%s %q missing; derived %v", kind, w, got)
		}
	}
}
