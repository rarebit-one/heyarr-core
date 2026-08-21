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
