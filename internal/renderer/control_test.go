package renderer

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recorder is a renderer that records what it was told and answers whatever it
// was given. Enough to pin the wire format, which is the part real hardware
// judges us on.
type recorder struct {
	srv     *httptest.Server
	actions []string
	bodies  []string
	reply   string
	status  int
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	rec := &recorder{status: http.StatusOK, reply: emptyReply}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.actions = append(rec.actions, r.Header.Get("SOAPAction"))
		rec.bodies = append(rec.bodies, string(body))
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		w.WriteHeader(rec.status)
		_, _ = w.Write([]byte(rec.reply))
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

const emptyReply = `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body></s:Body></s:Envelope>`

func (rec *recorder) controller(t *testing.T, version string) *Controller {
	t.Helper()
	c, err := NewController(rec.srv.Client(), Renderer{
		FriendlyName: "test renderer",
		AVTransport: Service{
			Type:       "urn:schemas-upnp-org:service:AVTransport:" + version,
			ControlURL: rec.srv.URL,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStartLoadsThenPlays(t *testing.T) {
	t.Parallel()

	rec := newRecorder(t)
	c := rec.controller(t, "1")
	if err := c.Start(context.Background(), "http://peer/render/tok/stream.mp4", "Big Buck Bunny", "video/mp4"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Order is not negotiable: a Play with no URI loaded is UPnP error 701.
	want := []string{
		`"urn:schemas-upnp-org:service:AVTransport:1#SetAVTransportURI"`,
		`"urn:schemas-upnp-org:service:AVTransport:1#Play"`,
	}
	if len(rec.actions) != 2 || rec.actions[0] != want[0] || rec.actions[1] != want[1] {
		t.Fatalf("actions = %v, want %v", rec.actions, want)
	}

	load := rec.bodies[0]
	for _, fragment := range []string{
		"<InstanceID>0</InstanceID>",
		"http://peer/render/tok/stream.mp4",
		"Big Buck Bunny",
		"object.item.videoItem",
	} {
		if !strings.Contains(load, fragment) {
			t.Errorf("SetAVTransportURI body is missing %q", fragment)
		}
	}
	// Metadata is XML inside an XML element, so it must arrive escaped —
	// a renderer handed raw angle brackets sees a malformed envelope.
	if !strings.Contains(load, "&lt;DIDL-Lite") {
		t.Error("DIDL-Lite was not escaped into CurrentURIMetaData")
	}
	// Speed is "1" and not "1.0"; some renderers compare it literally.
	if !strings.Contains(rec.bodies[1], "<Speed>1</Speed>") {
		t.Errorf("Play body = %s", rec.bodies[1])
	}
}

// TestServiceVersionIsEchoed guards the difference between the two devices
// this was written against: the Samsung advertises AVTransport:1 and the
// Devialet AVTransport:2, and a renderer rejects an action sent as the wrong
// one.
func TestServiceVersionIsEchoed(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"1", "2"} {
		t.Run("AVTransport:"+version, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(t)
			if err := rec.controller(t, version).Pause(context.Background()); err != nil {
				t.Fatalf("Pause: %v", err)
			}
			want := `"urn:schemas-upnp-org:service:AVTransport:` + version + `#Pause"`
			if rec.actions[0] != want {
				t.Errorf("SOAPAction = %s, want %s", rec.actions[0], want)
			}
			if !strings.Contains(rec.bodies[0], `xmlns:u="urn:schemas-upnp-org:service:AVTransport:`+version+`"`) {
				t.Errorf("body namespace is not version %s: %s", version, rec.bodies[0])
			}
		})
	}
}

func TestSeekSendsRelTime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		to   time.Duration
		want string
	}{
		{name: "the start", to: 0, want: "0:00:00"},
		{name: "under a minute", to: 42 * time.Second, want: "0:00:42"},
		{name: "over an hour", to: 3*time.Hour + 4*time.Minute + 5*time.Second, want: "3:04:05"},
		{name: "a negative target is clamped", to: -time.Minute, want: "0:00:00"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(t)
			if err := rec.controller(t, "1").Seek(context.Background(), tc.to); err != nil {
				t.Fatalf("Seek: %v", err)
			}
			// REL_TIME is an absolute offset within the track. ABS_TIME is a
			// different thing and would silently do nothing on many devices.
			if !strings.Contains(rec.bodies[0], "<Unit>REL_TIME</Unit>") {
				t.Error("Seek did not use REL_TIME")
			}
			if !strings.Contains(rec.bodies[0], "<Target>"+tc.want+"</Target>") {
				t.Errorf("target not %s: %s", tc.want, rec.bodies[0])
			}
		})
	}
}

func TestPositionParsesWhatDevicesActuallySend(t *testing.T) {
	t.Parallel()

	reply := func(rel, dur, uri string) string {
		return `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
			`<u:GetPositionInfoResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">` +
			`<RelTime>` + rel + `</RelTime><TrackDuration>` + dur + `</TrackDuration>` +
			`<TrackURI>` + uri + `</TrackURI>` +
			`</u:GetPositionInfoResponse></s:Body></s:Envelope>`
	}

	tests := []struct {
		name         string
		rel, dur     string
		wantElapsed  time.Duration
		wantDuration time.Duration
	}{
		{name: "ordinary", rel: "0:01:23", dur: "1:30:00", wantElapsed: 83 * time.Second, wantDuration: 90 * time.Minute},
		{name: "fractional seconds are truncated", rel: "0:00:12.500", dur: "0:00:30", wantElapsed: 12 * time.Second, wantDuration: 30 * time.Second},
		{
			// A legal answer that real devices give, and not a reason to fail
			// a progress poll.
			name: "NOT_IMPLEMENTED", rel: "NOT_IMPLEMENTED", dur: "NOT_IMPLEMENTED",
		},
		{name: "empty", rel: "", dur: ""},
		{name: "MM:SS", rel: "02:30", dur: "10:00", wantElapsed: 150 * time.Second, wantDuration: 10 * time.Minute},
		{
			// What the Samsung reported before it had parsed the stream — the
			// reading that made a finished playback look like a failed one.
			name: "all zeroes", rel: "0:00:00", dur: "0:00:00",
		},
		{name: "nonsense", rel: "later", dur: "soon"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(t)
			rec.reply = reply(tc.rel, tc.dur, "http://peer/render/tok")
			got, err := rec.controller(t, "1").Position(context.Background())
			if err != nil {
				t.Fatalf("Position: %v", err)
			}
			if got.Elapsed != tc.wantElapsed {
				t.Errorf("Elapsed = %v, want %v", got.Elapsed, tc.wantElapsed)
			}
			if got.Duration != tc.wantDuration {
				t.Errorf("Duration = %v, want %v", got.Duration, tc.wantDuration)
			}
			if got.URI != "http://peer/render/tok" {
				t.Errorf("URI = %q", got.URI)
			}
		})
	}
}

func TestStateAndPlaying(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state       TransportState
		wantPlaying bool
	}{
		{state: StatePlaying, wantPlaying: true},
		// TRANSITIONING counts. A Samsung sits here while it switches away
		// from whatever app is on screen; treating it as "not playing"
		// reports a failure for a playback that is about to start.
		{state: StateTransitioning, wantPlaying: true},
		{state: StatePausedPlayback, wantPlaying: false},
		{state: StateStopped, wantPlaying: false},
		{state: StateNoMediaPresent, wantPlaying: false},
	}

	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			t.Parallel()
			rec := newRecorder(t)
			rec.reply = `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
				`<u:GetTransportInfoResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">` +
				`<CurrentTransportState>` + string(tc.state) + `</CurrentTransportState>` +
				`</u:GetTransportInfoResponse></s:Body></s:Envelope>`
			got, err := rec.controller(t, "1").State(context.Background())
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			if got != tc.state {
				t.Fatalf("State = %q, want %q", got, tc.state)
			}
			if got.Playing() != tc.wantPlaying {
				t.Errorf("Playing() = %v, want %v", got.Playing(), tc.wantPlaying)
			}
		})
	}
}

// TestPlayWithoutMediaReportsTheUPnPError is the fault a renderer returns when
// Play arrives with nothing loaded. It has to surface as something an operator
// can act on rather than "500".
func TestPlayWithoutMediaReportsTheUPnPError(t *testing.T) {
	t.Parallel()

	rec := newRecorder(t)
	rec.status = http.StatusInternalServerError
	rec.reply = `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">` +
		`<s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring>` +
		`<detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">` +
		`<errorCode>701</errorCode><errorDescription>Transition not available</errorDescription>` +
		`</UPnPError></detail></s:Fault></s:Body></s:Envelope>`

	err := rec.controller(t, "1").Play(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"701", "Transition not available", "Play"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestNewControllerRefusesARendererItCannotDrive(t *testing.T) {
	t.Parallel()

	if _, err := NewController(nil, Renderer{FriendlyName: "a speaker with no transport"}); err == nil {
		t.Fatal("want an error for a renderer with no AVTransport")
	}
}

func TestDIDLClassFollowsTheMediaType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mime string
		want string
	}{
		{mime: "video/mp4", want: "object.item.videoItem"},
		{mime: "audio/mpeg", want: "object.item.audioItem.musicTrack"},
		// A speaker handed videoItem may refuse it outright, so the class has
		// to follow the type rather than defaulting to video.
		{mime: "audio/flac", want: "object.item.audioItem.musicTrack"},
		{mime: "", want: "object.item.videoItem"},
	}

	for _, tc := range tests {
		t.Run(tc.mime, func(t *testing.T) {
			t.Parallel()
			if got := didl("http://peer/x", "t", tc.mime); !strings.Contains(got, tc.want) {
				t.Errorf("didl(%q) has no %s: %s", tc.mime, tc.want, got)
			}
		})
	}
}
