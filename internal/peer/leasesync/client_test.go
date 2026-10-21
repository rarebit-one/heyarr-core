package leasesync

import (
	"slices"
	"strings"
	"testing"
)

// A decoded lease set is authoritative: Refresh replaces the sibling's cached
// set with it. So only exactly one complete object with a `leases` field may
// decode; a truncated, oversized or trailing-data body is a failed fetch that
// keeps the cache.
func TestDecodeLeasesAcceptsExactlyOneCompleteSet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		want    []string
		wantErr string
	}{
		{name: "a set", body: `{"leases":["a","b"]}`, want: []string{"a", "b"}},
		{name: "an empty set", body: `{"leases":[]}`, want: []string{}},
		{name: "trailing whitespace is fine", body: "{\"leases\":[\"a\"]}\n  \n", want: []string{"a"}},
		{name: "trailing garbage", body: `{"leases":[]}garbage`, wantErr: "data after its lease set"},
		{name: "a second object", body: `{"leases":[]}{"leases":["x"]}`, wantErr: "data after its lease set"},
		{name: "no leases field", body: `{}`, wantErr: "without a leases field"},
		{name: "truncated", body: `{"leases":["a",`, wantErr: "decoding the leases"},
		{name: "over the limit", body: `{"leases":["` + strings.Repeat("x", maxLeasesBody) + `"]}`, wantErr: "more than"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeLeases(strings.NewReader(tc.body), "peer-b")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeLeases: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("leases = %q, want %q", got, tc.want)
			}
		})
	}
}
