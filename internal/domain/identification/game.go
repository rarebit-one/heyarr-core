package identification

import (
	"regexp"
	"strconv"
	"strings"
)

// A GOG offline installer set: "setup_<slug>_<version>_(<build>).exe" plus
// "…-1.bin", "…-2.bin" for the data parts. Optional parenthesised tags such as
// "(64bit)" sit between the version and the build; the build id is always the
// LAST parenthesised number, so the body capture is greedy.
var reGOGWindowsInstaller = regexp.MustCompile(`(?i)^setup_(.+)_\((\d+)\)(?:-\d+)?\.(?:exe|bin)$`)

// The older GOG Linux shape: "gog_<slug>_<version>.sh". The newer one
// ("<slug>_<v>_<v>_<build>.sh") cannot be split into a title and a version
// from the name alone, so it is left to the library-scoped rule, which takes
// the title from the directory instead.
var reGOGLinuxInstaller = regexp.MustCompile(`(?i)^gog_(.+)\.sh$`)

// reParenGroup strips the "(64bit)"-style tags out of an installer body.
var reParenGroup = regexp.MustCompile(`\([^)]*\)`)

// reTrailingYear is a curated directory's "(2013)": display metadata, never
// part of a game's key (see gameTitle).
var reTrailingYear = regexp.MustCompile(`^(.*?)\s*\(((?:19|20)\d{2})\)\s*$`)

// gameContainerExts are the extensions a game ships as: installers, disk
// images and archives. They say nothing about content type on their own — a
// movie torrent can carry an `.exe`, a music folder a `.zip` — which is why
// only the library-scoped rule claims them (see gameRules).
var gameContainerExts = newSet(
	".exe", ".msi", ".bin", ".sh", ".appimage", ".dmg", ".pkg", ".deb", ".rpm",
	".zip", ".7z", ".rar", ".tar", ".tgz", ".gz", ".xz", ".bz2", ".zst", ".iso",
)

// gameArchiveExts are the containers that are archives rather than
// installers; the rest of gameContainerExts install something.
var gameArchiveExts = newSet(".zip", ".7z", ".rar", ".tar", ".tgz", ".gz", ".xz", ".bz2", ".zst", ".iso")

// gameRules identify a game: an installer set, a store depot, or a curated
// directory of files (ADR-0118).
//
// # Two strengths of evidence
//
// A GOG installer name and a Steam depot tree are unambiguous on their own:
// nothing else is named "setup_<slug>_(<build>).exe" or lives under
// "depots/<id>/<id>/". Those rules fire in any library, and are registered
// first so a depot's intro `.mp4` is not claimed as a movie when nothing biases
// the registry.
//
// A bare `.exe` or `.zip`, or the `.pak`/`.dll` data files of an installed
// game, are NOT evidence of a game anywhere but a game library. The rules that claim them are RequiresLibrary:
// inside a `game` library every file under "<Title>/" is that game; outside
// one they never fire, so a fake movie torrent's `setup.exe` stays unidentified
// instead of minting a phantom game Work.
//
// # Filenames describe; they do not certify
//
// The store facts a rule records (store, drm_free, platform, build_id) land on
// the ASSET, as what that one file's name says about it. They are not a claim
// that the store package is complete: a lone "-1.bin", or one `.dll` from a
// depot, says "gog" or "steam" as loudly as the whole set does. The `owned`
// profile therefore judges the EDITION's attributes, which only a store fetch
// that verified the whole package against the store's manifest writes
// (ADR-0118).
//
// # The key is the title alone
//
// A game's work key carries no year. Installer names, depot trees and store
// listings almost never carry one, and a want that names a year while the
// scan cannot would converge on nothing (see Describe). Same-titled remakes
// collide; ADR-0118 records that and what would change it.
func gameRules() []Rule {
	return []Rule{
		{Name: "game/steam-depot", ContentType: Game, Match: matchSteamDepot},
		{Name: "game/gog-installer-dir", ContentType: Game, Match: matchGOGInstallerInDir, RequiresLibrary: true},
		{Name: "game/gog-installer", ContentType: Game, Match: matchGOGInstaller},
		{Name: "game/title-dir", ContentType: Game, Match: matchGameTitleDir, RequiresLibrary: true},
	}
}

// gogInstaller is what a GOG installer's own filename says about itself.
type gogInstaller struct {
	slugTitle []string // the product slug as display tokens
	version   string   // "" when the name carries none we can separate
	build     string   // GOG build id; "" for the Linux shape
	platform  string   // windows or linux
}

// parseGOGInstaller recognises the GOG installer shapes by base name.
func parseGOGInstaller(base string) (gogInstaller, bool) {
	if m := reGOGWindowsInstaller.FindStringSubmatch(base); m != nil {
		title, version := splitGameVersion(m[1])
		if len(title) == 0 {
			return gogInstaller{}, false
		}
		return gogInstaller{slugTitle: title, version: version, build: m[2], platform: "windows"}, true
	}
	if m := reGOGLinuxInstaller.FindStringSubmatch(base); m != nil {
		title, version := splitGameVersion(m[1])
		if len(title) == 0 {
			return gogInstaller{}, false
		}
		return gogInstaller{slugTitle: title, version: version, platform: "linux"}, true
	}
	return gogInstaller{}, false
}

// splitGameVersion splits an installer body at its version: the first token
// that starts with a digit and contains a dot ("4.04a", "2.13"). A title
// number like "2077" or "3" has no dot, so "cyberpunk_2077_2.13" keeps its
// whole name. No dotted token means no separable version, and every token is
// title — a stable answer rather than a guess.
func splitGameVersion(body string) (title []string, version string) {
	body = reParenGroup.ReplaceAllString(body, " ")
	toks := strings.FieldsFunc(body, func(r rune) bool { return r == '_' || r == ' ' })
	for i, t := range toks {
		if t[0] >= '0' && t[0] <= '9' && strings.Contains(t, ".") {
			return toks[:i], strings.Join(toks[i:], " ")
		}
	}
	return toks, ""
}

// matchGOGInstaller titles a GOG installer from its own slug. It is the rule
// for an installer found outside a game library, where a directory above it
// is as likely to be "Downloads" as the game.
func matchGOGInstaller(p Path) (Candidate, bool) {
	inst, ok := parseGOGInstaller(p.Base)
	if !ok {
		return Candidate{}, false
	}
	return gogCandidate(strings.Join(inst.slugTitle, " "), inst)
}

// matchGOGInstallerInDir is the same installer inside a game library, titled by
// the game's directory when there is one. That is what keeps an installer and
// the manual beside it on one Work: "Cyberpunk 2077/setup_cyberpunk_2077_…exe"
// and "Cyberpunk 2077/manual.pdf" must converge, and the directory is the name
// a person curated. A DLC installer under "<Game>/dlc/<name>/" lands on the base
// game with its own edition.
func matchGOGInstallerInDir(p Path) (Candidate, bool) {
	inst, ok := parseGOGInstaller(p.Base)
	if !ok {
		return Candidate{}, false
	}
	title := strings.Join(inst.slugTitle, " ")
	if dir := gameDir(p); dir != "" {
		title = dir
	}
	return gogCandidate(title, inst)
}

func gogCandidate(rawTitle string, inst gogInstaller) (Candidate, bool) {
	c, ok := gameCandidate(rawTitle)
	if !ok {
		return Candidate{}, false
	}
	slug := normKey(inst.slugTitle)
	// One edition per installer package and build: a new GOG build is a new
	// edition, and a DLC installer is never folded into the base game's bytes.
	revision := inst.build
	if revision == "" {
		revision = inst.version
	}
	c.EditionKey = strings.Join([]string{"gog", inst.platform, strings.ReplaceAll(slug, " ", "-"), revision}, ":")
	c.EditionType = "installer"
	c.EditionLabel = strings.TrimSpace("GOG " + capitalize(inst.platform) + " " + inst.version)
	c.EditionAttributes = map[string]any{
		"store":    "gog",
		"drm_free": true, // GOG's offline installers are DRM-free by the store's own policy
		"platform": inst.platform,
	}
	if inst.version != "" {
		c.EditionAttributes["version"] = inst.version
	}
	if inst.build != "" {
		c.EditionAttributes["build_id"] = inst.build
	}
	return c, true
}

// matchSteamDepot is a DepotDownloader tree:
// "<Game>/depots/<depot id>/<manifest id>/…". Every file in it, whatever its
// extension, is part of that depot's edition. The title is the directory just
// above "depots"; a tree with nothing above it names no game and is left to an
// acquisition that already knows which Work it is for.
//
// DRM-freeness is deliberately left unset: some Steam builds run without the
// client and many do not, and the depot does not say which (§63 reports it as
// undetermined rather than guessed).
func matchSteamDepot(p Path) (Candidate, bool) {
	dirs := ParsePath(p.Rel).Dirs
	for i := 1; i+2 < len(dirs); i++ {
		if !strings.EqualFold(dirs[i], "depots") || !isDigits(dirs[i+1]) || !isDigits(dirs[i+2]) {
			continue
		}
		c, ok := gameCandidate(dirs[i-1])
		if !ok {
			return Candidate{}, false
		}
		depot, manifest := dirs[i+1], dirs[i+2]
		c.EditionKey = "steam:" + depot + ":" + manifest
		c.EditionType = "depot"
		c.EditionLabel = "Steam depot " + depot
		c.EditionAttributes = map[string]any{
			"store":    "steam",
			"depot_id": depot,
			"build_id": manifest,
		}
		return c, true
	}
	return Candidate{}, false
}

// matchGameTitleDir claims everything else in a game library: any file under
// "<Title>/" is that game, and an installer or archive at the library root is
// titled by its own name. Only ever consulted inside a game library
// (RequiresLibrary).
func matchGameTitleDir(p Path) (Candidate, bool) {
	title := gameDir(p)
	if title == "" {
		if !gameContainerExts[p.Ext] {
			return Candidate{}, false
		}
		title = strings.TrimSuffix(p.Stem, ".tar")
	}
	c, ok := gameCandidate(title)
	if !ok {
		return Candidate{}, false
	}
	switch {
	case gameArchiveExts[p.Ext]:
		c.EditionKey, c.EditionType = "archive", "archive"
	case gameContainerExts[p.Ext]:
		c.EditionKey, c.EditionType = "installer", "installer"
	}
	return c, true
}

// gameDir is the directory that names a game: the first directory of the
// ORIGINAL path, before roleView stepped over extras. A game's tree nests
// ("<Game>/extras/…", "<Game>/dlc/<name>/…", "<Game>/bin/x64/…"), and only the
// outermost directory is the game.
func gameDir(p Path) string {
	dirs := ParsePath(p.Rel).Dirs
	if len(dirs) == 0 {
		return ""
	}
	return dirs[0]
}

// gameCandidate builds the Work half of a game candidate from a raw title.
func gameCandidate(raw string) (Candidate, bool) {
	toks, year := gameTitle(raw)
	key := normKey(toks)
	if key == "" {
		return Candidate{}, false
	}
	return Candidate{
		ContentType:       Game,
		WorkKey:           workKey(Game, key),
		Title:             displayTitle(toks),
		SortTitle:         key,
		Year:              year,
		WorkAttributes:    map[string]any{},
		EditionAttributes: map[string]any{},
	}, true
}

// gameTitle tokenises a game title and lifts off a curated "(2013)". It does
// NOT use parseName: that cuts a title at its first plausible year or release
// token, which would turn "Cyberpunk 2077" into "Cyberpunk".
func gameTitle(raw string) (toks []string, year int) {
	raw = strings.TrimSpace(raw)
	if m := reTrailingYear.FindStringSubmatch(raw); m != nil && strings.TrimSpace(m[1]) != "" {
		raw = m[1]
		year, _ = strconv.Atoi(m[2])
	}
	return tokenize(raw), year
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
