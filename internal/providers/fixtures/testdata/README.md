# The provider fixture corpus

Recorded request/response pairs from a **real** Torznab endpoint and a **real**
Transmission instance, replayed against the real client code (ADR-0026).

Torznab rather than Prowlarr, per ADR-0028: Heyarr binds to the protocol, so
the corpus stays valid across product versions — which matters more here than
anywhere else, because for an indexer these fixtures are the only test that
will ever run.

## What is in here, and what it cost to get

The Torznab corpus holds captures from **two different servers speaking one
protocol** — Jackett and Prowlarr, both configured against the same public
tracker. That is not redundancy. ADR-0028's claim is that this client is bound
to the protocol rather than shaped to one product, and a single corpus cannot
demonstrate it: the client would be shaped to whichever server it saw first
while looking exactly as though it were not.

The two disagree about the most important response in the protocol:

| | invalid API key | unsupported function |
|---|---|---|
| Jackett | HTTP **200** with `<error code="100">` | HTTP **200**, code 201 |
| Prowlarr | HTTP **401** with an **empty body** | HTTP **400**, code 202 |

So an error document arrives with 200 *and* with 400, and an error also arrives
with no document at all. Neither the status nor the body can be trusted alone.

### The tracker is Linux ISOs, and that has a cost worth naming

Captures are taken against a tracker that indexes **nothing but Linux
distributions**, so every release name committed permanently to this public
repository is of the form `ubuntu budgie 26 10 snapshot1 desktop amd64 iso`.

The cost: a Linux ISO asserts no resolution, no codec, no audio layout and no
HDR. **No real capture in this corpus contains a quality attribute**, and
against these two indexers the provider determines a release's SIZE and nothing
else — every quality rule in a §62 profile evaluates to `undetermined`.

That absence is genuinely useful, because it is the `undetermined` path made
reachable with real data. But it means the positive cases live in
`torznab/synthesised/`, clearly labelled, and one indexer's habits are **not**
coverage of Torznab's real attribute variance. Claiming otherwise would be the
false confidence ADR-0026 exists to prevent.

### It is not one tracker's quirk — it is what discovery can know (#129)

Measured against a **second, unrelated** real indexer carrying actual film (an
Internet Archive endpoint, via the same manager), the per-item attributes were
identical to the Linux-ISO tracker's: `category`, `seeders`, `grabs`, `infohash`
and the volume factors — **no `resolution`, `video_codec`, `source`,
`audio_channels` or `hdr`**. So this generalises past the corpus: against a real
indexer, discovery determines **size and identity, nothing else**.

The asymmetry that makes this a design position rather than a gap: the SAME
release, once its bytes are held and probed, has every attribute determined —
resolution, codec, source, HDR and audio channels all resolve to `pass`/`fail`/
`bonus` against a §62 profile. So a quality attribute is knowable, but only
*after* acquisition (`ffprobe`), never at discovery.

| | resolution | codec | source | hdr | audio | size |
|---|---|---|---|---|---|---|
| **candidate**, from a real indexer | undetermined | undetermined | undetermined | undetermined | undetermined | determined |
| **held asset**, once probed | determined | determined | determined | determined | determined | determined |

The sharp consequence, worth stating because §62 profiles read as though the
indexer will tell us what a release is: an `undetermined` **accept** gate fails
closed, so a profile that gates acquisition on `resolution` makes **no candidate
ever acceptable against any real indexer** — acquisition is structurally
unreachable, presenting as "nothing is ever acquired" with no error anywhere. A
profile that gates only on `size_bytes` acquires and then satisfies. This is the
concrete case for treating the indexer as a release LOCATOR (size + identity)
and leaving quality to the probe (§60 upgrades) and to #107's metadata
providers — the seeded profiles gating on `resolution` in `accept` contradict
it.

### The real `429` is a manager disabling an indexer, not a tracker rate limit

Captured live, the shape a constructed sequence would not guess:

```
http=429   <error code="429" description="Indexer is disabled till <time> due to recent failures." />
```

It answers in **milliseconds** where a successful search takes ~100 s, because
this is the indexer MANAGER short-circuiting on its own account — a different
thing from an upstream tracker asking for room, with the same status code. The
client now surfaces that `description` (`RateLimitError`, `internal/indexers`)
instead of flattening every 429 to "rate limiting", while still treating it as a
rate limit for backoff. A synthesised fixture would capture the shape; the real
one depends on first provoking a failure, so it is a sequence rather than a
response.

## Capturing

```sh
scripts/capture-fixtures.sh torznab      http://host:9696/1/api <api-key>
scripts/capture-fixtures.sh transmission http://host:9091 <user> <pass>
```

Then read what it wrote, and check it the way CI will:

```sh
git diff --stat internal/providers/fixtures/testdata
go test ./internal/providers/fixtures/ -run TestTheCommittedCorpusIsClean
```

## ⚠️ This is a public repository and git history is permanent

A tracker passkey identifies a person to a private tracker. An indexer API key
is a live credential. Neither can be un-committed — rotating afterwards does not
remove it from history.

Redaction happens **at capture time**, in `redact()` in the capture script. The
scanner in this package is a **second line**, running over the committed corpus
in CI: it exists to catch the time redaction was wrong, not to replace it. If
the scanner fires, fix `redact()` too, or the next capture repeats the mistake.

The scanner elides what it finds rather than printing it. CI logs are as public
as the repository, and a guard that prints the secret it caught is a second way
of publishing it.

## Layout

One directory per service, one file per exchange:

```
testdata/
├── torznab/                 the PROTOCOL
│   ├── jackett/             one server that speaks it
│   │   ├── caps.json
│   │   ├── search-with-results.json
│   │   ├── search-empty.json
│   │   ├── unauthorised.json          200 + <error code="100">
│   │   ├── unsupported-function.json
│   │   └── indexer-not-found.json     a JSON body on a Torznab path
│   ├── prowlarr/            another, disagreeing
│   │   └── … the same six, answered differently
│   └── synthesised/         NOT captured; see each file's note
└── transmission/
    ├── session-handshake-409.json
    ├── session-get.json
    └── torrent-get.json
```

A server whose name IS its protocol gets no subdirectory — `transmission` is
one protocol with one implementation, and `transmission/transmission` would be
a directory saying nothing twice. Torznab has several, and there the
subdirectory is the only thing stopping two captures of `caps` from
overwriting each other.


Every file carries provenance — origin, service, **which server answered**,
version, when, and the exact procedure — and `Load` **refuses** one that does
not. `server` is required for a capture specifically: the corpus's central
claim is unreadable if a fixture cannot say which implementation produced it. A capture nobody can
regenerate is one nobody can trust the day it starts failing, which is the same
reasoning that puts a version, a digest and a URL in `scripts/toolchain.lock`
rather than just a URL.

`origin: synthesised` is legal and deliberately uncomfortable: it must justify
itself in its note. It is for shapes a healthy instance will not produce on
demand — a 429, a truncated body — not a shortcut for not having an instance.

## What the corpus needs to contain

Beyond the happy path, which is the easy half:

| case | why it matters |
|---|---|
| a search with **zero** results | a normal outcome that must not fail a job into backoff, or an unavailable release becomes an indexer hammering loop |
| a bad key | Torznab signals it as an `<error code="100">` **document, usually with HTTP 200** — so a client checking only the status code reads an error as a successful empty search and reports "no releases found" forever |
| `t=caps` | the capability handshake: which content types the indexer can actually search |
| `429` | rate limiting is normal operation, not an error to propagate |
| a malformed body | the error must name what failed to parse |
| a non-JSON error page | what a reverse proxy in front of the service actually returns |
| Transmission's `409` handshake | a client treating it as an error works against every hand-written fixture and fails against every real instance |
| **results with fields OMITTED** | see below — and in this corpus the real captures are ALL of this shape, for quality attributes |

That last one is easy to overlook and is the one this corpus most needs.
`Indexer.Search` returns `[]acquisition.ReleaseCandidate` directly, so attribute
extraction happens **inside** the provider with no conversion layer. §63 can only
report `undetermined` for an attribute it never received — so the "could not
determine" path, which is most of how a degraded node behaves, is unreachable
without a fixture that genuinely leaves the field out.

A corpus of successful, complete responses is one that has never seen the
responses that actually occur at 03:00.
