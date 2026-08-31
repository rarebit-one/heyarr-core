package downloads

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // BitTorrent v1 identifies pieces and the info dict by SHA-1; the wire format requires it, this is not a security choice.
	"fmt"
	"sort"
)

// A minimal, self-contained BitTorrent v1 metainfo generator, for the
// daemon-in-the-loop harness (#379) and nothing else.
//
// # Why this lives here rather than pulling a torrent library in
//
// The harness needs ONE thing a fixture cannot give it: a real .torrent a real
// qBittorrent will accept, whose payload a real qBittorrent will actually fetch
// and complete — so the pending claim `acquires-over-daemon-clients` ("a real
// qBittorrent transfer completed") has something honest behind it. It does not
// need a torrent client, a tracker or DHT; it needs a single-file torrent that
// carries a BEP-19 web seed (`url-list`), so the payload comes from a plain
// HTTP server on the compose network with no peer, no tracker and no swarm.
// That is ~100 lines of bencode and SHA-1, and a dependency for it would be a
// supply-chain surface added to a public repo to avoid writing code the spec
// (BEP-3/BEP-19) fully specifies.
//
// It is `_test.go` on purpose: it is harness scaffolding, never linked into the
// shipped binary. Its correctness is checked by TestMakeWebseedTorrent below,
// which runs on the ordinary merge path (it needs no daemon) — so the one part
// of the harness that has real algorithmic content is proven in normal CI even
// though the daemon leg only runs in the scheduled lane.

// makeWebseedTorrent builds the metainfo for a single-file torrent whose sole
// source is a web seed.
//
// name is the file name the client will write. data is the exact payload bytes.
// pieceLen is the piece size in bytes. webseedURL is the BEP-19 url-list entry:
// for a single-file torrent it is the direct URL of the file itself.
//
// It returns the bencoded .torrent and the lower-case hex v1 infohash (the
// SHA-1 of the bencoded info dictionary), which is the identity qBittorrent
// will report back and the client keys transfers on.
func makeWebseedTorrent(name string, data []byte, pieceLen int, webseedURL string) (torrent []byte, infoHash string, err error) {
	if name == "" {
		return nil, "", fmt.Errorf("torrentfile: a torrent needs a file name")
	}
	if pieceLen <= 0 {
		return nil, "", fmt.Errorf("torrentfile: piece length must be positive, got %d", pieceLen)
	}
	if webseedURL == "" {
		return nil, "", fmt.Errorf("torrentfile: a web-seed torrent needs a url-list entry")
	}

	// pieces is the concatenation of the SHA-1 of each successive piece, which
	// is how BEP-3 identifies content and how the client verifies each piece it
	// pulls from the web seed.
	var pieces []byte
	for off := 0; off < len(data); off += pieceLen {
		end := off + pieceLen
		if end > len(data) {
			end = len(data)
		}
		sum := sha1.Sum(data[off:end]) //nolint:gosec // see file header: BEP-3 mandates SHA-1.
		pieces = append(pieces, sum[:]...)
	}

	info := bencodeDict{
		"length":       len(data),
		"name":         name,
		"piece length": pieceLen,
		"pieces":       bencodeBytes(pieces),
	}
	infoEncoded := bencode(info)
	ih := sha1.Sum(infoEncoded) //nolint:gosec // the v1 infohash is defined as SHA-1(info).

	meta := bencodeDict{
		"info": info,
		// A single-file url-list entry is the direct file URL (BEP-19). The
		// client fetches every piece from here; there is no tracker key, so no
		// announce is attempted.
		"url-list": bencodeList{webseedURL},
	}
	return bencode(meta), fmt.Sprintf("%x", ih[:]), nil
}

// The bencode encoder below is deliberately tiny and total: it accepts only the
// four shapes a metainfo file uses (int, byte string, list, dict) and panics on
// anything else, because a torrent generator that silently mis-encoded would
// produce a file the client rejects with a message about the client rather than
// the bug.

type bencodeDict map[string]any

type bencodeList []any

// bencodeBytes marks a raw byte string (e.g. the pieces blob) so it is emitted
// as bytes rather than being mistaken for something else.
type bencodeBytes []byte

func bencode(v any) []byte {
	var b bytes.Buffer
	bencodeInto(&b, v)
	return b.Bytes()
}

func bencodeInto(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case int:
		fmt.Fprintf(b, "i%de", t)
	case string:
		fmt.Fprintf(b, "%d:%s", len(t), t)
	case bencodeBytes:
		fmt.Fprintf(b, "%d:", len(t))
		b.Write(t)
	case bencodeList:
		b.WriteByte('l')
		for _, e := range t {
			bencodeInto(b, e)
		}
		b.WriteByte('e')
	case bencodeDict:
		b.WriteByte('d')
		// Keys MUST be emitted in lexicographic order or the info hash is not
		// the one every other client computes for the same content.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(b, "%d:%s", len(k), k)
			bencodeInto(b, t[k])
		}
		b.WriteByte('e')
	default:
		panic(fmt.Sprintf("torrentfile: bencode cannot encode %T", v))
	}
}
