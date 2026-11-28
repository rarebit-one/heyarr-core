# ADR-0129: Recording-specific music requests use explicit metadata evidence

A title alone does not identify a song. Artist and recording must survive from a
request through search and satisfaction; an album, another artist's song, or an
unrelated file must not satisfy the want. Existing music Works represent albums
and path-based scanner matches do not establish a song's identity.

A music descriptor names an artist and canonical MusicBrainz recording UUID
together. `music-recording:<uuid>` is a separate key namespace that cannot
collide with scanner-derived `music:<artist>:<album>` keys. The ID and artist
are caller assertions unless independently resolved. The indexer query includes
artist and title; the release must name the artist and either the song or an
album/compilation marker. Explicitly live, cover,
remix, instrumental and karaoke releases are rejected unless requested. This
filter narrows discovery; it is not verification.

A release may contain many audio files. Ingest inspects each private, temporary staging
copy within the store-owned staging directory and requires a unique file with both embedded title and artist
matching the request. It records only that file, and writes a match method
`embedded_tags` and the matching blob digest on the Work. Reconciliation must
only accept an asset whose current digest matches that evidence. This is a
metadata match, not proof of the audible performance or the requested MBID:
incorrect or dishonest tags can still yield a wrong result. Missing, conflicting
or ambiguous tags are never accepted; probe/tool failures remain retryable.
Fingerprints can be added as optional, stronger evidence later.

The staging bytes are hashed before and after inspection. Ingest checks the
stored bytes and requires a private, read-only inode before recording the match;
a shared hardlink is held for local repair. A known corrupt incumbent is queued
for the existing integrity verifier; an untracked corrupt incumbent requires
operator repair. Neither is silently replaced. Temporary staging files use the
store's existing cleanup mechanism. Legacy title-only music requests keep their
old behaviour and must be audited and migrated explicitly. Peer operations
carry the work key but not all attributes; a reconstructed peer must fail
closed without local metadata evidence. Playback and browsing must likewise
only expose assets for which the current digest has evidence. No guarantee of
cross-peer recording matching is made until evidence is carried or independently
re-established on each peer.
