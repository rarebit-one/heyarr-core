package replication

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

func TestDecodeHeadsBoundaries(t *testing.T) {
	const limit = 128
	const valid = `{"heads":["a","b"],"space_id":"space-a"}`
	tests := []struct {
		name      string
		body      string
		want      []string
		wantError bool
		oversized bool
	}{
		{name: "ordinary", body: valid, want: []string{"a", "b"}},
		{name: "empty heads", body: `{"heads":[]}`},
		{name: "at limit", body: valid + strings.Repeat(" ", limit-len(valid)), want: []string{"a", "b"}},
		{name: "over limit after valid JSON", body: valid + strings.Repeat(" ", limit-len(valid)+1), wantError: true, oversized: true},
		{name: "over limit inside JSON", body: `{"heads":["` + strings.Repeat("a", limit) + `"]}`, wantError: true, oversized: true},
		{name: "truncated", body: `{"heads":["a",`, wantError: true},
		{name: "malformed", body: `{"heads":!}`, wantError: true},
		{name: "wrong type", body: `{"heads":[3]}`, wantError: true},
		{name: "second document", body: valid + `{}`, wantError: true},
		{name: "trailing garbage", body: valid + `x`, wantError: true},
		{name: "empty response", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Fragment reads as a transport may: limits must not depend on read size
			// or on the HTTP peer declaring Content-Length.
			got, err := decodeHeads(iotest.HalfReader(strings.NewReader(tt.body)), limit)
			if (err != nil) != tt.wantError {
				t.Fatalf("got %v, error %v; want error %v", got, err, tt.wantError)
			}
			if tt.oversized && (err == nil || !strings.Contains(err.Error(), "heads response exceeds 128 bytes")) {
				t.Fatalf("overflow was not explicit: %v", err)
			}
			if err != nil && got != nil {
				t.Fatalf("returned partial frontier on error: %v", got)
			}
			if err == nil && !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecodeHeadsStopsReadingAtLimit(t *testing.T) {
	const limit = 128
	body := strings.NewReader(strings.Repeat(" ", limit*4))
	if _, err := decodeHeads(body, limit); err == nil {
		t.Fatal("accepted oversized body")
	}
	if consumed := limit*4 - body.Len(); consumed != limit+1 {
		t.Fatalf("consumed %d bytes, want %d", consumed, limit+1)
	}
}

func TestDecodeHeadsReadFailure(t *testing.T) {
	boom := errors.New("transport failed")
	body := io.MultiReader(strings.NewReader(`{"heads":[]}`), iotest.ErrReader(boom))
	got, err := decodeHeads(body, 128)
	if !errors.Is(err, boom) || got != nil {
		t.Fatalf("got %v, error %v; want no heads and transport failure", got, err)
	}
}
