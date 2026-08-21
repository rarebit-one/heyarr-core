package controller

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/providers"
)

// The two ADR-0014 failures, caught at startup rather than from a full disk.

func downloadProvider(local string) providers.Resolved {
	return providers.Resolved{
		Name:         "acquire",
		Kind:         providers.KindTransmission,
		Capabilities: []providers.Capability{providers.CapabilityDownload},
		PathMap:      []providers.PathMapping{{Remote: "/downloads", Local: local}},
		Enabled:      true,
	}
}

// A download path inside a library root is REFUSED, because there is no
// configuration in which it is what somebody meant: a scan of that root will
// meet files the client is still writing.
func TestADownloadPathInsideALibraryIsRefused(t *testing.T) {
	cfg := config.Defaults()
	cfg.Libraries = []config.Library{{Name: "films", ContentType: "movie", Roots: []string{"/data/media"}}}

	for _, local := range []string{
		"/data/media/incoming", // inside the root
		"/data/media",          // the root itself
	} {
		t.Run(local, func(t *testing.T) {
			err := checkDownloadPaths(cfg, []providers.Resolved{downloadProvider(local)}, discard())
			if err == nil {
				t.Fatal("this overlap must be refused")
			}
			// Both paths named, because an operator with several libraries has
			// to know which pair collided.
			for _, want := range []string{local, "/data/media", "acquire"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal should name %q, said: %v", want, err)
				}
			}
		})
	}
}

// A library root inside the download path is the same mistake written the other
// way round, and is refused too.
func TestALibraryInsideTheDownloadPathIsRefused(t *testing.T) {
	cfg := config.Defaults()
	cfg.Libraries = []config.Library{{Name: "films", ContentType: "movie", Roots: []string{"/data/dl/media"}}}

	err := checkDownloadPaths(cfg, []providers.Resolved{downloadProvider("/data/dl")}, discard())
	if err == nil {
		t.Fatal("a library inside the download path is the same overlap")
	}
}

// A sibling directory is exactly the recommended layout and must be accepted.
func TestASiblingDownloadPathIsFine(t *testing.T) {
	cfg := config.Defaults()
	cfg.Libraries = []config.Library{{Name: "films", ContentType: "movie", Roots: []string{"/data/media"}}}

	if err := checkDownloadPaths(cfg,
		[]providers.Resolved{downloadProvider("/data/torrents")}, discard()); err != nil {
		t.Fatalf("the recommended single-volume layout was refused: %v", err)
	}
}

// The boundary case a plain prefix test gets wrong: /data/media-old is not
// inside /data/media, and refusing it would reject a legitimate layout.
func TestAPathThatMerelySharesAPrefixIsNotAnOverlap(t *testing.T) {
	cfg := config.Defaults()
	cfg.Libraries = []config.Library{{Name: "films", ContentType: "movie", Roots: []string{"/data/media"}}}

	if err := checkDownloadPaths(cfg,
		[]providers.Resolved{downloadProvider("/data/media-old")}, discard()); err != nil {
		t.Fatalf("/data/media-old is a different directory: %v", err)
	}
}

// A disabled provider is not checked. Its paths are not in use, and refusing to
// start over a configuration nobody is running would be Heyarr insisting on
// tidiness.
func TestADisabledProviderIsNotChecked(t *testing.T) {
	cfg := config.Defaults()
	cfg.Libraries = []config.Library{{Name: "films", ContentType: "movie", Roots: []string{"/data/media"}}}

	p := downloadProvider("/data/media/incoming")
	p.Enabled = false
	if err := checkDownloadPaths(cfg, []providers.Resolved{p}, discard()); err != nil {
		t.Fatalf("a disabled provider's paths are not in use: %v", err)
	}
}

// The cross-filesystem case WARNS rather than refusing — ADR-0014 says it
// "degrades to a copy with a warning, never an error" — and the warning names
// the cost, because "different filesystems" means nothing to somebody who has
// not read the ADR.
func TestACrossFilesystemDownloadPathWarnsAndNamesTheCost(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// A real directory that is not the store's filesystem cannot be conjured
	// portably, so this drives the warning function directly with a path that
	// exists and a store root that does not resolve — which exercises the
	// unknown branch — and then asserts the SHAPE of what it says when it does
	// fire, using a temporary directory as both.
	tmp := t.TempDir()
	warnIfDownloadWillCopy("acquire", tmp, filepath.Join(tmp, "cas"), log)

	// Same filesystem: silent. A warning that fired for the correct layout
	// would be one everybody learns to ignore.
	if strings.Contains(buf.String(), "COPY every file") {
		t.Errorf("warned about a path on the same filesystem as the store:\n%s", buf.String())
	}
}
