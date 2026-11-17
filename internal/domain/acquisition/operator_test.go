package acquisition

import (
	"errors"
	"strings"
	"testing"
)

// An operator's URL is a direct release: routed to direct clients, named by the
// file, identified by a digest that keeps any token in the link off the wire.
func TestAnOperatorsURLIsADirectRelease(t *testing.T) {
	t.Parallel()
	const raw = "https://files.example/roms/Some%20Game%20(Europe).zip?token=s3cret"
	cand, err := OperatorRelease(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cand.Protocol != ProtocolDirect {
		t.Errorf("protocol = %q, want direct", cand.Protocol)
	}
	if cand.Provider != OperatorProvider {
		t.Errorf("provider = %q, want %q", cand.Provider, OperatorProvider)
	}
	if cand.Source.Reveal() != raw {
		t.Errorf("source = %q, want the URL as given", cand.Source.Reveal())
	}
	if cand.Title != "Some Game (Europe).zip" {
		t.Errorf("title = %q, want the file the URL names", cand.Title)
	}
	for _, field := range []string{cand.ID, cand.Title} {
		if strings.Contains(field, "s3cret") || strings.Contains(field, "files.example") {
			t.Errorf("%q carries part of the link's host or query", field)
		}
	}

	again, err := OperatorRelease(" " + raw + " ")
	if err != nil || again.ID != cand.ID {
		t.Errorf("the same link gave id %q, then %q (%v)", cand.ID, again.ID, err)
	}
	other, err := OperatorRelease(strings.Replace(raw, "s3cret", "other", 1))
	if err != nil || other.ID == cand.ID {
		t.Errorf("two different links share the id %q (%v)", cand.ID, err)
	}

	bare, err := OperatorRelease("https://files.example/")
	if err != nil || bare.Title != "files.example" {
		t.Errorf("a URL naming no file is titled %q (%v), want its host", bare.Title, err)
	}
}

// A torrent handed over the same way is refused rather than fetched as a file,
// and so is anything that is not an http(s) URL.
func TestAnOperatorsURLRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw  string
		want error
	}{
		{"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", ErrTorrentURL},
		{"https://tracker.example/download/123/Some.Game.torrent?passkey=abc", ErrTorrentURL},
		{"http://tracker.example/SOME.GAME.TORRENT", ErrTorrentURL},
		{"ftp://files.example/game.zip", ErrNotDirectURL},
		{"files.example/game.zip", ErrNotDirectURL},
		{"https:///game.zip", ErrNotDirectURL},
		{"", ErrNotDirectURL},
		{"http://bad host/%zz", ErrNotDirectURL},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Parallel()
			if _, err := OperatorRelease(tc.raw); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
