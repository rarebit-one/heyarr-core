package httpapi

import (
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJSONCompressionNegotiation(t *testing.T) {
	const body = "catalog work with artwork and a primary asset "
	for _, tt := range []struct {
		name, encoding string
		wantGzip       bool
	}{
		{"no encoding", "", false},
		{"gzip", "br, gzip", true},
		{"gzip refused", "gzip;q=0", false},
		{"gzip weighted", "gzip;q=0.5", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/works", nil)
			if tt.encoding != "" {
				req.Header.Set("Accept-Encoding", tt.encoding)
			}
			recorder := httptest.NewRecorder()
			WriteJSON(recorder, req, slog.New(slog.DiscardHandler), http.StatusOK, map[string]string{"items": strings.Repeat(body, 100)})
			resp := recorder.Result()
			defer func() { _ = resp.Body.Close() }()
			if got := resp.Header.Get("Content-Encoding") == "gzip"; got != tt.wantGzip {
				t.Fatalf("gzip = %t, want %t", got, tt.wantGzip)
			}
			if resp.Header.Get("Vary") != "Accept-Encoding" {
				t.Fatal("Vary must name Accept-Encoding")
			}
			var reader io.Reader = resp.Body
			if tt.wantGzip {
				zipper, err := gzip.NewReader(reader)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = zipper.Close() }()
				reader = zipper
			}
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), body) {
				t.Fatal("response body changed")
			}
		})
	}
}
