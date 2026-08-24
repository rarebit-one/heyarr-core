package resources

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/api/render"
	"github.com/rarebit-one/heyarr-core/internal/domain/playback"
	"github.com/rarebit-one/heyarr-core/internal/renderer"
)

// TestPlayingOurs is the guard that stops this feature corrupting itself.
//
// A Samsung returns to whatever was on screen when a DLNA push finishes and
// resumes it. Measured: after a ten-second clip played, the television went
// back to a paused YouTube video and continued it. A poller that trusted
// GetPositionInfo would then write the viewer's progress through YouTube into
// their film's session — a number that decides where playback resumes
// tomorrow, climbing for hours, with the renderer answering honestly the whole
// time.
func TestPlayingOurs(t *testing.T) {
	t.Parallel()

	const ours = "blake3:ec133f8eda39d245d20ea5289aade13b4dcf323facfef406df192fb094f30388"
	const other = "blake3:1111111111111111111111111111111111111111111111111111111111111111"

	// A real capability URL, built the way playback builds one, so the test
	// breaks if the encoding of the digest in the path ever changes.
	token, err := render.Capability{
		BlobHash:  ours,
		ExpiresAt: time.Now().Add(time.Hour),
		MIME:      "video/mp4",
	}.Sign([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	ourURL := "http://192.0.2.4:7777" + render.Path(token) + "/stream.mp4"

	tests := []struct {
		name string
		uri  string
		blob string
		want bool
	}{
		{name: "our own capability URL", uri: ourURL, blob: ours, want: true},
		{
			// THE case. Not a malformed URL, not an error — a perfectly good
			// answer about content that is not ours.
			name: "the television went back to YouTube",
			uri:  "https://www.youtube.com/watch?v=aqz-KE-bpKQ",
			blob: ours, want: false,
		},
		{
			name: "another asset from this same Heyarr",
			uri:  strings.Replace(ourURL, base64.RawURLEncoding.EncodeToString([]byte(ours)), base64.RawURLEncoding.EncodeToString([]byte(other)), 1),
			blob: ours, want: false,
		},
		{
			name: "somebody else's DLNA server",
			uri:  "http://192.0.2.99:8200/MediaItems/412.mp4",
			blob: ours, want: false,
		},
		{
			// A linked asset (ADR-0020) has no blob, so there is nothing to
			// compare and the session must not be killed for it.
			name: "an asset with no blob is never judged",
			uri:  "https://www.youtube.com/watch?v=aqz-KE-bpKQ",
			blob: "", want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := playingOurs(tc.uri, tc.blob); got != tc.want {
				t.Errorf("playingOurs(%q, %q) = %v, want %v", tc.uri, tc.blob, got, tc.want)
			}
		})
	}
}

// TestTheGuardSurvivesReminting is the property that makes the digest — rather
// than the whole URL — the right thing to compare.
//
// A capability is re-minted on every play, and its expiry is part of what is
// signed, so two URLs for the same film never match as strings. Comparing URLs
// would make the guard fire on the second poll of every playback.
func TestTheGuardSurvivesReminting(t *testing.T) {
	t.Parallel()

	const blob = "blake3:ec133f8eda39d245d20ea5289aade13b4dcf323facfef406df192fb094f30388"
	secret := []byte("0123456789abcdef0123456789abcdef")

	first, err := render.Capability{BlobHash: blob, ExpiresAt: time.Unix(1_800_000_000, 0), MIME: "video/mp4"}.Sign(secret)
	if err != nil {
		t.Fatal(err)
	}
	second, err := render.Capability{BlobHash: blob, ExpiresAt: time.Unix(1_800_009_999, 0), MIME: "video/mp4"}.Sign(secret)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two mints produced the same token; this test proves nothing")
	}
	for _, token := range []string{first, second} {
		if !playingOurs("http://peer"+render.Path(token), blob) {
			t.Error("a re-minted capability for the same blob was judged foreign")
		}
	}
}

func TestProgressFrom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pos  renderer.Position
		err  error
		want *playback.Progress
	}{
		{
			name: "an ordinary position",
			pos:  renderer.Position{Elapsed: 83 * time.Second},
			want: &playback.Progress{Locator: "83", Unit: playback.UnitSeconds},
		},
		{
			// A renderer that will not say where it is is playing perfectly
			// well. Recording zero would rewind the session every ten seconds.
			name: "no position reported",
			pos:  renderer.Position{},
		},
		{name: "the device errored", err: errors.New("timeout"), pos: renderer.Position{Elapsed: time.Minute}},
		{
			name: "fractions are truncated to whole seconds",
			pos:  renderer.Position{Elapsed: 12500 * time.Millisecond},
			want: &playback.Progress{Locator: "12", Unit: playback.UnitSeconds},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := progressFrom(tc.pos, tc.err)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %+v, want nil", got)
			case tc.want == nil:
				return
			case got == nil:
				t.Fatalf("got nil, want %+v", tc.want)
			case *got != *tc.want:
				t.Errorf("got %+v, want %+v", *got, *tc.want)
			}
			if got != nil {
				// The locator has to satisfy the domain's own rules, or the
				// transition is refused at the point of writing it.
				if err := got.Validate(); err != nil {
					t.Errorf("the progress this produces is invalid: %v", err)
				}
			}
		})
	}
}
