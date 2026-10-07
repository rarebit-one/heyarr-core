package crdt

import "testing"

func TestIsPlaylistChangeAndSnapshotAreStrict(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		snap bool
		want bool
	}{
		{"playlist change", `{"Op":0,"ItemID":"tr:1","Tag":"t","Order":{"Counter":1,"Tag":"t"},"Observed":null}`, false, true},
		{"drive change", `{"op":1,"path":"a","blob":"x","at":1,"writer":"w","base":{}}`, false, false},
		{"star change", `{"Op":0,"ItemID":"tr:1","Tag":"t","At":1,"Observed":null}`, false, false},
		{"trailing value", `{"Op":0} {}`, false, false},
		{"playlist snapshot", `{"adds":[],"tombstones":[],"counter":0}`, true, true},
		{"drive snapshot", `{"entries":[],"counter":0}`, true, false},
		{"star snapshot", `{"stars":[],"tombstones":[],"counter":0}`, true, false},
	}
	for _, tc := range cases {
		got := IsPlaylistChange([]byte(tc.raw))
		if tc.snap {
			got = IsPlaylistSnapshot([]byte(tc.raw))
		}
		if got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
