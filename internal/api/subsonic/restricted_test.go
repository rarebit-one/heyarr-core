// Response bodies are closed by the t.Cleanup the harness registers.
//
//nolint:bodyclose // closed by the harness's t.Cleanup
package subsonic_test

import (
	"context"
	"testing"
)

// A restricted executor token (ADR-0104) is a valid credential, but not for the
// music library: the adapter refuses it as not authorised (50), while the
// ordinary read token keeps working.
func TestARestrictedTokenCannotReadTheLibrary(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	exec, err := h.store.CreateExecutor(context.Background(), "executor", nil)
	if err != nil {
		t.Fatal(err)
	}
	q := h.creds()
	q.Set("p", exec.Secret)
	r := decode(t, h.raw("getArtists", q))
	if r.Status != "failed" || r.Error == nil || r.Error.Code != 50 {
		t.Fatalf("a restricted token read the library: %+v", r)
	}
	if ok := h.get("getArtists", nil); ok.Status != "ok" {
		t.Fatalf("the ordinary read token stopped working: %+v", ok)
	}
}
