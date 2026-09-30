package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A HEAD runs the GET handler and net/http drops the body it writes, so the
// access log must not report those bytes as sent (#606).
func TestAccessLogCountsNoBytesForHead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		method string
		want   float64
	}{{http.MethodGet, 64}, {http.MethodHead, 0}} {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			s := &Server{log: slog.New(slog.NewJSONHandler(&buf, nil))}
			h := s.accessLogMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(strings.Repeat("x", 64)))
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tt.method, "/opds", nil))

			var line map[string]any
			if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
				t.Fatalf("access log line %q: %v", buf.String(), err)
			}
			if got := line["bytes"]; got != tt.want {
				t.Errorf("%s logged bytes = %v, want %v", tt.method, got, tt.want)
			}
		})
	}
}
