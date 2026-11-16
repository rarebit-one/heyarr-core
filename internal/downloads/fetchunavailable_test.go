package downloads

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Transmission reports a torrent it could not fetch only as a result string.
// The status in it says whether the far end was down for now or the release
// is not there (#670).
func TestATorrentTransmissionCouldNotFetchIsUnavailableOnlyForAnOutage(t *testing.T) {
	cases := []struct {
		result string
		want   bool
	}{
		{"Couldn't fetch torrent: No Response (0)", true},
		{"Couldn't fetch torrent: Request Timeout (408)", true},
		{"Couldn't fetch torrent: Too Many Requests (429)", true},
		{"Couldn't fetch torrent: Bad Gateway (502)", true},
		{"Couldn't fetch torrent: Not Found (404)", false},
		{"Couldn't fetch torrent: Forbidden (403)", false},
		{"invalid or corrupt torrent file", false},
		{"duplicate torrent", false},
	}
	for _, tc := range cases {
		t.Run(tc.result, func(t *testing.T) {
			if got := fetchUnavailable(tc.result); got != tc.want {
				t.Errorf("fetchUnavailable(%q) = %v, want %v", tc.result, got, tc.want)
			}

			// And through the real call: the add the grab makes carries
			// the mark (or does not) all the way out.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"result":"` + tc.result + `","arguments":{}}`))
			}))
			defer srv.Close()
			_, err := clientFor(t, srv.URL).Add(context.Background(),
				secret.Value("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"))
			if !errors.Is(err, ErrRPCFailure) {
				t.Fatalf("Add = %v, want ErrRPCFailure", err)
			}
			if got := errors.Is(err, providers.ErrUnavailable); got != tc.want {
				t.Errorf("Add: ErrUnavailable = %v, want %v: %v", got, tc.want, err)
			}
		})
	}
}

// A client that is down — the connection is refused — is unavailable for now,
// and the add says so (#670). Real: the server is started and then closed.
func TestAnAddAgainstADownClientIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	_, err := clientFor(t, url).Add(context.Background(),
		secret.Value("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"))
	if err == nil {
		t.Fatal("an add against a closed port succeeded")
	}
	if !errors.Is(err, providers.ErrUnavailable) {
		t.Errorf("Add = %v, want it marked ErrUnavailable", err)
	}
}
