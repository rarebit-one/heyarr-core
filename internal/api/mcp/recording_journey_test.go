package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/providers/musicbrainz"
)

// Exercise both public MCP calls and the real provider registry with only the
// external MusicBrainz HTTP response substituted. The selected ID must survive
// the caller's choice into the persisted want; two same-name recordings ensure
// the test does not accidentally depend on the first hit.
func TestRecordingDiscoveryToWant(t *testing.T) {
	t.Parallel()
	const selected = "22222222-2222-4222-8222-222222222222"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/recording/" || !strings.Contains(r.URL.Query().Get("query"), `artist:"Cyndi Lauper"`) || !strings.Contains(r.URL.Query().Get("query"), `recording:"Time After Time"`) {
			t.Errorf("unexpected MusicBrainz request: %s", r.URL.String())
		}
		fmt.Fprint(w, `{"recordings":[{"id":"11111111-1111-4111-8111-111111111111","title":"Time After Time","disambiguation":"live","artist-credit":[{"name":"Cyndi Lauper"}]},{"id":"22222222-2222-4222-8222-222222222222","title":"Time After Time","disambiguation":"studio","artist-credit":[{"name":"Cyndi Lauper"}]}]}`)
	}))
	defer upstream.Close()
	mb, err := musicbrainz.New(musicbrainz.Options{Name: "musicbrainz", Endpoint: upstream.URL, MinInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	reg := providers.New(nil)
	if err := reg.Register(mb); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, false, reg)
	var discovered struct {
		Results []struct {
			Title          string `json:"title"`
			RecordingID    string `json:"recording_id"`
			Disambiguation string `json:"disambiguation"`
			Artists        []struct {
				Name string `json:"name"`
			} `json:"artists"`
		} `json:"results"`
	}
	h.call("", "discover_content", `{"query":"Time After Time","artist":"Cyndi Lauper"}`).structured(t, &discovered)
	if len(discovered.Results) != 2 {
		t.Fatalf("discovered: %+v", discovered)
	}
	picked := discovered.Results[1]
	if picked.RecordingID != selected || picked.Disambiguation != "studio" || len(picked.Artists) != 1 {
		t.Fatalf("choice: %+v", picked)
	}
	params, err := json.Marshal(map[string]string{"title": picked.Title, "artist": picked.Artists[0].Name, "recording_id": picked.RecordingID, "content_type": "music", "quality_profile": "living-room"})
	if err != nil {
		t.Fatal(err)
	}
	var item struct {
		ID          string `json:"id"`
		WorkID      string `json:"work_id"`
		Acquisition *struct {
			State string `json:"state"`
		} `json:"acquisition"`
	}
	h.call("", "want_content", string(params)).structured(t, &item)
	if item.ID == "" || item.WorkID == "" || item.Acquisition == nil || item.Acquisition.State != "MISSING" {
		t.Fatalf("want: %+v", item)
	}
	var key, attrs string
	if err := h.db.Reader().QueryRowContext(context.Background(), `SELECT work_key, attributes FROM works WHERE id = ?`, item.WorkID).Scan(&key, &attrs); err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(attrs), &stored); err != nil {
		t.Fatal(err)
	}
	if key != "music-recording:"+selected || stored["recording_id"] != selected || stored["artist"] != "Cyndi Lauper" {
		t.Fatalf("stored work: key=%q attrs=%s", key, attrs)
	}
}
