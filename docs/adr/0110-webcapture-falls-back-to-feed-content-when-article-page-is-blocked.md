# ADR-0110 — Webcapture falls back to the feed's own full content when an article page is blocked

**Amends:** ADR-0063 (web-capture source scheme)

**Status:** accepted

---

## Problem

Some publishers return HTTP 403 (or 401/404/410/451) to every non-browser client.
The webcapture client fetches the article page; on a blocking response it has
historically failed the transfer, leaving the want permanently MISSING even though
the RSS or Atom feed the work was discovered from already carries the article's
full HTML body in `<content:encoded>` (RSS) or `<content type="html">` (Atom).

## Decision

When an article page returns a **qualifying blocking status** (401, 403, 404, 410,
or 451 — not 5xx, not network/timeout errors, which must keep retrying), and the
source encodes a feed URL (see encoding below), the webcapture client:

1. Fetches the feed at the embedded URL using the same `heyarr/…` User-Agent.
2. Parses it with `internal/webfeedparse` (shared package, no import-cycle).
3. Finds the entry whose guid/link matches the article URL.
4. Checks that the entry carries **substantive full content** (`<content:encoded>`
   or Atom `<content>`). A `<description>` or `<summary>` element is explicitly
   excluded — it is a summary, not an article.
5. Builds standalone HTML from the feed's body via the existing `inline()`/`render`
   path (same size limit, same self-containment semantics as a direct capture).
6. Marks provenance: `<meta name="heyarr-capture-source" content="feed">`.

If the fallback itself fails (feed unreachable, entry not found, content absent),
the transfer fails with the **original page error**, not the fallback error.

## Source encoding

ADR-0063 defined the source as `webcapture:<articleURL>`.  This ADR extends it to:

```
webcapture:<articleURL>\x00feed:<feedURL>
```

A null byte (`\x00`) is the separator. A null byte is never valid in a URL
(RFC 3986 §2), so the presence of `\x00` unambiguously distinguishes the new form.
Legacy sources (no null byte) parse exactly as before — no fallback, no feed fetch.

The `sourceKey()` SHA-256 hashes the full string, so the same article URL but a
different feed URL produces a different transfer ID; this is intentional and
acceptable.

## Rejected alternative — store feed content in item attributes

An earlier sketch stored the feed's HTML in an item attribute at poll time so the
download client could retrieve it without a second network call. This was rejected
because:

- **DB bloat.** A high-volume RSS feed repointed every held item's attributes on
  every poll, writing kilobytes of HTML per article into the items table each time.
- **Stale content.** The stored snapshot ages immediately; the fallback path fetches
  fresh feed content at capture time, which is always more current.
- **Schema change.** Storing arbitrary-length HTML in an attribute exceeds the
  intended attribute size budget and would require a migration.

## Qualifying statuses

Only blocking statuses trigger the fallback: **401, 403, 404, 410, 451**.

5xx and network errors are transient — the job will retry. Falling back on a 500
would produce a stale feed snapshot for what is probably a temporarily unhealthy
host, and would suppress the retry that would otherwise succeed.
