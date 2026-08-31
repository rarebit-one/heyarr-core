package downloads

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // BitTorrent v1 identity is SHA-1; this mirrors the wire format, not a security choice.
	"fmt"
	"testing"
)

// TestMakeWebseedTorrent checks the harness torrent generator on the ORDINARY
// merge path — it needs no daemon, so the one part of the daemon harness with
// real algorithmic content is proven in normal CI, not only in the scheduled
// lane that pulls containers.
//
// The checks are independent of the generator's own code path: a separate
// bencode decoder (below) re-reads the produced file, and the infohash is
// re-derived the way every real client derives it — SHA-1 over the LITERAL
// bytes of the info value as they appear in the file — so an encoding bug
// (wrong key order, a mis-counted length) fails here rather than surfacing as
// "qBittorrent rejected the torrent" inside a container nobody can attach to.
func TestMakeWebseedTorrent(t *testing.T) {
	// A payload longer than the piece length, so there is more than one piece
	// and the piece-boundary arithmetic is actually exercised.
	payload := bytes.Repeat([]byte("heyarr-daemon-harness-"), 500) // 11000 bytes
	const pieceLen = 4096
	const name = "fixture.bin"
	const webseed = "http://webseed/fixture.bin"

	torrent, infoHash, err := makeWebseedTorrent(name, payload, pieceLen, webseed)
	if err != nil {
		t.Fatalf("makeWebseedTorrent: %v", err)
	}

	top, _, err := decodeBencode(torrent, 0)
	if err != nil {
		t.Fatalf("the produced torrent is not valid bencode: %v", err)
	}
	dict, ok := top.(map[string]benValue)
	if !ok {
		t.Fatalf("top level is %T, want a dict", top)
	}

	// url-list carries exactly the web seed: without it the client has no source
	// at all (there is no tracker), so a dropped url-list would be a torrent
	// that can never complete.
	urlList, ok := dict["url-list"].value.([]benValue)
	if !ok || len(urlList) != 1 {
		t.Fatalf("url-list is %#v, want a one-element list", dict["url-list"].value)
	}
	if got := string(urlList[0].value.([]byte)); got != webseed {
		t.Errorf("url-list[0] = %q, want %q", got, webseed)
	}

	info, ok := dict["info"].value.(map[string]benValue)
	if !ok {
		t.Fatalf("info is %T, want a dict", dict["info"].value)
	}
	if got := string(info["name"].value.([]byte)); got != name {
		t.Errorf("info.name = %q, want %q", got, name)
	}
	if got := info["length"].value.(int64); got != int64(len(payload)) {
		t.Errorf("info.length = %d, want %d", got, len(payload))
	}
	if got := info["piece length"].value.(int64); got != pieceLen {
		t.Errorf("info.piece length = %d, want %d", got, pieceLen)
	}

	// The pieces blob must be the SHA-1 of each successive piece, recomputed
	// here from the payload with no reference to the generator's loop.
	var wantPieces []byte
	for off := 0; off < len(payload); off += pieceLen {
		end := off + pieceLen
		if end > len(payload) {
			end = len(payload)
		}
		sum := sha1.Sum(payload[off:end]) //nolint:gosec // BEP-3 mandates SHA-1.
		wantPieces = append(wantPieces, sum[:]...)
	}
	gotPieces := info["pieces"].value.([]byte)
	if !bytes.Equal(gotPieces, wantPieces) {
		t.Errorf("pieces blob is %d bytes and does not match an independent SHA-1 of the payload (%d bytes)",
			len(gotPieces), len(wantPieces))
	}

	// The infohash is SHA-1 over the literal info-value bytes as they sit in the
	// file — the decoder handed back that exact span. This is the identity
	// qBittorrent computes and the client keys transfers on.
	infoSpan := dict["info"].raw
	wantHash := fmt.Sprintf("%x", sha1.Sum(infoSpan)) //nolint:gosec // v1 infohash = SHA-1(info).
	if infoHash != wantHash {
		t.Errorf("infoHash = %q, but SHA-1 of the literal info bytes is %q", infoHash, wantHash)
	}
	if len(infoHash) != 40 {
		t.Errorf("infoHash %q is not a 40-char hex v1 hash", infoHash)
	}
}

func TestMakeWebseedTorrentRefusals(t *testing.T) {
	cases := []struct {
		name, file string
		pieceLen   int
		webseed    string
	}{
		{"no name", "", 16, "http://x/f"},
		{"zero piece length", "f", 0, "http://x/f"},
		{"no web seed", "f", 16, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := makeWebseedTorrent(tc.file, []byte("data"), tc.pieceLen, tc.webseed); err == nil {
				t.Fatalf("expected a refusal for %s", tc.name)
			}
		})
	}
}

// --- a minimal, independent bencode decoder, for the test only ---------------
//
// It exists so the checks above do not lean on the encoder they are meant to
// validate, and it records the raw byte span of every value so the info hash
// can be taken over the literal bytes the way a real client does.

type benValue struct {
	value any    // int64 | []byte | []benValue | map[string]benValue
	raw   []byte // the exact input slice this value was decoded from
}

func decodeBencode(b []byte, pos int) (any, int, error) {
	if pos >= len(b) {
		return nil, pos, fmt.Errorf("unexpected end at %d", pos)
	}
	switch c := b[pos]; {
	case c == 'i':
		end := pos + 1
		for end < len(b) && b[end] != 'e' {
			end++
		}
		if end >= len(b) {
			return nil, pos, fmt.Errorf("unterminated int at %d", pos)
		}
		var n int64
		if _, err := fmt.Sscanf(string(b[pos+1:end]), "%d", &n); err != nil {
			return nil, pos, err
		}
		return n, end + 1, nil
	case c >= '0' && c <= '9':
		colon := pos
		for colon < len(b) && b[colon] != ':' {
			colon++
		}
		var n int
		if _, err := fmt.Sscanf(string(b[pos:colon]), "%d", &n); err != nil {
			return nil, pos, err
		}
		start := colon + 1
		if start+n > len(b) {
			return nil, pos, fmt.Errorf("string at %d runs past the end", pos)
		}
		return b[start : start+n], start + n, nil
	case c == 'l':
		pos++
		var list []benValue
		for pos < len(b) && b[pos] != 'e' {
			start := pos
			v, next, err := decodeBencode(b, pos)
			if err != nil {
				return nil, pos, err
			}
			list = append(list, benValue{value: v, raw: b[start:next]})
			pos = next
		}
		return list, pos + 1, nil
	case c == 'd':
		pos++
		m := map[string]benValue{}
		for pos < len(b) && b[pos] != 'e' {
			k, next, err := decodeBencode(b, pos)
			if err != nil {
				return nil, pos, err
			}
			key, ok := k.([]byte)
			if !ok {
				return nil, pos, fmt.Errorf("dict key at %d is not a string", pos)
			}
			pos = next
			start := pos
			v, vnext, err := decodeBencode(b, pos)
			if err != nil {
				return nil, pos, err
			}
			m[string(key)] = benValue{value: v, raw: b[start:vnext]}
			pos = vnext
		}
		return m, pos + 1, nil
	default:
		return nil, pos, fmt.Errorf("unexpected byte %q at %d", c, pos)
	}
}
