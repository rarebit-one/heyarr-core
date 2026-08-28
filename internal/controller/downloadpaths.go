package controller

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/rarebit-one/heyarr-core/internal/config"
	"github.com/rarebit-one/heyarr-core/internal/providers"
	"github.com/rarebit-one/heyarr-core/internal/storagefabric/cas"
)

// Startup checks on where a download client puts things (§58, ADR-0014).
//
// # Where Heyarr should beat the prior art
//
// The *arr ecosystem documents both of these failures at length and then lets
// an operator configure their way into either one silently. You find out about
// the first from a full disk and about the second from a scan that ingested
// half a file.
//
// Heyarr already has cas.SameFilesystem and the warning
// warnIfIngestWillCopy prints for library roots. Extending both to the
// download path is a handful of lines and turns the ecosystem's most common
// operational failures into a startup line, before a single byte moves.

// checkDownloadPaths validates every download provider's path mapping against
// the store and the libraries.
//
// Returns an error only for the arrangement that cannot work at all. Everything
// else is a warning, in ADR-0014's idiom: "degrades to a copy with a warning,
// never an error".
func checkDownloadPaths(cfg config.Config, resolved []providers.Resolved, log *slog.Logger) error {
	for _, r := range resolved {
		if !r.Enabled || !hasDownload(r.Capabilities) {
			continue
		}
		for _, m := range r.PathMap {
			local := filepath.Clean(strings.TrimSpace(m.Local))
			if local == "" {
				continue
			}
			if err := refuseDownloadInsideALibrary(r.Name, local, cfg); err != nil {
				return err
			}
			warnIfDownloadWillCopy(r.Name, local, cfg.CAS.Root, log)
		}
	}
	return nil
}

// refuseDownloadInsideALibrary is a hard error, and one of the few here.
//
// A scanner pointed at a directory the download client is actively writing into
// will meet partial files. M1's acceptance already asserts a partial download
// is never ingested — its hash describes bytes nobody wanted — but relying on
// that catch means every incomplete transfer takes a trip through hashing to be
// rejected, and a rename mid-scan is a race nobody should be running.
//
// It is refused rather than warned about because there is no configuration in
// which it is what somebody meant. The two directories have opposite jobs: one
// is written by something outside Heyarr's control, the other is Heyarr's
// record of what it holds.
func refuseDownloadInsideALibrary(provider, local string, cfg config.Config) error {
	for _, lib := range cfg.Libraries {
		for _, root := range lib.Roots {
			cleanRoot := filepath.Clean(root)
			if !within(local, cleanRoot) && !within(cleanRoot, local) {
				continue
			}
			return fmt.Errorf(
				"provider %q: the download path %s overlaps the library root %s — "+
					"a scan of that root will meet files the download client is still "+
					"writing, so they would be hashed and rejected on every pass; "+
					"put downloads in a sibling directory instead",
				provider, local, cleanRoot)
		}
	}
	return nil
}

// warnIfDownloadWillCopy says so when ingesting from the download path cannot
// hardlink.
//
// The same reasoning as warnIfIngestWillCopy, applied to the other end. Both
// cheap rungs of ADR-0014's ladder need the source and the destination in ONE
// mount — reflink because cloning is a filesystem operation, hardlink because
// link(2) returns EXDEV across mounts whatever the device — so a download
// directory on a different mount from the store means every acquisition is a
// full byte copy, silently, one file at a time.
//
// A warning rather than an error: it works, it is merely expensive, and an
// operator who genuinely has two mounts should not be prevented from running
// Heyarr. What they should not be is uninformed. It asks SameMount, not
// SameFilesystem, for the #222 reason: one device can be two mounts.
func warnIfDownloadWillCopy(provider, local, casRoot string, log *slog.Logger) {
	same, known, err := cas.SameMount(casRoot, local)
	if err != nil {
		// Not fatal. The download directory may not exist yet — the client
		// creates it on first use — and a startup check is the wrong place to
		// insist on it.
		log.Debug("could not compare the download path and the content store",
			"provider", provider, "path", local, "cas_root", casRoot, "error", err)
		return
	}
	if !known || same {
		return
	}
	log.Warn("ingesting from this download path will COPY every file rather than share its bytes",
		"provider", provider,
		"path", local,
		"cas_root", casRoot,
		"why", "the download path and the content store are in different mounts (they may "+
			"even be on the same filesystem), and reflink and hardlink cannot cross a mount",
		"cost", "every acquisition will consume a second full copy of itself",
		"fix", "put the download path and cas.root in the same mount, not merely the same filesystem")
}

// within reports whether child is inside parent.
//
// Boundary-aware: "/data/media" is not inside "/data/med", which a plain prefix
// test would say it was — and that is the same mistake PathMap.Resolve guards
// against, arriving at a different layer.
func within(child, parent string) bool {
	if child == parent {
		return true
	}
	return strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+string(filepath.Separator))
}

func hasDownload(caps []providers.Capability) bool {
	for _, c := range caps {
		if c == providers.CapabilityDownload {
			return true
		}
	}
	return false
}
