package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/client"
)

func TestParsePosition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    string
		want  float64
		wantE bool
	}{
		{name: "bare seconds", in: "90", want: 90},
		{name: "zero", in: "0", want: 0},
		{name: "fractional seconds", in: "12.5", want: 12.5},
		// The three spellings of the same place. A person reads mm:ss off a
		// screen; a script computes seconds.
		{name: "mm:ss", in: "1:30", want: 90},
		{name: "h:mm:ss", in: "0:01:30", want: 90},
		{name: "over an hour", in: "1:02:03", want: 3723},
		{name: "padded", in: "01:02:03", want: 3723},
		{name: "surrounding space", in: "  90  ", want: 90},

		{name: "empty", in: "", wantE: true},
		{name: "negative", in: "-5", wantE: true},
		{name: "negative component", in: "1:-30", wantE: true},
		{name: "words", in: "halfway", wantE: true},
		{name: "too many parts", in: "1:2:3:4", wantE: true},
		{name: "empty component", in: "1::30", wantE: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parsePosition(tc.in)
			if tc.wantE {
				if err == nil {
					t.Fatalf("parsePosition(%q) = %v, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePosition(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parsePosition(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestFormatPosition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		seconds float64
		want    string
	}{
		{seconds: 0, want: "0:00:00"},
		{seconds: 90, want: "0:01:30"},
		{seconds: 3723, want: "1:02:03"},
		{seconds: 12.9, want: "0:00:12"},
	}
	for _, tc := range tests {
		if got := formatPosition(tc.seconds); got != tc.want {
			t.Errorf("formatPosition(%v) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

// TestEmitStatusOmitsAnUnreportedPosition pins the distinction between "at the
// start" and "the device would not say". Printing 0:00:00 for the second is
// how a resume ends up starting a film over.
func TestEmitStatusOmitsAnUnreportedPosition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     client.RendererStatus
		wantHas    []string
		wantAbsent []string
	}{
		{
			name:       "no position reported",
			status:     client.RendererStatus{Renderer: "Samsung", State: "PLAYING"},
			wantHas:    []string{"Samsung", "PLAYING"},
			wantAbsent: []string{"position"},
		},
		{
			name:    "elapsed only",
			status:  client.RendererStatus{Renderer: "Samsung", State: "PLAYING", Elapsed: 90},
			wantHas: []string{"position", "0:01:30"},
			// No duration means no "of X" — a renderer that has not parsed the
			// stream yet reports none, and inventing one would be a lie.
			wantAbsent: []string{" of "},
		},
		{
			name:    "elapsed and duration",
			status:  client.RendererStatus{Renderer: "Samsung", State: "PLAYING", Elapsed: 90, Duration: 3723},
			wantHas: []string{"0:01:30 of 1:02:03"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := emitStatus(&buf, tc.status, false); err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.wantHas {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output is missing %q:\n%s", want, buf.String())
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(buf.String(), absent) {
					t.Errorf("output should not contain %q:\n%s", absent, buf.String())
				}
			}
		})
	}
}
