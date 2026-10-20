package acquisition

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/rarebit-one/heyarr-core/internal/domain/policy"
	"github.com/rarebit-one/heyarr-core/internal/domain/secret"
)

// GOGSourceScheme tags a release source that a GOG store fetch, not an HTTP or
// torrent client, must take (ADR-0120). It is the same seam the yt-dlp tag is
// (followed.YtDlpSourceScheme): a download client routes on the tag, never on a
// parsed host, so the choice does not depend on registration order.
//
// The rest of the source is the store's own name for the game — lgogdownloader's
// "gamename", the slug GOG uses for the product's files — because that is what
// the store tool takes, and what an owned-library listing reports.
const GOGSourceScheme = "gog:"

// gogGameName is the shape of a GOG gamename: lower-case letters, digits and
// underscores. It is checked rather than trusted because the store tool takes
// the name as a regular expression; anything outside this set is refused, never
// escaped, so no caller can widen a fetch beyond the one game it names.
var gogGameName = regexp.MustCompile(`^[a-z0-9_]{1,128}$`)

// ValidGOGGameName reports whether name is a well-formed GOG gamename.
func ValidGOGGameName(name string) bool { return gogGameName.MatchString(name) }

// GOGSource is the tagged release source for one GOG game.
func GOGSource(gameName string) string { return GOGSourceScheme + gameName }

// gogRelease is a GOG release source: the gamename, then optionally "@" and
// the want the release was recorded for.
var gogRelease = regexp.MustCompile(`^gog:([a-z0-9_]{1,128})(?:@([A-Za-z0-9-]{1,64}))?$`)

// GOGReleaseSource is the release source a store fetch records for one want:
// the game, tagged with the want. Per want, not per game, so each want's
// transfer and acquisition are its own — two profiles wanting one game never
// share (and strand) one acquisition row. Both fetch into the same per-game
// directory, one at a time, so the second costs a verification, not a download.
func GOGReleaseSource(gameName, desiredItemID string) string {
	return GOGSource(gameName) + "@" + desiredItemID
}

// ParseGOGSource returns the gamename a GOG-tagged source names, and whether
// the source is one at all. A tagged source with a malformed name is NOT one:
// the caller refuses it rather than fetching something it cannot name exactly.
func ParseGOGSource(source string) (string, bool) {
	m := gogRelease.FindStringSubmatch(strings.TrimSpace(source))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// StoreFacts are the edition attributes a verified store package earns
// (ADR-0118 §2, ADR-0120). A store release candidate carries them, and ingest
// writes them onto every edition the verified package filled — the one place
// they are allowed to reach the edition. Absent store means the release is not
// a store release.
func StoreFacts(a Attributes) (store string, drmFree, hasDRMFree, ok bool) {
	v, present := a[policy.AttrStore]
	if !present || v.Kind != policy.KindText || strings.TrimSpace(v.Text) == "" {
		return "", false, false, false
	}
	if d, has := a[policy.AttrDRMFree]; has && d.Kind == policy.KindFlag {
		drmFree, hasDRMFree = d.Flag, true
	}
	return strings.TrimSpace(v.Text), drmFree, hasDRMFree, true
}

// StoreFetches is the pacing for store fetches (ADR-0120): attempted at once,
// then — while the want stays unsatisfied — again on a widening cadence, capped,
// never abandoned. Six hours because a failed fetch is usually a login that
// needs renewing or a store outage, not something a minute fixes; a week because
// a game is worth asking for again even when its package keeps failing to
// verify.
func StoreFetches() Schedule {
	return Schedule{Name: "store-fetch", Base: 6 * time.Hour, Max: 7 * 24 * time.Hour}
}

// StoreReleaseCandidate is the direct release a store fetch records for a game:
// the store's own package, tagged so only that store's client takes it, and
// carrying the facts the store will vouch for once every file is verified.
func StoreReleaseCandidate(gameName, title, desiredItemID string) ReleaseCandidate {
	source := GOGReleaseSource(gameName, desiredItemID)
	return ReleaseCandidate{
		ID:       source,
		Title:    title,
		Provider: "gog",
		Source:   secret.Value(source),
		Attributes: Attributes{
			policy.AttrStore: policy.Text("gog"),
			// GOG's offline installers run without its client by the store's
			// own policy.
			policy.AttrDRMFree: policy.Flag(true),
		},
	}
}

// EvaluatePackagePlacement is placement for a verified store package (ADR-0120):
// a game is replicated only when EVERY file of its package is. One installer
// part held at a second site is not a backup of the game, so the package's
// verdict is its worst blob's — satisfied only if all are, otherwise the first
// blob that is not, with how many of the package's files it holds.
//
// replicasOf answers for one blob; it is a function so the caller reads
// replicas once per blob without this package knowing where they live.
func EvaluatePackagePlacement(blobs, required []string, replicasOf func(string) []PeerReplica) PlacementVerdict {
	if len(blobs) == 0 {
		return PlacementVerdict{Satisfaction: SatisfactionUnknown, Detail: "the package lists no files"}
	}
	rank := map[Satisfaction]int{
		SatisfactionNot: 0, SatisfactionUnknown: 1, SatisfactionConverging: 2,
		SatisfactionNotApplicable: 3, SatisfactionSatisfied: 3,
	}
	worst := PlacementVerdict{Satisfaction: SatisfactionSatisfied}
	placed := 0
	for i, blob := range blobs {
		v := EvaluatePlacement(blob, required, replicasOf(blob))
		if v.Satisfaction == SatisfactionSatisfied {
			placed++
		}
		if i == 0 || rank[v.Satisfaction] < rank[worst.Satisfaction] {
			worst = v
		}
	}
	if worst.Satisfaction == SatisfactionSatisfied {
		worst.Detail = fmt.Sprintf("every required peer holds every file of the package (%d files)", len(blobs))
		return worst
	}
	worst.Detail = fmt.Sprintf("%d of %d package files fully placed; worst: %s", placed, len(blobs), worst.Detail)
	return worst
}

// StoreManifestName is the file a store download client writes beside a
// package it verified, naming every file the store vouched for (ADR-0120 §3).
// Ingest checks the transfer against it, because the bytes can change between
// the store's check and the ingest job: a file deleted or truncated in that gap
// must fail the acquisition, not quietly shrink what is recorded as the package.
const StoreManifestName = ".heyarr-store-manifest.json"

// StoreManifest is what the store vouched for.
type StoreManifest struct {
	Store string              `json:"store"`
	Files []StoreManifestFile `json:"files"`
}

// StoreManifestFile is one vouched-for file: its base name and size.
type StoreManifestFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Unmet lists what the manifest names that held (base name → size) lacks, or
// holds at another size. An empty manifest is unmet as a whole: a package the
// store vouched for nothing of is not a verified package.
func (m StoreManifest) Unmet(held map[string]int64) []string {
	if len(m.Files) == 0 {
		return []string{"the manifest names no files"}
	}
	var out []string
	for _, f := range m.Files {
		size, ok := held[f.Name]
		switch {
		case !ok:
			out = append(out, f.Name+" is missing")
		case f.Size > 0 && size != f.Size:
			out = append(out, fmt.Sprintf("%s is %d bytes, the store vouched for %d", f.Name, size, f.Size))
		}
	}
	return out
}
