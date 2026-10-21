package identification

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ROM identification: a console game's dump, held in a ROM library (ADR-0121).
//
// # A ROM is not a game
//
// A `game` is fetched from a store it is owned on and judged by `owned`
// (ADR-0118). A ROM has no store, so it is searched on indexers like a book and
// judged by its own profile. The two are separate content types because the
// route is chosen per content type (strategy.For): a platform attribute on
// `game` could not send one game to a store and another to an indexer.
//
// # The key is the platform and the title
//
// "Tetris" on the Game Boy and "Tetris" on the NES are different games, and a
// SNES dump and a PC remake must never converge. So a ROM's work key is
// `rom:<platform>:<title>`, with the platform from the closed vocabulary below.
// The platform lives IN the key, not only in the Work's attributes, because a
// fan-out want (ADR-0119) carries the key to a sibling site and creates the
// Work there with empty attributes; the key is what survives.
//
// # Layout
//
// A ROM library is laid out the way ES-DE lays out its ROMs directory: one
// directory per platform, named by the platform's short name, holding files or
// one directory per game (a multi-file disc set). The platform directory is the
// strongest evidence; an extension only one platform uses is the fallback for a
// file at the library root. Every rule needs a ROM library: `.bin`, `.iso` and
// `.zip` are evidence of nothing anywhere else.
//
// # Names describe
//
// No-Intro and Redump name a dump "Title (Region) (Rev 1) (Disc 2)". The
// parenthesised and bracketed tags are lifted off the title and recorded on the
// edition as what the name says. They certify nothing: a hash match against a
// DAT is a later change, and until then no profile gates on them.

// romPlatforms is the closed platform vocabulary for ROMs, ES-DE's system short
// names. The name is what a ROM library's platform directory is called and what
// a want's descriptor names.
var romPlatforms = newSet(
	"atari2600", "atari7800", "lynx",
	"nes", "snes", "n64", "gb", "gbc", "gba", "nds", "n3ds", "virtualboy",
	"gc", "wii", "wiiu", "switch",
	"mastersystem", "megadrive", "segacd", "sega32x", "saturn", "dreamcast", "gamegear",
	"psx", "ps2", "ps3", "psp", "psvita",
	"pcengine", "neogeo", "ngp", "ngpc", "wonderswan", "wonderswancolor",
	"xbox",
)

// ROMPlatforms lists the ROM platform vocabulary, sorted.
func ROMPlatforms() []string {
	out := make([]string, 0, len(romPlatforms))
	for p := range romPlatforms {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// romExtPlatform maps an extension only one platform uses to that platform. A
// shared extension (.bin, .iso, .chd, .cue, .rvz, .zip, .7z) is absent: it says
// "a dump" without saying of what, so it is identified only under a platform
// directory. So is `.md`, Mega Drive's other extension: it is every README's
// too, and a readme in a download must never count as the ROM (.gen, .smd and
// .bin carry Mega Drive dumps instead).
var romExtPlatform = map[string]string{
	".a26": "atari2600", ".a78": "atari7800", ".lnx": "lynx",
	".nes": "nes", ".fds": "nes",
	".sfc": "snes", ".smc": "snes",
	".n64": "n64", ".z64": "n64", ".v64": "n64",
	".gb": "gb", ".gbc": "gbc", ".gba": "gba",
	".nds": "nds", ".3ds": "n3ds", ".cia": "n3ds", ".cci": "n3ds", ".vb": "virtualboy",
	".wbfs": "wii", ".wua": "wiiu", ".wux": "wiiu",
	".nsp": "switch", ".xci": "switch",
	".sms": "mastersystem", ".gen": "megadrive", ".smd": "megadrive", ".32x": "sega32x",
	".gdi": "dreamcast", ".gg": "gamegear",
	".cso": "psp", ".vpk": "psvita",
	".pce": "pcengine", ".ngp": "ngp", ".ngc": "ngpc", ".ws": "wonderswan", ".wsc": "wonderswancolor",
}

// romSharedExts are the dump, disc-image and archive shapes several platforms
// use. Under a platform directory they are that platform's dumps.
var romSharedExts = newSet(
	".bin", ".cue", ".iso", ".img", ".ccd", ".sub", ".chd", ".cdi", ".m3u", ".pbp",
	".rvz", ".gcz", ".ciso", ".rpx",
	".zip", ".7z",
)

// romDescriptorExts are a disc set's supporting files: a cue sheet, a
// playlist, a track list, CloneCD's control and subchannel files (.ccd, .sub).
// They are ingested beside the images they belong to but are no dump
// themselves, so a release of only these holds no game. `.sub` is also a
// subtitle extension; roleView gives it that role in a scan, which keeps it
// from satisfying a want there too.
var romDescriptorExts = newSet(".cue", ".m3u", ".gdi", ".ccd", ".sub")

// ROMExtPlatform is the one platform an extension belongs to, when only one
// platform uses it (".gba" → gba); ok is false for a shared or unknown shape.
func ROMExtPlatform(ext string) (string, bool) {
	p, ok := romExtPlatform[strings.ToLower(ext)]
	return p, ok
}

// ROMPlatformInPath is the first directory of relPath that names a ROM
// platform ("psx" in "Release/psx/Game.bin"), when one does.
func ROMPlatformInPath(relPath string) (string, bool) {
	for _, d := range ParsePath(relPath).Dirs {
		if p := strings.ToLower(d); romPlatforms[p] {
			return p, true
		}
	}
	return "", false
}

// IsROMDump reports whether an asset is dump data a ROM want can be satisfied
// by: a primary file of a ROM shape that is not a disc descriptor. An unknown
// path (relPath "") is judged by its role alone.
func IsROMDump(role, relPath string) bool {
	if role != RolePrimary {
		return false
	}
	if relPath == "" {
		return true
	}
	ext := strings.ToLower(ParsePath(relPath).Ext)
	return IsPrimaryExt(ROM, ext) && !romDescriptorExts[ext]
}

// IsROMDescriptorExt reports whether ext is a disc-set descriptor rather than
// dump data (ADR-0121).
func IsROMDescriptorExt(ext string) bool { return romDescriptorExts[strings.ToLower(ext)] }

func romRules() []Rule {
	return []Rule{
		{Name: "rom/platform-dir", ContentType: ROM, Match: matchROMInPlatformDir, RequiresLibrary: true},
		{Name: "rom/extension", ContentType: ROM, Match: matchROMByExtension, RequiresLibrary: true},
	}
}

// matchROMInPlatformDir identifies "<platform>/<file>" and
// "<platform>/<Game>/<file>": the platform from the directory, the title from
// the game directory when there is one (a disc set's files are one game) and
// from the file's own name otherwise.
func matchROMInPlatformDir(p Path) (Candidate, bool) {
	dirs := ParsePath(p.Rel).Dirs
	if len(dirs) == 0 {
		return Candidate{}, false
	}
	platform := strings.ToLower(dirs[0])
	if !romPlatforms[platform] {
		return Candidate{}, false
	}
	// Companion artwork and subtitles ("snes/<Game> (USA).png",
	// "psx/<Game>/cover.jpg") belong to the ROM beside them, as they do for
	// every other content type; roleView has already given them their role.
	_, own := romExtPlatform[p.Ext]
	aux := isAuxExt(p.Ext)
	if !own && !romSharedExts[p.Ext] && !aux {
		return Candidate{}, false
	}
	name := p.Stem
	if len(dirs) > 1 {
		name = dirs[1]
	}
	c, ok := romCandidate(platform, name, p)
	if ok && aux {
		// A picture is no kind of dump; the edition's type comes from its dumps.
		c.EditionType = ""
	}
	return c, ok
}

// IdentifyROMFile identifies one acquired file as a dump for a platform the
// caller already knows: an acquisition for a ROM want, whose transfer path
// ("<release>/<file>.bin", or a lone ".iso") carries no platform directory for
// the scan rules to read. The platform comes from the want, so a shared
// extension is enough; the edition facts (region, revision, disc, type) still
// come from the file's own name. The library-scan rules stay as strict as they
// are. ok is false for a file that is no ROM or companion shape, or a platform
// outside the vocabulary.
func IdentifyROMFile(platform, relPath string) (Candidate, bool) {
	if !romPlatforms[platform] {
		return Candidate{}, false
	}
	p := ParsePath(relPath)
	role, lang, _ := roleView(p)
	_, own := romExtPlatform[p.Ext]
	aux := isAuxExt(p.Ext)
	if !own && !romSharedExts[p.Ext] && !aux {
		return Candidate{}, false
	}
	c, ok := romCandidate(platform, p.Stem, p)
	if !ok {
		return Candidate{}, false
	}
	if aux {
		c.EditionType = ""
	}
	if c.EditionKey == "" {
		c.EditionKey = "default"
	}
	c.AssetRole, c.Language = role, lang
	c.Source, c.Rule, c.Identified = SourcePathHeuristic, "rom/acquired-file", true
	return c, true
}

// matchROMByExtension identifies a file whose extension only one platform uses,
// wherever it sits in the library.
func matchROMByExtension(p Path) (Candidate, bool) {
	platform, ok := romExtPlatform[p.Ext]
	if !ok {
		return Candidate{}, false
	}
	return romCandidate(platform, p.Stem, p)
}

func romCandidate(platform, name string, p Path) (Candidate, bool) {
	tags := parseROMName(name)
	// In a disc set the directory names the game and the files carry the dump
	// tags ("Final Fantasy VII/Final Fantasy VII (USA) (Rev 1) (Disc 1).bin"),
	// so a tag the directory does not spell out is read from the file.
	file := parseROMName(p.Stem)
	if tags.region == "" {
		tags.region = file.region
	}
	if tags.revision == "" {
		tags.revision = file.revision
	}
	key := normKey(tags.title)
	if key == "" {
		return Candidate{}, false
	}
	edAttrs := map[string]any{"platform": platform}
	if tags.region != "" {
		edAttrs["region"] = tags.region
	}
	if tags.revision != "" {
		edAttrs["revision"] = tags.revision
	}
	// The file's own disc tag, not the game directory's: in a disc set every
	// file is one disc of the same edition.
	if d := file.disc; d != "" {
		edAttrs["disc"] = d
	}
	editionKey := strings.Join(nonEmpty(platform, slug(tags.region), slug(tags.revision)), ":")
	label := strings.Join(nonEmpty(tags.region, tags.revision), " ")
	return Candidate{
		ContentType:       ROM,
		WorkKey:           workKey(ROM, platform, key),
		Title:             displayTitle(tags.title),
		SortTitle:         key,
		WorkAttributes:    map[string]any{"platform": platform},
		EditionAttributes: edAttrs,
		EditionKey:        editionKey,
		EditionLabel:      label,
		EditionType:       romEditionType(p.Ext),
	}, true
}

// romEditionType is what kind of dump the file's shape says it is.
func romEditionType(ext string) string {
	switch ext {
	case ".zip", ".7z":
		return "archive"
	case ".chd", ".rvz", ".gcz", ".ciso", ".cso", ".wbfs", ".pbp":
		return "compressed"
	case ".bin", ".cue", ".iso", ".img", ".ccd", ".cdi", ".gdi", ".m3u", ".wua", ".wux", ".rpx":
		return "disc-image"
	default:
		return "cartridge"
	}
}

// romName is what a No-Intro or Redump name says about a dump.
type romName struct {
	title    []string
	region   string // "USA", "Europe", "Japan, USA"; "" when untagged
	revision string // "Rev 1", "v1.1"; "" when untagged
	disc     string // "1" from "(Disc 1)"; "" when untagged
}

var (
	reROMTag      = regexp.MustCompile(`\s*[\(\[]([^\)\]]*)[\)\]]`)
	reROMDisc     = regexp.MustCompile(`(?i)^disc\s*(\d+)$`)
	reROMRevision = regexp.MustCompile(`(?i)^(rev\s*[\w.]+|v\d+(\.\d+)*)$`)
	romRegions    = newSet(
		"usa", "europe", "japan", "world", "asia", "australia", "brazil", "canada",
		"china", "france", "germany", "italy", "korea", "netherlands", "spain",
		"sweden", "taiwan", "uk", "hong kong",
	)
)

// parseROMName lifts the bracketed tags off a dump's name. The title is what
// comes before the first tag; a tag is a region when every comma-separated part
// of it is a region, a revision or disc by shape, and anything else (languages,
// "[!]", "(Proto)") is left unread.
func parseROMName(name string) romName {
	var out romName
	head := name
	if loc := reROMTag.FindStringIndex(name); loc != nil {
		head = name[:loc[0]]
	}
	out.title = tokenize(head)
	for _, m := range reROMTag.FindAllStringSubmatch(name, -1) {
		tag := strings.TrimSpace(m[1])
		switch {
		case out.region == "" && isRegionTag(tag):
			out.region = tag
		case out.revision == "" && reROMRevision.MatchString(tag):
			out.revision = tag
		case out.disc == "":
			if d := reROMDisc.FindStringSubmatch(tag); d != nil {
				out.disc = d[1]
			}
		}
	}
	return out
}

func isRegionTag(tag string) bool {
	parts := strings.Split(tag, ",")
	for _, part := range parts {
		if !romRegions[strings.ToLower(strings.TrimSpace(part))] {
			return false
		}
	}
	return len(parts) > 0
}

// DescribeROM is Describe for a ROM, whose identity is its platform and title
// (ADR-0121). The tags a dump's name carries are not identity, so a want for
// "Super Mario World (USA)" and a scan of "Super Mario World (Europe).sfc"
// converge on one Work with two editions.
func DescribeROM(platform, title string) (key, sortTitle string, err error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if !romPlatforms[platform] {
		return "", "", fmt.Errorf("%q is not a ROM platform (one of %s)", platform, strings.Join(ROMPlatforms(), ", "))
	}
	toks := parseROMName(strings.TrimSpace(title)).title
	norm := normKey(toks)
	if norm == "" {
		norm = strings.ToLower(strings.TrimSpace(title))
	}
	return workKey(ROM, platform, norm), norm, nil
}

// ROMPlatformFromKey reads the platform back out of a ROM work key, the one
// place a fan-out Work keeps it (see the package note above).
func ROMPlatformFromKey(key string) (string, bool) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || parts[0] != ROM || !romPlatforms[parts[1]] {
		return "", false
	}
	return parts[1], true
}

func slug(s string) string {
	return strings.Join(tokenize(strings.ToLower(s)), "-")
}

func nonEmpty(parts ...string) []string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// romPlatformAliases is how release names spell a platform, as lower-case
// token sequences. Longer spellings are tried first, so "wii u" is never read
// as "wii" and "game boy advance" never as "game boy".
var romPlatformAliases = map[string][]string{
	"atari2600":       {"atari 2600", "atari2600", "a2600"},
	"atari7800":       {"atari 7800", "atari7800", "a7800"},
	"lynx":            {"atari lynx", "lynx"},
	"nes":             {"nintendo entertainment system", "famicom", "nes"},
	"snes":            {"super nintendo entertainment system", "super nintendo", "super famicom", "snes", "sfc"},
	"n64":             {"nintendo 64", "n64"},
	"gb":              {"game boy", "gameboy", "gb"},
	"gbc":             {"game boy color", "gameboy color", "gbc"},
	"gba":             {"game boy advance", "gameboy advance", "gba"},
	"nds":             {"nintendo ds", "nds"},
	"n3ds":            {"nintendo 3ds", "3ds", "n3ds"},
	"virtualboy":      {"virtual boy", "virtualboy"},
	"gc":              {"gamecube", "game cube", "ngc"},
	"wii":             {"wii"},
	"wiiu":            {"wii u", "wiiu"},
	"switch":          {"nintendo switch", "switch", "nsw"},
	"mastersystem":    {"master system", "mastersystem"},
	"megadrive":       {"mega drive", "megadrive", "genesis"},
	"segacd":          {"sega cd", "mega cd", "segacd"},
	"sega32x":         {"sega 32x", "32x"},
	"saturn":          {"sega saturn", "saturn"},
	"dreamcast":       {"dreamcast"},
	"gamegear":        {"game gear", "gamegear"},
	"psx":             {"playstation 1", "playstation one", "psx", "ps1", "psone"},
	"ps2":             {"playstation 2", "ps2"},
	"ps3":             {"playstation 3", "ps3"},
	"psp":             {"playstation portable", "psp"},
	"psvita":          {"playstation vita", "ps vita", "psvita", "vita"},
	"pcengine":        {"pc engine", "pcengine", "turbografx 16", "turbografx", "tg16"},
	"neogeo":          {"neo geo", "neogeo"},
	"ngp":             {"neo geo pocket"},
	"ngpc":            {"neo geo pocket color"},
	"wonderswan":      {"wonderswan"},
	"wonderswancolor": {"wonderswan color"},
	"xbox":            {"xbox"},
}

// romAliasSeqs is romPlatformAliases flattened to token sequences, longest
// first, built once.
var romAliasSeqs = func() []romAlias {
	var out []romAlias
	for platform, spellings := range romPlatformAliases {
		for _, s := range spellings {
			out = append(out, romAlias{platform: platform, toks: strings.Fields(s)})
		}
	}
	// Every vocabulary name spells itself ("[NGPC]", "[WonderSwanColor]"): a
	// release naming exactly the platform a want names must pass the gate.
	for platform := range romPlatforms {
		out = append(out, romAlias{platform: platform, toks: []string{platform}})
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].toks) != len(out[j].toks) {
			return len(out[i].toks) > len(out[j].toks)
		}
		if out[i].platform != out[j].platform {
			return out[i].platform < out[j].platform
		}
		return strings.Join(out[i].toks, " ") < strings.Join(out[j].toks, " ")
	})
	return out
}()

// releaseTokens lower-cases a release name into its alphanumeric tokens and
// reports, for each, whether it sat inside brackets ((), [] or {}).
func releaseTokens(title string) (toks []string, bracketed []bool) {
	depth := 0
	var cur strings.Builder
	curBracketed := false
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			bracketed = append(bracketed, curBracketed)
			cur.Reset()
		}
	}
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if cur.Len() == 0 {
				curBracketed = depth > 0
			}
			cur.WriteRune(r)
		case r == '(' || r == '[' || r == '{':
			flush()
			depth++
		case r == ')' || r == ']' || r == '}':
			flush()
			if depth > 0 {
				depth--
			}
		default:
			flush()
		}
	}
	flush()
	return toks, bracketed
}

// sizeUnits are the size units a release name may state that are also a
// platform's spelling.
var sizeUnits = newSet("gb")

type romAlias struct {
	platform string
	toks     []string
}

// ReleasePlatform is what a release name says about the platform it is for.
type ReleasePlatform struct {
	// Platform is the one platform the name spells out; "" unless Determined.
	Platform string
	// Determined is false when the name spells out no platform, or more than
	// one (a multi-platform pack, or a title that happens to contain a
	// platform's name): either way the name cannot show which one it is.
	Determined bool
	// Spelled is every platform the name spells out, sorted, for the
	// explanation.
	Spelled []string
}

// ParseReleasePlatform reads the platform a release name spells out, for the
// ROM platform gate (ADR-0121 §4). It is the release-title counterpart of the
// scan rules above and, like ParseReleaseSeasonEpisode, the only place a
// release title is read for it. Bracketed tags count ("[SNES]" is the commonest
// spelling), so it tokenises the raw name rather than the bracket-stripping
// tokenize. Each token is claimed by the longest spelling that covers it.
func ParseReleasePlatform(title string) ReleasePlatform {
	return ParseReleasePlatformFor(title, "")
}

// ParseReleasePlatformFor is ParseReleasePlatform for a release found for a
// wanted title: the title's own words are claimed first, so a game named after
// a platform ("Switch" on the PSX, "Game Boy Camera") is not read as evidence
// for that platform in every release of it.
func ParseReleasePlatformFor(title, workTitle string) ReleasePlatform {
	// A release named like the file it is ("Tetris 2.gb", "Metal Slug.ngc")
	// ends in an extension only one platform uses: that is evidence for that
	// platform, and is lifted off before the words are read, so ".gb" after a
	// number is not taken for a size and ".ngc" not for the GameCube.
	extPlatform := ""
	if i := strings.LastIndex(title, "."); i > 0 {
		if p, ok := romExtPlatform[strings.ToLower(strings.TrimSpace(title[i:]))]; ok {
			extPlatform, title = p, title[:i]
		}
	}
	toks, bracketed := releaseTokens(title)
	claimed := make([]bool, len(toks))
	if wt, _ := releaseTokens(workTitle); len(wt) > 0 {
	search:
		for i := 0; i+len(wt) <= len(toks); i++ {
			for k := range wt {
				if toks[i+k] != wt[k] {
					continue search
				}
			}
			for k := range wt {
				claimed[i+k] = true
			}
			break
		}
	}
	// "GB" after a number is a size ("2 GB", "1.4 GB"), not the Game Boy: claim
	// it before any alias can, so a SNES release that states its size is not
	// read as SNES-and-Game-Boy. A bracketed "[GB]" is a platform tag even after
	// a number ("Tetris 2 [GB]"), so only an unbracketed one is a size.
	for i := 1; i < len(toks); i++ {
		if sizeUnits[toks[i]] && !bracketed[i] && isDigits(toks[i-1]) {
			claimed[i] = true
		}
	}
	found := map[string]bool{}
	for _, a := range romAliasSeqs {
		n := len(a.toks)
	scan:
		for i := 0; i+n <= len(toks); i++ {
			for k := 0; k < n; k++ {
				if claimed[i+k] || toks[i+k] != a.toks[k] {
					continue scan
				}
			}
			for k := 0; k < n; k++ {
				claimed[i+k] = true
			}
			found[a.platform] = true
		}
	}
	if extPlatform != "" {
		found[extPlatform] = true
	}
	out := ReleasePlatform{}
	for p := range found {
		out.Spelled = append(out.Spelled, p)
	}
	sort.Strings(out.Spelled)
	if len(out.Spelled) == 1 {
		out.Platform, out.Determined = out.Spelled[0], true
	}
	return out
}
