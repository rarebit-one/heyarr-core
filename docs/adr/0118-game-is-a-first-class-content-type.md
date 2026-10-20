# 0118. `game` is a first-class content type, fetched from a store it is owned on

**Status:** Proposed
**Date:** 2026-10-20
**Follows:** ADR-0080 (a new content type is registered, not reused), ADR-0082
(a content type decides its route and default profile), ADR-0115

## Context

People own games across several stores and want those copies held the way
Heyarr holds everything else: content-addressed, replicated to a second site,
and reconciled against what they asked for. A game is replaceable plaintext
mass media with strong external identity (store product ids, build ids), which
is Heyarr's core model. It is not personal state; only save games are, and
they belong to the encrypted personal plane, not here.

Nothing in the vocabulary could hold one. A game library could not be created
(`IsContentType` refused it, #227), an installer in any library fell through
to the movie-first fallback or to Unidentified, and every acquisition route
was the wrong one: an indexer search for a game returns mostly repacks, whose
titles have no open parser (ADR-0091) and whose bytes are not the copy a
person owns.

## Decision

### 1. `game` is registered as its own content type

`game` joins the registry exactly as `document` did (ADR-0080): a constant, a
member of `ContentTypes()`, and rules in `Default()`. Library creation, the
works-patch guard and `GET /works?content_type=game` pick it up for free.

### 2. Identification trusts specific shapes everywhere and generic ones only in a game library

A GOG offline installer (`setup_<slug>_<version>_(<build>).exe` and its
`-N.bin` parts) and a Steam DepotDownloader tree (`<Game>/depots/<depot>/<manifest>/…`)
are unambiguous, so those rules fire in any library and are registered first —
a depot's intro video must not become a movie. A bare `.exe` or `.zip`, or a
game's `.pak`/`.dll` data files, are evidence of a game only inside a game
library, where everything under `<Title>/` is that game. Those rules carry the new `Rule.RequiresLibrary`, so a
fake movie torrent's `setup.exe` stays unidentified instead of minting a
phantom game Work.

An edition is one store's package at one build: `gog:<platform>:<package>:<build>`
or `steam:<depot>:<manifest>`. A new build is a new edition and a DLC installer
is its own edition of the base game, never folded into the base game's bytes.
Each file records on its asset what its name says: `store`, `platform`,
`build_id` and, where the store states it, `drm_free`. A Steam depot leaves
`drm_free` unset because the depot does not say.

**Filenames describe; they do not certify.** A lone `-1.bin`, or one `.dll` from
a depot, carries the same store facts as the whole package, and one file of a
game is not a usable copy of it. So the facts a profile judges live on the
**edition**, and only a store fetch writes them there, after verifying the
whole package against the store's manifest (part count, checksums). A scan
never does.

### 3. A game's work key is its title alone

Installer names, depot trees and store listings almost never carry a year, and
a want that keyed on one would never converge with the installer that
satisfies it. `Describe` therefore drops the year for games, and game titles
bypass `parseName`, which would read "Cyberpunk 2077" as a title and a year. A
curated directory's `(2015)` is kept as display metadata.

### 4. A new route: `store`

Games are on `acquisition.RouteStore`: fetched by a store's own tooling against
the person's library, never asked of an indexer. `Route.Searchable` now names
the routes that are not searched (direct, store) so an unknown route keeps the
conservative "searchable" reading. The store fetch itself is the next change;
until it lands a game want reports what is held and is never searched.

### 5. The `owned` profile, and three policy attributes

`store` (text), `drm_free` (flag) and `platform` (text) join the closed
attribute vocabulary. Reconcile reads them from the edition's attributes,
never the asset's (§2). Until the store fetch lands, nothing writes them, so a
game want reports undetermined rather than satisfied; that is the intended
order, not a rule that silently never matches. `build_id` stays an edition fact, not a policy attribute: a build
equality says nothing about quality, and "newest build" is the store fetch's
concern.

The seeded `owned` profile accepts a copy whose store is known, prefers a
DRM-free one, and is terminal on DRM-free. An indexer repack carries no store,
so it is refused as *undetermined* with the rule named (§63), which is the
honest answer. A Steam depot is kept but stays open to a DRM-free copy.

### 6. Where `game` was deliberately NOT added

- **Torznab categories.** Games are not searched on the default route, so
  mapping them would advertise a path they do not take. That arrives with an
  opt-in indexer route for DRM-free content, gated by a profile.
- **DLNA.** It already lists only servable media types, so an installer never
  appears on a television.

## Consequences

- A `game` library can be created and scanned; GOG installer sets, Steam depots
  and curated `<Title>/` trees identify as games with stable keys.
- A scanned game is held and browsable, but does not satisfy a want under
  `owned`: its store facts describe files, not a verified package, so it reports
  undetermined. A verified store package (the next change) satisfies; a person
  who wants scans to count can choose a looser profile.
- Same-titled games (a 1996 original and its 2013 reboot) share a work key.

## What would make us revisit

- **Title collisions in practice.** If remakes collide in real libraries, key on
  an external id (`gog:`, `steam:`, `igdb:`) once discovery records one, the
  ADR-0088 confident-match path, rather than reintroducing a year.
- **The opt-in indexer route.** A profile that admits DRM-free indexer releases
  will need the Torznab categories and a way for a profile to name its route.
- **Launch state.** Per-game runner settings and save games are personal state
  and belong to the client and the vault tier, not here.
