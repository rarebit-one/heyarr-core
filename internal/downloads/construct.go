package downloads

import (
	"fmt"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// Constructor builds the download clients this package implements, for
// providers.BuildWith.
//
// It lives here rather than in the registry because internal/providers cannot
// import this package — this one imports IT, for the Provider contract. That
// cycle is the interface boundary working rather than an accident of layering,
// and the injected constructor is how the two are wired together by whoever
// owns both: the worker and the controller.
//
// Returning handled=false for a kind it does not implement means several
// constructors compose, and an unrecognised kind still falls through to the
// registry's honest "configured, not implemented" report.
func Constructor(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if r.Kind == providers.KindFake && isDownloadOnly(r) {
		return fakeFromConfig(r)
	}
	if r.Kind == providers.KindHTTP {
		return httpFromConfig(r, now)
	}
	if r.Kind == providers.KindYtDlp {
		return ytdlpFromConfig(r, now)
	}
	if r.Kind == providers.KindWebCapture {
		return webCaptureFromConfig(r, now)
	}
	if r.Kind != providers.KindTransmission && r.Kind != providers.KindQBittorrent {
		return nil, false, nil
	}

	// Configuration validated the SHAPE of the mapping; this converts it into
	// the ordered form and is where a mapping that is wrong in a way only this
	// package knows about would be caught. Both checks exist because
	// configuration cannot import this package either.
	maps := make([]Mapping, 0, len(r.PathMap))
	for _, m := range r.PathMap {
		maps = append(maps, Mapping{Remote: m.Remote, Local: m.Local})
	}
	pathMap, err := ParsePathMap(r.Name, maps)
	if err != nil {
		return nil, true, err
	}

	endpoint := ""
	if r.Endpoint != nil {
		endpoint = r.Endpoint.String()
	}

	// The credential comes out of its wrapper in credentialFor, which is the
	// one place in this package where it does.
	user, pass := credentialFor(r)

	// Both torrent clients are configured the same way; only the constructor
	// differs, because the difference between them is the wire, not the config.
	if r.Kind == providers.KindQBittorrent {
		client, err := NewQBittorrent(QBOptions{
			Name:         r.Name,
			Endpoint:     endpoint,
			Username:     user,
			Password:     pass,
			PathMap:      pathMap,
			Label:        r.Label,
			Capabilities: r.Capabilities,
			Now:          now,
		})
		if err != nil {
			return nil, true, err
		}
		return client, true, nil
	}

	client, err := New(Options{
		Name:         r.Name,
		Endpoint:     endpoint,
		Password:     pass,
		Username:     user,
		PathMap:      pathMap,
		Label:        r.Label,
		Capabilities: r.Capabilities,
		Now:          now,
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

// credentialFor is where the RPC credential leaves its wrapper.
//
// # This used to be a parser, and that was the bug
//
// Transmission's RPC is HTTP basic auth, so it needs a username as well as a
// password. Before ADR-0031 a provider Entry carried one credential field, so
// #102 packed both into it as "user:pass" with the username defaulting to
// "transmission" — and split on the first colon to get them back out.
//
// A password containing a colon was therefore silently cut in half: Heyarr
// authenticated as the wrong user with a truncated password, got a 401, and
// reported an unreachable download client. The configuration was correct and
// nothing said otherwise.
//
// The credential is now TYPED by the provider's declared auth scheme, so there
// is nothing here to parse. Basic() either yields the pair the operator wrote,
// byte for byte, or reports that this provider does not have a basic
// credential at all.
func credentialFor(r providers.Resolved) (user, pass string) {
	username, password, ok := r.Credential.Basic()
	if !ok {
		// A Transmission entry always resolves to a basic credential, so this
		// is unreachable through Constructor's kind check. It is not an error
		// because a wrong-shaped credential is a configuration mistake that
		// providers.Validate already refuses at startup; reaching here would
		// mean something built a Resolved by hand, and the safe reading of a
		// credential we cannot use is "there is none".
		return "", ""
	}
	// The credential is revealed exactly here, at the point it is handed to
	// the thing that must send it. Reveal() greps cleanly, which is the whole
	// argument for the Secret type.
	return username, password.Reveal()
}

// simulatedTransferSize is what a configured fake download client produces.
//
// 256 KiB: large enough that the file is streamed and hashed rather than
// living in one buffer, small enough that a demo which fetches several does not
// notice. It is not configurable because nothing has needed it to be, and a
// knob nobody turns is a knob that goes untested.
const simulatedTransferSize = 256 * 1024

// isDownloadOnly reports whether this fake is a download client rather than an
// indexer.
//
// providers.Fake serves the indexer side — it answers Search from configured
// offers — and this package's Fake serves the download side, writing real bytes
// to a real directory. A fake declaring BOTH is left to providers.Fake, because
// taking it here would silently remove its Search.
func isDownloadOnly(r providers.Resolved) bool {
	var download bool
	for _, c := range r.Capabilities {
		if c == providers.CapabilityIndexer {
			return false
		}
		if c == providers.CapabilityDownload {
			download = true
		}
	}
	return download
}

// fakeFromConfig builds the in-process download client the acceptance demo
// needs (#247).
//
// # Why this exists at all
//
// downloads.Fake is production code rather than a test fixture, and its own doc
// says why: "putting it in a _test.go file would mean the demo could not reach
// it, and the demo is the thing that proves the claim on a real machine over a
// real socket". Until this constructor, THE DEMO COULD NOT REACH IT — only the
// transmission kind had a constructor, and that needs a daemon. So the reason it
// is not in a _test.go file was not served by anything.
//
// The consequence was narrow and specific: the demo's full-arc section reached
// SELECTED and then adopted bytes by hand through POST /acquisitions, which is
// §65's exceptional path. Everything between "a release was chosen" and "bytes
// arrived" was unproven end to end, which is exactly where #225's hole was.
//
// # The download directory is the path map's local side
//
// Not a new configuration key. A download client's path map already says where
// its completed files land as this host sees them, which is precisely what this
// fake needs to write into — and reusing it means the demo configures a fake
// exactly as it configures the real client.
func fakeFromConfig(r providers.Resolved) (providers.Provider, bool, error) {
	if len(r.PathMap) == 0 {
		// Refused rather than defaulted to a temp directory. A fake writing
		// somewhere nobody named would produce files the scanner never walks
		// and an ingest that cannot find them, reported as a transfer that
		// completed and vanished.
		return nil, true, fmt.Errorf(
			"provider %q: a fake download client needs a path_map so it has somewhere "+
				"to write completed transfers", r.Name)
	}
	dir := r.PathMap[0].Local
	if dir == "" {
		return nil, true, fmt.Errorf(
			"provider %q: the first path_map entry has no local path", r.Name)
	}
	return NewFake(r.Name, dir).Simulate(simulatedTransferSize), true, nil
}

// httpFromConfig builds the plain-HTTP download client (§58).
//
// Like the fake, its download directory is the path map's local side rather
// than a new configuration key: a download client's path map already says where
// its completed files land as this host sees them, and for a client that IS
// this host that is exactly where the fetch must write. A client with no
// path_map has nowhere to put a completed transfer, so it is refused at
// construction rather than defaulting to a directory the scanner never walks.
func httpFromConfig(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if len(r.PathMap) == 0 {
		return nil, true, fmt.Errorf(
			"provider %q: an http download client needs a path_map so it has somewhere "+
				"to write completed transfers", r.Name)
	}
	dir := r.PathMap[0].Local
	if dir == "" {
		return nil, true, fmt.Errorf(
			"provider %q: the first path_map entry has no local path", r.Name)
	}
	client, err := NewHTTP(HTTPOptions{
		Name:  r.Name,
		Dir:   dir,
		Label: r.Label,
		Now:   now,
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

// ytdlpFromConfig builds the yt-dlp download client (§58, M12 Phase 3).
//
// Like the http client its download directory is the path map's local side
// rather than a new configuration key: yt-dlp writes the finished video to this
// host's disk, and a client with no path_map has nowhere to put a completed
// transfer, so it is refused at construction rather than defaulting to a
// directory the scanner never walks. It takes no endpoint and no credential — a
// public video is fetched by running the tool, not by reaching a configured
// service.
func ytdlpFromConfig(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if len(r.PathMap) == 0 {
		return nil, true, fmt.Errorf(
			"provider %q: a yt-dlp download client needs a path_map so it has somewhere "+
				"to write completed transfers", r.Name)
	}
	dir := r.PathMap[0].Local
	if dir == "" {
		return nil, true, fmt.Errorf(
			"provider %q: the first path_map entry has no local path", r.Name)
	}
	client, err := NewYtDlp(YtDlpOptions{
		Name:  r.Name,
		Dir:   dir,
		Label: r.Label,
		Now:   now,
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}

// webCaptureFromConfig builds the web-capture download client (§58, M12 Phase 4).
//
// Its download directory is the path map's local side, like the http and yt-dlp
// clients: the captured single-file HTML is written to this host's disk, and a
// client with no path_map has nowhere to put a completed capture, so it is
// refused at construction rather than defaulting to a directory the scanner never
// walks. It takes no endpoint and no credential — an article is captured by
// fetching its page, not by reaching a configured service.
func webCaptureFromConfig(r providers.Resolved, now func() time.Time) (providers.Provider, bool, error) {
	if len(r.PathMap) == 0 {
		return nil, true, fmt.Errorf(
			"provider %q: a web-capture download client needs a path_map so it has somewhere "+
				"to write completed captures", r.Name)
	}
	dir := r.PathMap[0].Local
	if dir == "" {
		return nil, true, fmt.Errorf(
			"provider %q: the first path_map entry has no local path", r.Name)
	}
	client, err := NewWebCapture(WebCaptureOptions{
		Name:  r.Name,
		Dir:   dir,
		Label: r.Label,
		Now:   now,
	})
	if err != nil {
		return nil, true, err
	}
	return client, true, nil
}
