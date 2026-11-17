package acquisition

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"path"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
)

// An operator's direct release (#836): a URL a person hands over as the one
// release for a want, fetched as the payload itself by the http client.
//
// It is ADR-0060's direct release with a third provenance. A feed's enclosure
// and a store's package are recorded whole, without a search, because their
// source already says where the bytes are; an operator who names the file is
// the same case, and nothing in a search would improve on it. The release is
// accepted by that provenance, not ranked against the profile — a URL carries
// no attributes for §62's rules to read.
//
// Only a direct link is accepted. A magnet or a .torrent is a torrent release,
// and turning one into a direct download would fetch the .torrent file as if it
// were the payload — the very misrouting #836 was filed for. Those still come
// to a want the way they always have: from a search, as a candidate.

// OperatorProvider is the provider an operator's direct release is recorded
// under.
const OperatorProvider = "operator"

// ErrNotDirectURL is a hand-over that is not an http(s) URL with a host.
var ErrNotDirectURL = errors.New("a direct release must be an http(s) URL")

// ErrTorrentURL is a magnet or a .torrent link handed over as a direct release.
var ErrTorrentURL = errors.New("a magnet or .torrent link is a torrent release, not a direct one")

// OperatorRelease is the direct release an operator's URL records: protocol
// direct, so the grab offers it only to direct clients whatever their
// configuration order.
//
// The id is a digest of the whole URL, so the same link is the same release
// (and a blocked one stays blocked) while a token in it never reaches the id,
// the API or the event log. The title is the file the URL names, for the same
// reason: the host and query are where a link's credentials live.
func OperatorRelease(raw string) (ReleaseCandidate, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return ReleaseCandidate{}, ErrNotDirectURL
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "magnet" {
		return ReleaseCandidate{}, ErrTorrentURL
	}
	if (scheme != "http" && scheme != "https") || u.Host == "" {
		return ReleaseCandidate{}, ErrNotDirectURL
	}
	if strings.HasSuffix(strings.ToLower(u.Path), ".torrent") {
		return ReleaseCandidate{}, ErrTorrentURL
	}

	sum := sha256.Sum256([]byte(raw))
	title := path.Base(u.Path)
	if title == "/" || title == "." {
		title = u.Hostname()
	}
	if unescaped, err := url.PathUnescape(title); err == nil {
		title = unescaped
	}
	return ReleaseCandidate{
		ID:       "url:" + hex.EncodeToString(sum[:])[:16],
		Title:    title,
		Provider: OperatorProvider,
		Source:   secret.Value(raw),
		Protocol: ProtocolDirect,
	}, nil
}
