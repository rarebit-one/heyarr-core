# The provider fixture corpus

Recorded request/response pairs from a **real** Torznab endpoint and a **real**
Transmission instance, replayed against the real client code (ADR-0026).

Torznab rather than Prowlarr, per ADR-0028: Heyarr binds to the protocol, so
the corpus stays valid across product versions — which matters more here than
anywhere else, because for an indexer these fixtures are the only test that
will ever run.

## Why this directory is empty

Because nobody has captured yet, and inventing the contents would defeat the
point.

A real indexer proxies real trackers with real credentials. It is not
reproducible, it is not ours, and it can never run in CI. ADR-0026 therefore
makes recorded fixtures the **primary** test strategy for indexers rather than
a stand-in for one — and that raises the stakes on what goes in here
considerably.

For a media file, a hand-written fixture is merely unrealistic. For an indexer,
a hand-written fixture is *the only thing the test will ever see*. It does not
approximate reality; it replaces it. A corpus that was invented rather than
captured tests that the client agrees with whoever invented it.

So this directory stays empty until somebody with an instance runs the capture.

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
├── torznab/
│   ├── caps.json
│   ├── search-with-results.json
│   ├── search-empty.json
│   └── unauthorised.json
└── transmission/
    ├── session-handshake-409.json
    ├── session-get.json
    └── torrent-get.json
```

Every file carries provenance — origin, service, version, when, and the exact
procedure — and `Load` **refuses** one that does not. A capture nobody can
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
| **results with fields OMITTED** | see below |

That last one is easy to overlook and is the one this corpus most needs.
`Indexer.Search` returns `[]acquisition.ReleaseCandidate` directly, so attribute
extraction happens **inside** the provider with no conversion layer. §63 can only
report `undetermined` for an attribute it never received — so the "could not
determine" path, which is most of how a degraded node behaves, is unreachable
without a fixture that genuinely leaves the field out.

A corpus of successful, complete responses is one that has never seen the
responses that actually occur at 03:00.
