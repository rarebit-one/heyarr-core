// Response bodies are closed by the t.Cleanup the harness registers in raw().
//
//nolint:bodyclose // closed by the harness's t.Cleanup
package subsonic_test

import (
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/url"
	"strings"
	"testing"
)

func hexOf(s string) string { return hex.EncodeToString([]byte(s)) }

func TestPing(t *testing.T) {
	h := newHarness(t)
	r := h.get("ping", nil)
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok (error: %+v)", r.Status, r.Error)
	}
	if r.Version != "1.16.1" {
		t.Errorf("version = %q, want 1.16.1", r.Version)
	}
	if r.Type != "heyarr" {
		t.Errorf("type = %q, want heyarr", r.Type)
	}
	if r.ServerVersion != "test-server" {
		t.Errorf("serverVersion = %q, want test-server", r.ServerVersion)
	}
	if !r.OpenSubsonic {
		t.Error("openSubsonic should be true")
	}
}

// TestPingXMLDefault proves the default format is XML, and that it carries the
// namespace and OpenSubsonic handshake attributes a strict client parses.
func TestPingXMLDefault(t *testing.T) {
	h := newHarness(t)
	q := h.creds()
	q.Del("f") // default format
	resp := h.raw("ping", q)
	body, _ := io.ReadAll(resp.Body)

	var env struct {
		XMLName       xml.Name `xml:"subsonic-response"`
		Status        string   `xml:"status,attr"`
		Version       string   `xml:"version,attr"`
		Type          string   `xml:"type,attr"`
		ServerVersion string   `xml:"serverVersion,attr"`
		OpenSubsonic  bool     `xml:"openSubsonic,attr"`
	}
	if err := xml.Unmarshal(body, &env); err != nil {
		t.Fatalf("response is not XML: %v\n%s", err, body)
	}
	if env.Status != "ok" || env.Version != "1.16.1" || env.Type != "heyarr" || !env.OpenSubsonic {
		t.Fatalf("unexpected XML envelope: %+v", env)
	}
	if !strings.Contains(string(body), `xmlns="http://subsonic.org/restapi"`) {
		t.Errorf("XML is missing the Subsonic namespace:\n%s", body)
	}
}

func TestAuthMissingPassword(t *testing.T) {
	h := newHarness(t)
	q := url.Values{"u": {"player"}, "c": {"c"}, "v": {"1.16.1"}, "f": {"json"}}
	r := decode(t, h.raw("ping", q))
	if r.Status != "failed" || r.Error == nil || r.Error.Code != 10 {
		t.Fatalf("missing password: got %+v / %+v", r.Status, r.Error)
	}
}

func TestAuthWrongPassword(t *testing.T) {
	h := newHarness(t)
	q := h.creds()
	q.Set("p", "not-a-real-token")
	r := decode(t, h.raw("ping", q))
	if r.Status != "failed" || r.Error == nil || r.Error.Code != 40 {
		t.Fatalf("wrong password: got %+v / %+v", r.Status, r.Error)
	}
}

// TestAuthTokenSchemeRefused proves the salted-token scheme is refused with a
// message that tells the user how to fix it — the adapter cannot recompute the
// MD5 because Heyarr never holds the plaintext token.
func TestAuthTokenSchemeRefused(t *testing.T) {
	h := newHarness(t)
	q := url.Values{
		"u": {"player"}, "t": {"deadbeef"}, "s": {"salt"},
		"c": {"c"}, "v": {"1.16.1"}, "f": {"json"},
	}
	r := decode(t, h.raw("ping", q))
	if r.Status != "failed" || r.Error == nil || r.Error.Code != 40 {
		t.Fatalf("token scheme: got %+v / %+v", r.Status, r.Error)
	}
	if !strings.Contains(strings.ToLower(r.Error.Message), "password") {
		t.Errorf("refusal should name the fix, got %q", r.Error.Message)
	}
}

// TestPasswordHexEncoded proves the enc:<hex> password form is accepted — some
// clients send it so the token never appears verbatim in a URL.
func TestPasswordHexEncoded(t *testing.T) {
	h := newHarness(t)
	q := h.creds()
	q.Set("p", "enc:"+hexOf(h.token))
	r := decode(t, h.raw("ping", q))
	if r.Status != "ok" {
		t.Fatalf("enc password: status %q, error %+v", r.Status, r.Error)
	}
}

func TestGetLicense(t *testing.T) {
	h := newHarness(t)
	r := h.get("getLicense", nil)
	if r.License == nil || !r.License.Valid {
		t.Fatalf("license = %+v, want valid", r.License)
	}
}

// TestOpenSubsonicExtensionsUnauthenticated proves the one handshake endpoint a
// client may call before it has a working credential answers without one, and
// advertises OpenSubsonic support (a present, if empty, extension list).
func TestOpenSubsonicExtensionsUnauthenticated(t *testing.T) {
	h := newHarness(t)
	resp := h.raw("getOpenSubsonicExtensions", url.Values{"f": {"json"}})
	r := decode(t, resp)
	if r.Status != "ok" {
		t.Fatalf("status = %q, want ok", r.Status)
	}
	if r.OpenSubsonicExtensions == nil {
		t.Fatal("openSubsonicExtensions should be present (even if empty)")
	}
}

func TestGetMusicFolders(t *testing.T) {
	h := newHarness(t)
	r := h.get("getMusicFolders", nil)
	if r.MusicFolders == nil {
		t.Fatal("no musicFolders payload")
	}
	if len(r.MusicFolders.MusicFolder) != 1 {
		t.Fatalf("music folders = %+v, want exactly the one music library", r.MusicFolders.MusicFolder)
	}
	if r.MusicFolders.MusicFolder[0].Name != "Music" {
		t.Errorf("folder name = %q, want Music", r.MusicFolders.MusicFolder[0].Name)
	}
}

func TestUnknownMethod(t *testing.T) {
	h := newHarness(t)
	r := h.get("getPodcasts", nil)
	if r.Status != "failed" || r.Error == nil {
		t.Fatalf("unknown method should fail, got %+v", r)
	}
	if !strings.Contains(r.Error.Message, "getPodcasts") {
		t.Errorf("error should name the method, got %q", r.Error.Message)
	}
}
