// Responses in this file are closed by the harness's t.Cleanup, which
// bodyclose cannot see through — hence the file-wide exemption rather than a
// comment on each call site.
//
//nolint:bodyclose // responses are closed by the harness's t.Cleanup
package resources_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/auth"
	"github.com/rarebit-one/heyarr-core/internal/events"
)

// device is the shape this file asserts against. It is written out here rather
// than imported from the package under test so that a field being renamed shows
// up as a test failure rather than as a silent rename on both sides.
type device struct {
	ID        string `json:"id"`
	DeviceKey string `json:"device_key"`
	Name      string `json:"name"`
	Platform  string `json:"platform"`
	Profile   struct {
		Containers    []string `json:"containers"`
		VideoCodecs   []string `json:"video_codecs"`
		AudioCodecs   []string `json:"audio_codecs"`
		MaxWidth      int64    `json:"max_width"`
		MaxHeight     int64    `json:"max_height"`
		MaxBitrateBPS int64    `json:"max_bitrate_bps"`
		SupportsHDR   bool     `json:"supports_hdr"`
	} `json:"profile"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
	LastSeenAt string `json:"last_seen_at"`
}

const livingRoom = `{
	"device_key": "tv-living-room",
	"name": "Living Room",
	"platform": "tvos",
	"profile": {
		"containers": ["mp4", "mkv"],
		"video_codecs": ["h264", "hevc"],
		"audio_codecs": ["aac", "eac3"],
		"max_width": 3840,
		"max_height": 2160,
		"max_bitrate_bps": 120000000,
		"supports_hdr": true
	}
}`

func (h *harness) registerDevice(t *testing.T, body string) (*http.Response, device) {
	t.Helper()
	resp := h.do(http.MethodPost, "/api/v1/devices", "", strings.NewReader(body))
	var d device
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		if err := json.Unmarshal(h.body(resp), &d); err != nil {
			t.Fatalf("the response is not a device: %v", err)
		}
	}
	return resp, d
}

// Registration is an upsert, and this is the assertion the table's whole shape
// exists for: an app announces itself on every launch, and a row per launch is
// how these tables end up with four thousand devices called "Living Room".
func TestRegisteringTheSameDeviceTwiceConvergesOnOneRow(t *testing.T) {
	h := newHarness(t)

	first, created := h.registerDevice(t, livingRoom)
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first registration = %d, want 201", first.StatusCode)
	}
	if loc := first.Header.Get("Location"); loc == "" {
		t.Error("a created device has no Location header")
	}

	second, again := h.registerDevice(t, strings.Replace(livingRoom, `"Living Room"`, `"Lounge"`, 1))
	if second.StatusCode != http.StatusOK {
		t.Fatalf("re-registration = %d, want 200 — a second POST must not create a second device",
			second.StatusCode)
	}
	if again.ID != created.ID {
		t.Errorf("re-registration produced id %q, want the original %q", again.ID, created.ID)
	}
	if again.Name != "Lounge" {
		t.Errorf("name = %q, want the updated one", again.Name)
	}
	if again.CreatedAt != created.CreatedAt {
		t.Errorf("created_at moved on re-registration: %q then %q", created.CreatedAt, again.CreatedAt)
	}

	var page struct {
		Items []device `json:"items"`
	}
	if err := json.Unmarshal(h.body(h.get("/api/v1/devices")), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("the collection holds %d devices after two registrations of one device", len(page.Items))
	}
}

// Invariant 7 with the emphasis on "transition". A re-registration that changes
// nothing is not one, and emitting for it would make every app launch in the
// house an event.
func TestDeviceEventsFireOnChangeAndNotOnEveryLaunch(t *testing.T) {
	h := newHarness(t)

	h.registerDevice(t, livingRoom)
	assertLastDeviceEvent(t, h, events.TypeDeviceRegistered, 1)

	// Byte-identical re-registration: no transition, no event.
	h.registerDevice(t, livingRoom)
	assertLastDeviceEvent(t, h, events.TypeDeviceRegistered, 1)

	// A changed profile is a transition.
	h.registerDevice(t, strings.Replace(livingRoom, `"supports_hdr": true`, `"supports_hdr": false`, 1))
	assertLastDeviceEvent(t, h, events.TypeDeviceUpdated, 2)

	// And the same change again is not.
	h.registerDevice(t, strings.Replace(livingRoom, `"supports_hdr": true`, `"supports_hdr": false`, 1))
	assertLastDeviceEvent(t, h, events.TypeDeviceUpdated, 2)
}

// assertLastDeviceEvent checks the total count of playback.device.* events and
// the type of the most recent one.
func assertLastDeviceEvent(t *testing.T, h *harness, wantType string, wantCount int) {
	t.Helper()
	evs, err := h.events.Since(t.Context(), 0, []string{"playback.device.*"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != wantCount {
		types := make([]string, 0, len(evs))
		for _, e := range evs {
			types = append(types, e.Type)
		}
		t.Fatalf("device events = %v (%d), want %d", types, len(evs), wantCount)
	}
	if evs[len(evs)-1].Type != wantType {
		t.Errorf("the most recent device event is %q, want %q", evs[len(evs)-1].Type, wantType)
	}
}

// A profile that cannot describe a real device is a 400, not a stored row.
// Every one of these is a refusal that would otherwise reach the planner as
// nonsense and produce a decision nobody could explain.
func TestMalformedDeviceProfilesAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"no device key", `{"name":"x"}`, "device_key is required"},
		{"no name", `{"device_key":"k"}`, "name is required"},
		{
			"negative bitrate",
			`{"device_key":"k","name":"x","profile":{"max_bitrate_bps":-1}}`,
			"max_bitrate_bps must not be negative",
		},
		{
			"negative width",
			`{"device_key":"k","name":"x","profile":{"max_width":-1,"max_height":-1}}`,
			"max_width must not be negative",
		},
		{
			"an absurd resolution",
			`{"device_key":"k","name":"x","profile":{"max_width":4294967295,"max_height":4294967295}}`,
			"is not a real resolution",
		},
		{
			// Half a limit is worse than none: the planner would have to guess
			// what the other half means, and guessing wrong transcodes
			// something that did not need to.
			"half a resolution limit",
			`{"device_key":"k","name":"x","profile":{"max_width":1920}}`,
			"must be given together",
		},
		{
			"an empty codec name",
			`{"device_key":"k","name":"x","profile":{"video_codecs":["h264",""]}}`,
			"contains an empty entry",
		},
		{
			"something structured smuggled into a codec list",
			`{"device_key":"k","name":"x","profile":{"video_codecs":["h264 profile=high level=4.1"]}}`,
			"not a codec or container name",
		},
		{
			// Unknown fields are rejected rather than ignored, because the
			// usual cause is a typo: a client that sends the wrong key and
			// gets a 201 has registered a device with the wrong profile and
			// been told it worked.
			"an unknown field",
			`{"device_key":"k","name":"x","device_kind":"tvos"}`,
			"device_kind",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			resp, _ := h.registerDevice(t, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if body := string(h.body(resp)); !strings.Contains(body, tc.want) {
				t.Errorf("the problem document does not say why:\n%s", body)
			}
			var page struct {
				Items []device `json:"items"`
			}
			if err := json.Unmarshal(h.body(h.get("/api/v1/devices")), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 0 {
				t.Errorf("a refused registration stored a row: %+v", page.Items)
			}
		})
	}
}

// Two clients spelling one capability differently must converge, or the
// planner matches one television and not its identical twin.
func TestCodecNamesAreNormalisedAndDeduplicated(t *testing.T) {
	h := newHarness(t)
	_, d := h.registerDevice(t, `{
		"device_key":"k","name":"x",
		"profile":{"video_codecs":["  H264 ","h264","HEVC"]}
	}`)
	got := strings.Join(d.Profile.VideoCodecs, ",")
	if got != "h264,hevc" {
		t.Errorf("video_codecs = %q, want %q", got, "h264,hevc")
	}
}

// A device that can play nothing is a legitimate thing to be. The planner must
// be able to reason about it (the answer is TRANSCODE, or a refusal), so it has
// to be storable.
func TestADeviceThatDeclaresNothingIsAccepted(t *testing.T) {
	h := newHarness(t)
	resp, d := h.registerDevice(t, `{"device_key":"bare","name":"Bare"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	// [] and not null, in every list. A client should not have to handle both
	// for "this device declares no containers".
	raw := string(h.body(h.get("/api/v1/devices/" + d.ID)))
	for _, field := range []string{"containers", "video_codecs", "audio_codecs"} {
		if strings.Contains(raw, `"`+field+`":null`) {
			t.Errorf("%s is null rather than an empty list: %s", field, raw)
		}
	}
}

func TestUnknownDeviceIsA404(t *testing.T) {
	h := newHarness(t)
	if resp := h.get("/api/v1/devices/nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 — an empty 200 is indistinguishable from a device with nothing in it",
			resp.StatusCode)
	}
}

// Registering is a write; reading is a read. Asserted rather than assumed,
// because the scope on a route is the authorisation contract.
func TestDeviceScopes(t *testing.T) {
	h := newHarness(t, withAuth)
	readOnly := h.mint("reader", auth.ScopeRead)
	writer := h.mint("writer", auth.ScopeRead, auth.ScopeWrite)

	resp := h.do(http.MethodPost, "/api/v1/devices", readOnly.Secret, strings.NewReader(livingRoom))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a read token registered a device: %d", resp.StatusCode)
	}
	resp = h.do(http.MethodPost, "/api/v1/devices", writer.Secret, strings.NewReader(livingRoom))
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("a write token could not register a device: %d", resp.StatusCode)
	}
	if resp := h.do(http.MethodGet, "/api/v1/devices", readOnly.Secret, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("a read token could not list devices: %d", resp.StatusCode)
	}
}
