package probe_test

import (
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/media/probe"
)

func TestAudioPreferenceSelectsDialogueAndPreservesForeignOriginals(t *testing.T) {
	cases := []struct {
		name                   string
		streams                []probe.Stream
		languages              []string
		wantIndex, wantOrdinal int
		found                  bool
	}{
		{"unknown first", []probe.Stream{{Index: 1, Type: "audio", Language: "und"}, {Index: 2, Type: "audio", Language: "eng"}}, []string{"en"}, 2, 1, true},
		{"untagged first", []probe.Stream{{Index: 1, Type: "audio"}, {Index: 2, Type: "audio", Language: "eng"}}, []string{"en"}, 2, 1, true},
		{"English second", []probe.Stream{{Index: 0, Type: "video"}, {Index: 1, Type: "audio", Language: "ita"}, {Index: 4, Type: "subtitle"}, {Index: 5, Type: "audio", Language: "eng"}}, []string{"en"}, 5, 1, true},
		{"regional and ISO aliases", []probe.Stream{{Index: 3, Type: "audio", Language: "en-US"}}, []string{"eng"}, 3, 0, true},
		{"Chinese original", []probe.Stream{{Index: 1, Type: "audio", Language: "zho"}}, []string{"en"}, 1, 0, true},
		{"explicit preference order", []probe.Stream{{Index: 1, Type: "audio", Language: "eng"}, {Index: 2, Type: "audio", Language: "jpn"}}, []string{"ja", "en"}, 2, 1, true},
		{"commentary excluded", []probe.Stream{{Index: 1, Type: "audio", Language: "eng", Commentary: true}, {Index: 2, Type: "audio", Language: "eng"}}, []string{"en"}, 2, 1, true},
		{"only English commentary", []probe.Stream{{Index: 1, Type: "audio", Language: "eng", Commentary: true}, {Index: 2, Type: "audio", Language: "zho"}}, []string{"en"}, 2, 1, true},
		{"no preference", []probe.Stream{{Index: 1, Type: "audio", Language: "ita"}, {Index: 2, Type: "audio", Language: "eng"}}, nil, 1, 0, true},
		{"no audio", []probe.Stream{{Index: 0, Type: "video"}}, []string{"en"}, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, ordinal, found := (probe.Result{Streams: tc.streams}).AudioForLanguages(tc.languages)
			if found != tc.found || s.Index != tc.wantIndex || ordinal != tc.wantOrdinal {
				t.Fatalf("selected index=%d ordinal=%d found=%v, want %d %d %v", s.Index, ordinal, found, tc.wantIndex, tc.wantOrdinal, tc.found)
			}
		})
	}
}
