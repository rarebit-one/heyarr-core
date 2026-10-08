// Response bodies are closed by the t.Cleanup the harness registers.
//
//nolint:bodyclose // closed by the harness's t.Cleanup
package opds_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/api/opds"
)

// A restricted executor token (ADR-0104) is refused the publication catalogue
// with 403; the ordinary read token keeps working.
func TestARestrictedTokenCannotReadTheCatalogue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	exec, err := h.store.CreateExecutor(context.Background(), "executor", nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := h.token
	h.token = exec.Secret
	if resp := h.get(opds.Prefix, true); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a restricted token read the catalogue: %d", resp.StatusCode)
	}
	h.token = reader
	if resp := h.get(opds.Prefix, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("the ordinary read token stopped working: %d", resp.StatusCode)
	}
}
