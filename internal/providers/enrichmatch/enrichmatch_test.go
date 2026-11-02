package enrichmatch

import "testing"

func TestTokenSetRatio(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want float64 // exact where the arithmetic is clean
		min  float64 // else a lower bound
	}{
		{name: "identical", a: "Kind of Blue", b: "Kind of Blue", want: 1},
		{
			name: "query contained in answer",
			a:    "the almanack of naval ravikant",
			b:    "the almanack of naval ravikant eric jorgenson",
			want: 1, // min(|a|,|b|) is |a|, fully contained
		},
		{name: "order insensitive", a: "blue kind of", b: "kind of blue", want: 1},
		{name: "no overlap", a: "foo bar", b: "baz qux", want: 0},
		{name: "empty", a: "", b: "anything", want: 0},
		{name: "partial", a: "the great gatsby", b: "gatsby", min: 0.9}, // "gatsby" is the only >1-char token in b
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TokenSetRatio(tc.a, tc.b)
			if tc.min > 0 {
				if got < tc.min {
					t.Errorf("TokenSetRatio(%q,%q) = %v, want >= %v", tc.a, tc.b, got, tc.min)
				}
				return
			}
			if got != tc.want {
				t.Errorf("TokenSetRatio(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// Jaccard is the tie-breaker: where TokenSetRatio asks "is the shorter title
// contained in the other", Jaccard also counts the words the other side added,
// so "Alien" prefers "Alien" over "Alien: Covenant".
func TestJaccard(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"Alien", "Alien", 1},
		{"Alien", "Alien: Covenant", 0.5},
		{"The Matrix", "Matrix", 0.5},
		{"Arrival", "Departure", 0},
		{"", "Arrival", 0},
	}
	for _, tc := range cases {
		if got := Jaccard(tc.a, tc.b); got != tc.want {
			t.Errorf("Jaccard(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
	// Containment and Jaccard disagree exactly where the tie-break is needed.
	if TokenSetRatio("Alien", "Alien: Covenant") != 1 {
		t.Error("the shorter title is fully contained, so TokenSetRatio is 1 — Jaccard must be what tells the two apart")
	}
}
