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


def main():
    root = Path(__file__).resolve().parent.parent
    patterns = load(root / "scripts/hygiene.denylist", want_digest=False)
    digests = load(root / "scripts/hygiene.digests", want_digest=True)
    if not patterns or not digests:
        sys.exit("hygiene: a deny-list is empty — that is almost certainly a mistake")
    wanted = {d: label for d, label in digests}

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

        for word, line in tokens(blob):
            label = wanted.get(hashlib.sha256(word.encode()).hexdigest())
            if label:
                findings.append((rel, line, f"deny-list digest — {label}"))

        try:
            text = blob.decode("utf-8")
        except UnicodeDecodeError:
            continue
        for lineno, content in enumerate(text.splitlines(), 1):
            for rx, src in patterns:
                if rx.search(content):
                    findings.append((rel, lineno, f"deny-list shape /{src}/"))

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
