# 0091. Release attributes may be derived from the title — opt-in, fill-only-absent, and marked as derived

**Status:** Accepted (2026-09-10)
**Date:** 2026-09-10
**Milestone:** M3 — Search & Grading (§62 profiles / §63 attributes)

## Context

`internal/indexers/attributes.go` extracts a release's attributes from
**structured torznab `<attr>` fields only, and never from its title** (§63). The
rationale is stated there and is a good one: a title is "a filename written by a
stranger, with no schema and no obligation to be true", and an attribute derived
from one would be "indistinguishable, downstream, from attributes an indexer
actually asserted" — so §63's explanation would be reporting confidence in a
guess. `ReleaseCandidate.Title` carries the same instruction: *never parsed at
scoring time.*

That decision has a consequence the same file names plainly: *"against the two
indexers actually measured, this provider determines a release's SIZE and nothing
else. Every quality rule in a §62 profile evaluates to `undetermined` on their
candidates."* And it offers a way out — *"closing it is a metadata provider's
job rather than a better regular expression over titles."*

In practice that way out does not exist for the attributes in question. Observed
live on a reference host (2026-09-10): a `tv_series` follow on the `living-room`
profile — whose accept gate is `resolution.gte 1080` — projected 36 episode
wants and archived **zero**. Prowlarr returns dozens of real releases per episode
(a `1080p WEBRip x265` episode, and season packs), but a torrent tracker emits
`<size>` and no structured resolution, so every candidate is `resolution:
undetermined`, the accept gate can never hold, and the worker logs *"a search
found nothing acceptable"* on every pass. The size-only `indexer-determinable`
profile is the only one that accepts anything, and it rejects the actual matched
candidates on its 2 GB ceiling. **No profile makes a real TV or film follow
acquire from a title-only indexer.** That is the primary use case for the tool,
not an edge.

The "metadata provider's job" escape does not close it, because the missing
attributes are properties of the **release**, not of the **work**. TMDB (wired
up and healthy) resolves a series' identity — episodes, air dates, ids — but it
has no idea what resolution, source or codec a given torrent is; that fact lives
in exactly one place, the release name, and no provider of *work* metadata can
supply it. The design tells the operator to look elsewhere for a fact that has no
elsewhere.

And the empirical case is strong: the entire Sonarr/Radarr/Prowlarr ecosystem
grades releases by parsing their names, and it works, because scene and p2p
naming of the quality tokens (`1080p`, `WEB-DL`, `x265`, `HDR`) is a de-facto
schema, not free prose. heyarr already trusts exactly this: the **scanner** parses
the same tokens out of on-disk filenames (`internal/domain/identification/
quality.go`) to assign an edition's resolution/source/codec. The asymmetry — a
filename on disk is parseable evidence, the identical string from an indexer is
not — is the thing this ADR removes.

## Decision

**A release's attributes MAY be derived from its title, but only when title
derivation is switched on, only for attributes the indexer did not assert, and
only as values that are visibly marked as title-derived wherever §63 reports
them.** Structured evidence always wins; a guess never overwrites an assertion;
and no derived value is ever, in the words of the file this ADR amends,
"indistinguishable downstream from attributes an indexer actually asserted" —
because it is distinguishable, by construction, everywhere it is shown.

### 1. Opt-in, off by default

A new indexer setting `parse_titles` (default `false`) gates the whole behaviour.
Off, `attributesOf` is byte-for-byte what it is today: structured attrs and size,
nothing from the title. A deployment that has decided scene naming is reliable
enough for its trackers turns it on; one that shares §63's original caution
leaves it off and loses nothing. Reversing a deliberate design position is a
choice the operator makes, per node, not one this change makes for them.

### 2. The parser is the scanner's, exposed — not a second one

Derivation reuses `internal/domain/identification`'s existing, fixtured quality
parser rather than growing a second vocabulary of regexes over titles. The
package gains one exported entry point:

```go
// ParseReleaseAttributes reads the quality tokens (resolution, source, codec,
// dynamic range) a release name asserts about itself. It is the scanner's
// filename parser, applied to an indexer's release title.
func ParseReleaseAttributes(title string) map[policy.Attribute]policy.Value
```

It tokenises the title (`tokenize`) and runs `parseQuality` over the tokens — the
same code that decides a scanned file is `2160p HDR web-dl`. Sharing it means an
on-disk copy and an indexer release converge on one reading of the same string,
and a fix to the token table fixes both. Values are mapped into policy's typed
vocabulary: `resolution "1080p" → Num(1080)`, `source "web-dl" → Str("web-dl")`,
`codec "x265" → Str("hevc")` (the parser's `x264`/`x265` spellings are folded to
policy's `h264`/`hevc` class), and any recognised `dynamic` range → `Flag(true)`
for `hdr`. Audio codec and channel count are **not** derived: the parser does not
model them, and inventing them from a title is exactly the guess §63 rightly
fears.

### 3. Extraction stays provider-side, and asserted always wins

Derivation happens in `attributesOf` — in the provider, at extraction time — not
in `candidate.go` at scoring time. This keeps the boundary `ReleaseCandidate.Title`
draws ("attributes are extracted by the provider… re-deriving them from the title
at scoring time would be a second, invisible extraction"): the scorer still scores
the `Attributes` it is handed and never looks at the title.

The merge is **fill-only-absent**. `attributesOf` builds the structured map first,
exactly as today; then, if `parse_titles` is on, it fills in each derived
attribute **only where the structured map has no key**. A torznab `<resolution>`
of 1080, or a `<size>`, is never replaced by a parse of the name. A real assertion
beats a derived one, always — the half of §63 that is not negotiable.

### 4. Derived is marked, so the explanation stays honest

`policy.Value` gains a `Derived bool` (zero value `false`, so every existing
constructor and every structured value is asserted by default). `ParseReleaseAttributes`
sets it `true`. §63's reason rendering reads it and says which it is:

```
resolution 1080, which is at least 1080          (asserted)
resolution 1080 (read from the release name), which is at least 1080   (derived)
```

This is the direct answer to the objection the original file raises. Its fear is a
derived value that is "indistinguishable, downstream, from attributes an indexer
actually asserted", producing "a confident wrong answer with no reason attached".
Under this ADR the derivation is not invisible and not unattributed: the
explanation names it as a reading of the title at every gate it touches, so an
operator sees precisely which facts came from the tracker and which from the name,
and can distrust the second class specifically. The extraction §63 called invisible
is made visible; that is the whole of the concession it asked for.

### 5. Fixtures are real names, per ADR-0026

Unlike a Linux ISO, a release title asserts its own quality, so the parser's
fixtures are **real public release names** — `Slow.Horses.S01E01.1080p.WEB-DL.DDP5.1.x265`
and its kind — which are not secrets and carry no passkey (the fetch `Source` is
separate and already redacted). Each recognised and each deliberately-unrecognised
case is a fixture, so a branch of the token table that has never seen a real name
is, per ADR-0026, either fixtured or deleted.

## Consequences

- With `parse_titles` on, a `living-room`/`everyday` follow acquires from a
  title-only indexer as intended: `resolution`, `source`, `video_codec` and `hdr`
  become determinable, so a §62 accept gate can hold and a `prefer` can rank. This
  is the behaviour the tool's primary use case needs and did not have.
- §63's guarantee is preserved, not waived: an asserted attribute still beats a
  derived one, and the explanation still distinguishes "could not determine" from
  a fact — and now additionally distinguishes an asserted fact from a title-derived
  one. The honesty property is strengthened, not spent.
- The scanner and the indexer share one quality vocabulary. A title heyarr can read
  off a disk it can now read off a search result, and neither drifts from the other.
- `parse_titles` is per-node config, so a cautious deployment keeps today's behaviour
  exactly. Nothing changes for a node that does not opt in.

## Alternatives rejected

- **Size-only profiles as the answer** (`indexer-determinable` and kin). They
  acquire *something* without touching §63, but give up quality control entirely
  and walk into the season-pack-vs-episode problem (a 4 GB `S01` pack matching six
  episode wants). That is a worse place, reached to avoid a change that is
  ultimately the right one.
- **Always-on title parsing.** Simpler, and it is what the *arr tools do, but it
  overrides §63's position for every operator instead of offering it as a choice.
  The opt-in costs one config key and keeps the default honest.
- **A metadata provider for release quality.** The escape §63 names, but there is
  no provider of it: resolution/source/codec are properties of a release, not a
  work, and no work-metadata service (TMDB included) knows them. The title is the
  only source that exists.
