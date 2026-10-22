# 0121. A ROM is its own content type, searched by platform and title

**Status:** Proposed
**Date:** 2026-10-21
**Follows:** ADR-0118 (`game`, the store route), ADR-0093 (a match gate before
quality), ADR-0091 (attributes read from a release name), ADR-0119 (fan-out wants)

## Context

An operator wants console games held for an emulator handheld: a SNES
cartridge dump, a PS2 disc image. They are a different shape from what
ADR-0118 built `game` for.

- **No store.** A `game` comes from a store the person owns it on, and the
  `owned` profile refuses anything without one. A console dump has no store to
  fetch from, so on the store route it could never be acquired.
- **The route is per content type.** `strategy.For` picks the route from the
  content type alone. A console `platform` on `game` could not send one game to
  a store and another to an indexer.
- **The platform is identity.** "Tetris" on the Game Boy and "Tetris" on the
  NES are different games. A game's key is its title alone (ADR-0118 §3), which
  would collapse them, and would merge a SNES dump with a PC remake.
- **Names carry tags, not identity.** No-Intro and Redump name a dump
  `Title (Region) (Rev 1) (Disc 2)`. A want for "Super Mario World (USA)" and a
  scanned European dump are the same game in two editions.

## Decision

### 1. `rom` is its own content type, on the search route

`rom` joins the registry beside `game`, the way `game` did (ADR-0118 §1): a
constant, a member of `ContentTypes()`, and rules in `Default()`. Its strategy
is the search route with a new seeded `rom-indexed` profile. It is never judged
by `owned`, so `owned` keeps meaning "a store says you own this". A ROM taken
from an indexer is not certified as owned by anything here; where it came from
is the operator's business.

### 2. The key is the platform and the title

A ROM's work key is `rom:<platform>:<title>`, the title with its bracketed tags
lifted off. The platform comes from a closed vocabulary, ES-DE's system short
names (`snes`, `gba`, `ps2`, `n3ds`, …), which is also how a ROM library is laid
out. The platform is in the key, not only in the Work's attributes, because a
fan-out want (ADR-0119) recreates the Work at a sibling with empty attributes
and the key is what survives. The search reads the platform back from the key.

`Describe` has no platform parameter, so for a ROM it returns an empty key and
only the sort title. `DescribeROM(platform, title)` is the one way to key a ROM.
The want API's work descriptor gains `platform`, which is required for a `rom`,
must be in the vocabulary, and is refused on every other content type.

### 3. A ROM library is laid out by platform

Every ROM rule needs a ROM library (ADR-0118's `RequiresLibrary`): `.bin`,
`.iso` and `.zip` are evidence of nothing anywhere else.

- `<platform>/<file>` and `<platform>/<Game>/<file>` (a multi-file disc set)
  take the platform from the directory.
- A file whose extension only one platform uses (`.sfc`, `.gba`, `.nds`, …)
  is identified wherever it sits.
- A shared shape (`.iso`, `.chd`, `.bin`, `.zip`) at the library root is left
  unidentified.

No dump extension has a media type in the scanner's built-in table, so a scan
of a rom library keeps exactly the files these rules identify, with no media
type, and skips the rest. An operator's own extension table still overrides.

Region, revision and disc are read off the name onto the edition. They describe
the dump; they certify nothing. A hash match against a No-Intro or Redump DAT is
a later change, and until then no profile gates on them.

### 4. A platform match gate runs before quality

A ROM want's search keeps only releases whose name spells out the wanted
platform. This is a match gate in the ADR-0093 sense: which platform a release
is for is identity, not quality, so it is not a profile rule and its rejections
are tellable apart from quality ones (`Section: match`).

- `platform.matches` passes the wanted platform and fails another one.
- `platform.undetermined` fails a release that names no platform, or names
  more than one (a bundle, or a title that happens to contain a platform's
  name). The gate never keeps what it cannot show to be for the wanted platform.

`identification.ParseReleasePlatform` reads the platform from the release name,
the release-title counterpart of the scan rules. It matches the longest
spelling first, so "Wii U" is never read as "Wii" and "Game Boy Advance" never
as "Game Boy". The rejection is reported with its reason, never silently
dropped.

The query sent to an indexer is the title alone. Prowlarr is asked for the
Console parent category (1000) rather than a subcategory, because the Newznab
console subcategories name only some platforms and indexers file older ones
under Console/Other. The plain Torznab client sends no categories and falls back
to the general search, as for any type it has no function for.

### 5. Region ranks releases; it never gates them

When several releases of a ROM pass the platform gate, the household prefers
USA, then World, then Europe. `region` joins the attribute vocabulary as a set:
one dump is often several regions (`(USA, Europe)`). A rule on a set-valued
attribute holds when any member does, so `eq`/`in` ask "is one of them this"
and `neq`/`nin` ask "is none of them". The search reads the regions from the
release name's tag, marked derived (ADR-0091), in the worker beside the
platform gate. An indexer-asserted region would win.

`rom-indexed` scores USA 30, World 20 and Europe 10. A profile section names
each attribute and operator once, so the tiers are four rules that sum to those
scores: `eq usa` +20, `in [usa, world]` +10, `neq usa` +10 and
`nin [usa, world, europe]` −10. The last two cancel for any other region. A
multi-region dump scores as its best region, so `(USA, Europe)` ties with a
USA-only one. A Japan-only or untagged release scores 0: still accepted, just
not preferred. The profile is terminal once any dump is held, so region never
keeps a want open: a held European dump is not swapped for a later USA one.
That keeps a held copy from looping against a release that names its region
when the held file does not. For the same reason, profile validation refuses
`region` in `accept` or `terminal`: a held dump carries no region, so such a
rule could never hold and the want would search forever.

Seeding never updates an existing profile, so migration 00071 brings an
installation's `rom-indexed` up to these preferences, but only when the row is
still exactly as it was seeded. A profile an operator edited is left alone.

## Consequences

- A ROM want is searchable as soon as an indexer carries the Console category.
  Which indexers to enable, and what to acquire, is the operator's choice.
- A release named without its platform, which is common for single-ROM uploads
  named the No-Intro way, is refused as undetermined. The safe direction costs
  recall; the reason names the rule, so the gap is visible.
- `rom-indexed` accepts any nonempty release that passed the gate, ranks it by
  region (§5), and stops once one is held. A size sanity bound and DAT
  verification are not built.
- Only a dump satisfies a ROM want. Artwork and disc descriptors (`.cue`,
  `.m3u`, …) attach to the Work but never satisfy it. At ingest, a release that
  holds no dump, or whose payload is a dump for another platform, is refused as
  the release's fault.
- An acquired file is read with the want's platform, because a transfer path
  has no platform directory. Its edition facts still come from its own name.
- A Work cannot be corrected into a `rom`: the correction keeps the old key,
  which names no platform.
- Cover art can create a ROM edition before its dump arrives, so an edition
  without a type takes the type of the first file recorded into it that states
  one. That change emits `content.edition.updated`.
- Multi-file editions count per dump asset, as an ordinary edition does. The
  whole-package rule that `game` editions follow (ADR-0120) does not apply until
  DAT verification gives a ROM a manifest to check against.

## Not decided here

- Delivering ROMs to a device (an Android Games section writing into ES-DE's
  folders).
- BIOS and firmware files. A console-specific key or firmware dump belongs to
  the personal plane, not here.
- Save games, which are personal state (ADR-0118's Context).
