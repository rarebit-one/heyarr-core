# 0031. Provider credentials are typed by the provider's declared auth scheme

**Status:** Accepted
**Date:** 2026-08-22

## Context

§59 centralises provider configuration in one registry. A registry `Entry`
carried one credential field, `api_key`, and one field was enough for every
provider the registry had: Torznab authenticates with a bare token, and so does
every metadata provider likely to follow.

Transmission does not. Its RPC is HTTP basic auth, which needs a username **and**
a password. ADR-0025 had already ruled out putting the credential in a URL, so
M3-10 needed two fields from a registry that offered one.

What it did (#102) was pack both into the one field: the username defaulted to
`transmission`, and `api_key` could carry `user:pass` to override it. That was
recorded as a workaround at the point it happened rather than hidden, and #123
was filed against it.

**The workaround fails silently.** A password containing a colon —
`hunter2:the-real-part` — was split at the colon. Heyarr then authenticated as
user `hunter2` with password `the-real-part`, got a 401, and reported the
download client as unreachable. The operator's configuration was correct, the
daemon was healthy, and nothing in the system said which of those was true. It
is the worst shape a bug can have: a correct input, a wrong output, and a
plausible wrong explanation offered by the software itself.

## Decision

**A provider's credential is typed by the auth scheme its KIND declares.**

`AuthSchemeOf(Kind)` is a total function over the kinds: `torznab` → `token`,
`transmission` → `basic`, `fake` → `none`. The scheme is known before any
configuration is read, so validation can say what shape it *expected* rather
than inferring a shape from what it was handed.

`providers.Credential` holds that shape with unexported fields and
scheme-appropriate accessors. `Token()` answers only for a token credential and
`Basic()` only for a basic one; each returns a boolean rather than a
zero-valued half, so a caller cannot read a password out of a token provider and
send it as a bearer token. What used to be a naming convention is now the
compiler's problem.

Configuration keeps **two spellings and no per-service column**:

```yaml
providers:
  - name: an-indexer
    type: torznab
    api_key: sk-live-...              # shorthand: the scheme's single field

  - name: a-download-client
    type: transmission
    credential:
      username: heyarr                # typed: the shape the scheme declares
      password: "p:ssw0rd"            # a colon is a character here
```

`credential:` is one key on `Entry`, not one key per service. Which sub-keys it
reads is decided by the declared scheme, and a sub-key belonging to another
scheme is refused by name — `credential.token` on a Transmission entry is a
startup error, not a silently ignored line.

**The ambiguous legacy spelling is refused rather than guessed at.** `api_key`
containing a colon on a basic-scheme provider is now a startup error naming the
ambiguity and printing the replacement. Heyarr cannot tell `user:pass` from a
password containing a colon — they are the same bytes — so the only honest
answers are to guess or to ask. Guessing produced the bug. This is ADR-0025's
existing rule applied unchanged: a mistake somebody can fix in ten seconds
belongs at startup, because the same mistake found at the first acquisition
costs an afternoon of looking at the wrong system.

Every other pre-existing configuration is untouched. `api_key` on a Torznab
entry is still the token; `api_key` with no colon on a Transmission entry is
still the password with the assumed `transmission` username; no credential at
all is still an unauthenticated daemon on a trusted network.

**Nothing is stored.** ADR-0025's "no credential is stored in the database"
stands, so this needs no migration and no schema. A typed credential is a
richer shape in a configuration file that already holds the plaintext.

## Why not a `username` field

Because it would be empty for every provider but one. Torznab, and every
metadata provider on the roadmap, has nothing to put in it. A column that one
integration uses and the rest leave blank is exactly how a registry accretes
per-service fields, and §59 exists to prevent that. It also would not have
fixed anything on its own: `api_key` would still have been a field whose
meaning depended on which kind was reading it.

## Why not a reference into a secret store

This was the other candidate in #123, and it is where credentials probably end
up: `credential: {ref: "op://vault/item/password"}` keeps the plaintext out of
the row, out of the config file, and out of the backup stream.

It is rejected **for now, on size rather than on merit**. It needs a resolver
interface, at least one backend, a resolution point in startup that can fail for
reasons the configuration file cannot show, a story for what a node does when
the store is unreachable at 03:00 (ADR-0025 says it must still serve the
library, which means caching resolved secrets, which means deciding how long),
and a testing strategy for all of it. That is a milestone-sized change, and
#123 is about a colon.

It is also **orthogonal**, which is what makes deferring it cheap. A reference
resolves *to* a credential, and a credential now has a declared shape to resolve
into — so the secret-store direction is a new source for a `Credential`, not a
replacement for one. Doing the typing first makes it smaller, not larger.

## Consequences

**A new provider kind must declare its scheme.** `AuthSchemeOf` is total, and a
kind added without a case falls through to `none` — which is why a test asserts
every kind in `Kinds()` maps to a scheme in `AuthSchemes()`. A kind that
silently authenticated with nothing would be the same class of quiet failure
this ADR is about.

**A credential redacts as a whole, username included.** `Credential` and
`CredentialEntry` both redact in `fmt`, `slog` and JSON; only the *scheme*
survives, because it is not secret and it is what an operator debugging a 401
actually wants. The username is withheld because half of an RFC 7617 pair is
still half a credential. This was found by the log-scanning test rather than by
review — `Secret` already covered the password, and the raw config block printed
`"username":"heyarr"` beside two redactions — which is the argument for scanning
captured output instead of reading code.

**One deliberate break, for a configuration nobody should have had.** An
operator who wrote `api_key: user:pass` gets a startup error with the
replacement in the message. That is a real break and it is chosen over the
alternative, which is continuing to mangle passwords containing colons in
silence.

**The registry has one credential concept with several shapes**, rather than one
shape with several conventions. The next integration that needs two fields —
qBittorrent, SABnzbd, an OAuth-shaped metadata provider — adds a scheme, not a
column, and its configuration is refused if written in another scheme's shape.

## What would make us revisit

**An operator asking for the plaintext not to be in the config file at all.**
Compliance, a shared host, a config file in a git repository — any of those
makes the secret-store direction worth its size, and this decision is what it
would build on rather than something it would replace.

**A scheme that is not a fixed set of fields** — an OAuth flow with a refresh
token and an expiry, or a signed request needing a key id and a secret. Those
fit the typed model, but they also need a *lifecycle* (refresh, rotation,
re-authorisation) that a value read once from a file does not have. The second
one of those is the point at which "a credential is a value" stops being true.

**A provider kind whose scheme is not knowable from the kind** — a service that
speaks either token or basic depending on how the operator deployed it. That
would put the scheme back in configuration, and the declaration would move from
`AuthSchemeOf(Kind)` to an explicit `auth:` key. The shape checking survives;
only where the scheme is declared changes.
