# `scripts/acceptance.sh` — the section for #123

`scripts/acceptance.sh` is held by two other branches, so this is written out
rather than applied. Everything below is additive; nothing existing changes.

## Why anything belongs in the demo at all

The unit assertions prove the credential is typed, preserved and redacted. What
they cannot prove is that the **real binary reads a real config file** that way
— and the whole point of a typed credential is that it is a *configuration*
shape. The demo is the only place a `credential:` block is parsed by
`heyarr` from YAML rather than constructed as a Go value in a test.

Two of the three assertions below cost nothing: the config the demo already
writes gains a `credential:` block, and the existing provider-response
assertions get one more thing they must not contain.

---

## 1. Write the download client's credential in the typed form

**Anchor.** In the heredoc that writes the demo config, the existing entry:

```yaml
  - name: acceptance-downloads
    type: transmission
    endpoint: http://127.0.0.1:9
    path_map:
      - remote: /downloads/complete
        local: $FULLDATA/downloads
```

**Change.** Add a `credential:` block to it, with a colon in the password:

```yaml
  - name: acceptance-downloads
    type: transmission
    endpoint: http://127.0.0.1:9
    credential:
      username: acceptance-heyarr
      password: "not-a-real-password:with-a-colon-in-it"
    path_map:
      - remote: /downloads/complete
        local: $FULLDATA/downloads
```

**What this asserts by existing at all:** the binary starts. Before #123 this
config was unwritable; a password with a colon could only be spelled by
accident, and it would have been silently split. A startup failure here fails
the demo, which is the assertion.

Leave `acceptance-torznab`'s `api_key:` exactly as it is. It is the shorthand
spelling on a token-scheme provider, it is the form most existing deployments
use, and the demo continuing to pass with it unchanged is the back-compatibility
assertion at the config-file level.

---

## 2. Neither half of the credential reaches `GET /api/v1/providers`

**Anchor.** The download-client section, immediately after the existing pair:

```bash
  assert_not_contains "$dl_json" "api_key" "no credential field reaches the providers response"
  assert_not_contains "$dl_json" "password" "and no password field either"
```

**Add:**

```bash
  # Neither HALF of a basic credential reaches the response. The password is
  # covered above by the field NAME; these two are the VALUES, and the username
  # is the one a redaction covering only the password would leak. Half of an
  # RFC 7617 pair is still half a credential, and this repository is public.
  assert_not_contains "$dl_json" "not-a-real-password:with-a-colon-in-it" \
    "no credential VALUE reaches the providers response"
  assert_not_contains "$dl_json" "acceptance-heyarr" \
    "and no username either — half a credential is still a credential"
```

**Anchor.** The indexer section, after:

```bash
  assert_not_contains "$prov_json" "api_key" \
    "no credential field reaches the providers response"
```

**Add:**

```bash
  assert_not_contains "$prov_json" "not-a-real-key-and-nothing-will-read-it" \
    "no token VALUE reaches the providers response either"
```

---

## 3. The credential does not reach the log, at any level

**Anchor.** Wherever the run's captured stderr/log file is already available to
assert on (the demo captures it for the startup and job assertions). Placed in
the provider section, after the response assertions.

**Add:**

```bash
  # ADR-0025: asserted by SEARCHING CAPTURED OUTPUT, never by reading the code.
  # The demo runs at the configured log level; a leak that only appears at
  # debug is still a leak, so this scans everything the run produced.
  assert_not_contains "$(cat "$LOGFILE")" "not-a-real-password:with-a-colon-in-it" \
    "no credential reaches the log"
  assert_not_contains "$(cat "$LOGFILE")" "acceptance-heyarr" \
    "no credential username reaches the log"
  assert_not_contains "$(cat "$LOGFILE")" "not-a-real-key-and-nothing-will-read-it" \
    "no indexer token reaches the log"
```

Substitute the branch's own name for the captured log — `$LOGFILE` is a
placeholder for whatever the merged script calls it.

---

## 4. A wrong-shaped credential is refused at startup, by name

**Anchor.** Alongside the existing startup-refusal assertions (the ones that
prove a malformed endpoint and a missing credential stop the binary).

**Add** — a config written to a temporary path, started, and expected to fail:

```bash
  # ADR-0031: the scheme is DECLARED, so a credential written in another
  # scheme's shape is a startup error naming the field — not a line silently
  # ignored. The message must name the provider, the key and the scheme.
  refuse_config "a credential in the wrong scheme's shape" \
    "credential.token" "basic" <<'CFG'
providers:
  - name: wrong-shape
    type: transmission
    endpoint: http://127.0.0.1:9
    credential:
      token: not-a-real-token
CFG

  # The #123 case itself: an ambiguous legacy credential is refused rather than
  # guessed at. Heyarr cannot tell "user:pass" from a password containing a
  # colon, and guessing wrong is the silent corruption this issue is about.
  refuse_config "an ambiguous legacy api_key" \
    "api_key" "colon" <<'CFG'
providers:
  - name: ambiguous
    type: transmission
    endpoint: http://127.0.0.1:9
    api_key: "not-a-real-password:with-a-colon-in-it"
CFG
```

`refuse_config` is a placeholder for the branch's existing
start-and-expect-failure helper; if there is none, inline the same
`run_and_expect_failure` shape the endpoint refusals already use.

**Assert `assert_eq`, not `assert_contains`, on the exit status** — a config
refusal must exit non-zero for the *refusal's* reason, and a binary that
crashed for another one would satisfy a substring match on the message alone.

---

## Enum-like values: `assert_eq`, never `assert_contains`

Two values in this section are enum-like and must be compared for equality:

```bash
  # The scheme, if the providers response is ever extended to report it.
  # `assert_contains "$dl_json" "basic"` would pass against "not-basic",
  # against a detail string mentioning basic auth, and against the word
  # appearing anywhere else in the document.
  assert_eq "$(jq -r '.providers[] | select(.name == "acceptance-downloads") | .auth_scheme // "absent"' \
    <<<"$dl_json")" "absent" \
    "the providers response does not currently report an auth scheme"
```

That last one is written as `absent` deliberately: **this change adds no field
to any API response**, and asserting the absence is how the demo notices if a
later change adds one without an OpenAPI edit (ADR-0015).

---

## What is deliberately NOT added

- **No live Transmission.** ADR-0026 stands: the live exercise is the existing
  opt-in `HEYARR_TEST_TRANSMISSION_URL` test, and a credential assertion needing
  a real daemon would be a gate nobody runs.
- **No new provider kind for the demo.** The colon assertion works against the
  unreachable `127.0.0.1:9` endpoint the demo already uses, because everything
  being asserted happens before a socket is opened.
