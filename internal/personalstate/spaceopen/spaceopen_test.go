package spaceopen

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiclient "github.com/rarebit-one/heyarr-core/internal/client"
)

// A rotation that commits between reading the keys (epoch 1) and reading the
// history adds an epoch-2 row. History must drop it, so opening at epoch 1 still
// sees a complete chain instead of refusing a row it cannot open yet.
func TestHistoryDropsRowsAboveTheOpeningEpoch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/key-history") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"space_id":"s","entries":[` +
			`{"epoch":1,"sealed_prev":"AQ==","created_at":"2026-10-08T00:00:00Z"},` +
			`{"epoch":2,"sealed_prev":"Ag==","created_at":"2026-10-08T00:00:01Z"}]}`))
	}))
	t.Cleanup(srv.Close)
	c, err := apiclient.New(apiclient.Options{Addr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	got, err := History(context.Background(), c, "s", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Epoch != 1 {
		t.Fatalf("history at epoch 1 = %+v, want only the epoch-1 row", got)
	}
	if none, err := History(context.Background(), c, "s", 0); err != nil || len(none) != 0 {
		t.Fatalf("history at epoch 0 = %+v, %v; want none", none, err)
	}
}
