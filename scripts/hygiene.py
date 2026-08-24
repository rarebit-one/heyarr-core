"""Fail if a tracked file names a real machine, network, premises or person.

Two lists, for two different reasons (see the header comment in each):

  scripts/hygiene.denylist  shapes, in plain text — a home directory, a
                            routable address, a LAN suffix. A shape discloses
                            nothing, and it is the half that catches the NEXT
                            leak, whose name nobody has listed yet.
  scripts/hygiene.digests   retired proper nouns, as SHA-256 of the lowercased
                            token. A name DOES disclose something, so this
                            guard recognises them without spelling them.

Both passes run here rather than as `git grep` because the address rule needs a
negative lookahead to say "routable, but not a documentation range", which
POSIX ERE cannot express, and because findings must be reported WITHOUT their
matched text: CI logs for a public repo are public, so a guard that echoed the
offending line would leak the very string it just caught.

# Two surfaces, and the second one is not scanned by default

`--issues` runs the same two passes over ISSUE AND PULL REQUEST titles and
bodies instead of over tracked files.

It exists because scanning only tracked files is what let a retired host name
survive a scrub that everybody believed was complete (#230). `git grep -i` on a
clean tree returned nothing, which is exactly the reassurance that made the
issue tracker invisible — while the same name sat in three issue TITLES, one of
them open, where it appears in search results, notification mail, the issues
list and every cross-repo reference.

It is a separate mode rather than part of the default because it needs `gh` and
the network, and the tracked-file guard has to stay offline and deterministic:
a pre-push hook or a CI gate that fails when GitHub is slow is a gate that gets
removed.
"""

import hashlib
import re
import subprocess
import sys
from pathlib import Path

# Only alphanumeric runs are candidate tokens; every other byte is a separator,
# so a name is found inside a path, a URL, a hostname or an ssh command.
TOKEN = re.compile(rb"[A-Za-z][A-Za-z0-9]{2,}")
# ...and a name glued into an identifier still has to be seen, so each token is
# additionally split at camelCase and letter/digit boundaries. This runs on the
# ORIGINAL casing and lowercases afterwards: splitting a token that has already
# been lowercased finds nothing, because that is where the boundaries were.
SUBTOKEN = re.compile(r"[A-Z]+(?![a-z])|[A-Z][a-z]+|[a-z]+|[0-9]+")

# The fields of an issue or pull request that are scanned, as a constant so the
# self-test can assert what they are. A scan that quietly stopped reading
# bodies would report CLEAN, and clean is the answer a reader trusts.
ISSUE_FIELDS = ("title", "body")


def load(path, want_digest):
    """Read a list file, dropping comments and blanks."""
    out = []
    for lineno, raw in enumerate(path.read_text().splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        if want_digest:
            digest, _, label = line.partition("  ")
            digest = digest.strip().lower()
            if not re.fullmatch(r"[0-9a-f]{64}", digest):
                sys.exit(f"hygiene: {path}:{lineno}: not a sha256 digest")
            out.append((digest, label.strip()))
        else:
            try:
                out.append((re.compile(line, re.IGNORECASE), line))
            except re.error as exc:
                sys.exit(f"hygiene: {path}:{lineno}: bad regex: {exc}")
    return out


def tokens(blob):
    """Every lowercased candidate token in a file, with its line number."""
    line = 1
    pos = 0
    for m in TOKEN.finditer(blob):
        line += blob.count(b"\n", pos, m.start())
        pos = m.start()
        raw = m.group().decode("ascii")
        word = raw.lower()
        yield word, line
        for sub in SUBTOKEN.findall(raw):
            sub = sub.lower()
            if sub != word:
                yield sub, line


def scan_text(text, blob, patterns, wanted):
    """Both passes over one document. Returns (line, reason) findings.

    Shared by the file surface and the issue surface so the two can never drift
    into recognising different things — the failure this guard is for is a name
    that one check knows about and another does not.
    """
    found = []
    for word, line in tokens(blob):
        label = wanted.get(hashlib.sha256(word.encode()).hexdigest())
        if label:
            found.append((line, f"deny-list digest — {label}"))
    if text is not None:
        for lineno, content in enumerate(text.splitlines(), 1):
            for rx, src in patterns:
                if rx.search(content):
                    found.append((lineno, f"deny-list shape /{src}/"))
    return found


def self_test():
    """Prove both passes still catch something, on every run.

    A guard is trusted for the runs on which it says CLEAN, and this one will
    say clean for the rest of the repository's life once the current findings
    are dealt with. From that day on, a scan that had silently stopped matching
    would be indistinguishable from a clean repository — which is #187's
    vacuous assertion, wearing a hygiene guard's clothes.

    So it checks itself against a SYNTHETIC name whose digest is computed here
    rather than read from the list. Nothing real is spelled, and the whole
    token → subtoken → digest → match path is exercised.
    """
    fake = "zzsynthetichostzz"
    wanted = {hashlib.sha256(fake.encode()).hexdigest(): "self-test"}
    doc = f"a line naming {fake} inside it".encode()
    if not scan_text(None, doc, [], wanted):
        sys.exit("hygiene: SELF-TEST FAILED — the digest pass no longer matches a known name")

    # ...and glued into an identifier, which is the case SUBTOKEN exists for.
    glued = f"someIdentifier{fake.capitalize()}Suffix".encode()
    if not scan_text(None, glued, [], wanted):
        sys.exit("hygiene: SELF-TEST FAILED — the digest pass no longer splits identifiers")

    rx = [(re.compile(r"forbidden-shape-[0-9]+", re.IGNORECASE), "self-test")]
    if not scan_text("a forbidden-shape-42 here", b"", rx, {}):
        sys.exit("hygiene: SELF-TEST FAILED — the shape pass no longer matches a pattern")

    # A clean document must stay clean, or every run is a false positive.
    if scan_text("nothing to see", b"nothing to see", rx, wanted):
        sys.exit("hygiene: SELF-TEST FAILED — a clean document was reported as a finding")

    if set(ISSUE_FIELDS) != {"title", "body"}:
        sys.exit(f"hygiene: SELF-TEST FAILED — the issue scan reads {ISSUE_FIELDS}, and a scan "
                 "that skips a field reports CLEAN rather than reporting less")


def scan_issues(patterns, wanted):
    """The issue and pull request surface: titles and bodies.

    NUMBERS ONLY in the output, and the field they were found in — never the
    title, never the body, never the matched token. An issue title is a public
    string, but a guard that prints it is a guard that republishes every leak
    it finds into a CI log, which is the mistake scripts/hygiene.digests exists
    to avoid one level down.

    COMMENTS ARE NOT SCANNED, and that is a stated gap rather than an
    oversight: reading them is one request per issue, which turns a two-second
    check into minutes, and the surface that matters most — what a reader and a
    search engine see first — is the title. Widen this if a leak is ever found
    in a comment, and say so here.
    """
    import json

    out = subprocess.run(
        ["gh", "api", "--paginate",
         "repos/{owner}/{repo}/issues?state=all&per_page=100",
         "--jq", "[.[] | {number, title, body, pr: (.pull_request != null)}]"],
        capture_output=True, text=True,
    )
    if out.returncode != 0:
        sys.exit("hygiene: could not read the issue tracker — is `gh` authenticated?\n"
                 + out.stderr.strip())

    items = []
    for chunk in out.stdout.strip().splitlines():
        if chunk:
            items.extend(json.loads(chunk))

    findings = []
    for item in items:
        kind = "PR" if item.get("pr") else "issue"
        for field in ISSUE_FIELDS:
            value = item.get(field) or ""
            if not value:
                continue
            for _, why in scan_text(value, value.encode(), patterns, wanted):
                findings.append((f"{kind} #{item['number']}", field, why))
    return findings, len(items)


def main():
    root = Path(__file__).resolve().parent.parent
    patterns = load(root / "scripts/hygiene.denylist", want_digest=False)
    digests = load(root / "scripts/hygiene.digests", want_digest=True)
    if not patterns or not digests:
        sys.exit("hygiene: a deny-list is empty — that is almost certainly a mistake")
    self_test()
    wanted = {d: label for d, label in digests}

    if "--issues" in sys.argv[1:]:
        findings, count = scan_issues(patterns, wanted)
        if findings:
            print(f"hygiene: FORBIDDEN content in {len(set(findings))} place(s) "
                  f"across {count} issues and pull requests:", file=sys.stderr)
            for where, field, why in sorted(set(findings)):
                print(f"  {where} ({field}): {why}", file=sys.stderr)
            print(
                "\nThis repo is public. Edit the item to describe the SHAPE of the thing\n"
                "rather than its identity. Note that GitHub keeps edit history, so an edit\n"
                "reduces what a scraper hits without removing the string — a TITLE is worth\n"
                "editing because it appears in search results, notification mail and every\n"
                "cross-reference; a body is a judgement call.\n"
                "Do not quote the offending string in the fix, the commit message or the PR.",
                file=sys.stderr,
            )
            return 1
        print(f"hygiene: {len(patterns)} shape pattern(s) and {len(digests)} name digest(s) "
              f"checked against {count} issues and pull requests (titles and bodies) — clean")
        return 0

    files = subprocess.run(
        ["git", "-C", str(root), "ls-files", "-z"],
        capture_output=True, check=True,
    ).stdout.split(b"\0")

    findings = []
    scanned = 0
    for name in files:
        if not name:
            continue
        path = root / name.decode()
        try:
            blob = path.read_bytes()
        except (OSError, ValueError):
            continue
        if b"\0" in blob[:8192]:  # binary; nothing readable to leak
            continue
        scanned += 1
        rel = name.decode()

        try:
            text = blob.decode("utf-8")
        except UnicodeDecodeError:
            text = None
        for line, why in scan_text(text, blob, patterns, wanted):
            findings.append((rel, line, why))

    if findings:
        print(f"hygiene: FORBIDDEN content in {len(findings)} place(s):", file=sys.stderr)
        # The match itself is never printed. A public CI log that echoed the
        # offending line would republish exactly what this guard exists to stop.
        for rel, line, why in sorted(set(findings)):
            print(f"  {rel}:{line}: {why}", file=sys.stderr)
        print(
            "\nThis repo is public. Replace the name with a placeholder that describes\n"
            "the SHAPE of the thing rather than its identity — `Site A` / `peer-a` for\n"
            "premises, `the reference host` / `<host>` for machines, `/srv/media/...`\n"
            "for library paths. If a finding is a genuine false positive, narrow the\n"
            "pattern in scripts/hygiene.denylist and say why in the commit message.\n"
            "Do not quote the offending string in the fix, the commit message or the PR.",
            file=sys.stderr,
        )
        return 1

    print(
        f"hygiene: {len(patterns)} shape pattern(s) and {len(digests)} name digest(s) "
        f"checked against {scanned} tracked files — clean"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
