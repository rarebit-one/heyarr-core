"""Fail if something this repository claims to do is not exercised by `make demo`.

The reasoning, the format and the five defects that prompted it are in
scripts/claims.list. This file is the mechanism and deliberately holds no
opinions of its own: every claim, its state and its evidence live in the ledger,
because a guard whose rules are in its implementation is a guard nobody edits.

# Why a transcript rather than instrumentation

The demo is the only thing in this repository that drives the real binary end to
end, and a claim is proven by that binary having done something. Reading what it
printed keeps the check outside scripts/acceptance.sh — which matters because
that file is single-owner while a branch is working on it, and a guard that
forced every feature branch to touch it would be a guard that caused conflicts
instead of catching defects.

# Why absence of evidence is a failure and not a warning

Because that is the entire failure mode. Five mechanisms shipped green with no
caller; each of them would have produced a warning that scrolled past. A proven
claim whose evidence has gone means either the feature stopped running or
somebody changed the words it prints — and both of those want a human, now.
"""

from __future__ import annotations

import sys
from dataclasses import dataclass, field
from pathlib import Path

LEDGER = Path(__file__).with_name("claims.list")

# Where the demo stops asserting and starts narrating.
#
# scripts/acceptance.sh ends with an epilogue describing what the run proved and
# what it did not, and that epilogue prints UNCONDITIONALLY — it is prose, not a
# consequence of anything having happened. So a claim whose evidence appears
# only after this line is a claim matched against narration, which is precisely
# the "reads as coverage without being it" failure this whole file exists for.
#
# This is not hypothetical. The first version of the ledger used `took the PIECE
# path` for cooperative acquisition; a sabotage run that deleted the entire
# swarm section left that claim passing, because the phrase survived in the
# epilogue's own description of the section that no longer ran.
EPILOGUE_MARKER = "what this run proves, and what it does not"

PROVEN = "proven"
PENDING = "pending"


@dataclass
class Claim:
    ident: str
    line: int
    state: str = ""
    ref: str = ""
    why: str = ""
    issue: str = ""
    evidence: str = ""
    # needs names a machine capability (ffmpeg, ffprobe, ...) the evidence
    # cannot appear without. A proven claim is then exempt on a run whose
    # epilogue records that capability as absent — and ONLY then. See the
    # ledger header.
    needs: str = ""
    problems: list[str] = field(default_factory=list)


def parse(text: str) -> list[Claim]:
    """Read the ledger into claims, keeping line numbers for the error messages.

    Unknown keys are an error rather than being ignored. A typo in a key would
    otherwise leave a claim with no evidence, which this guard would then report
    as a missing feature — sending somebody to look at working code.
    """
    claims: list[Claim] = []
    known = {"state", "ref", "why", "issue", "evidence", "needs"}
    for number, raw in enumerate(text.splitlines(), start=1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        key, _, value = line.partition(" ")
        value = value.strip()
        if key == "claim":
            claims.append(Claim(ident=value, line=number))
            continue
        if not claims:
            sys.exit(f"claims.list:{number}: {key!r} before any `claim`")
        if key not in known:
            sys.exit(f"claims.list:{number}: unknown key {key!r}")
        setattr(claims[-1], key, value)
    return claims


def validate(claims: list[Claim]) -> list[str]:
    """Check the ledger itself, before checking anything against it."""
    errors: list[str] = []
    seen: set[str] = set()
    for claim in claims:
        where = f"claims.list:{claim.line}"
        if not claim.ident:
            errors.append(f"{where}: a claim with no id")
        if claim.ident in seen:
            errors.append(f"{where}: duplicate claim {claim.ident!r}")
        seen.add(claim.ident)
        if claim.state not in (PROVEN, PENDING):
            errors.append(f"{where}: {claim.ident} has state {claim.state!r}, want proven or pending")
        if not claim.evidence:
            errors.append(f"{where}: {claim.ident} names no evidence")
        if not claim.why:
            errors.append(f"{where}: {claim.ident} says no why")
        # A pending claim without an issue is a gap nobody is carrying.
        if claim.state == PENDING and not claim.issue:
            errors.append(f"{where}: {claim.ident} is pending and names no issue")
    return errors


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print("usage: claims.py <demo-transcript>", file=sys.stderr)
        return 2

    claims = parse(LEDGER.read_text(encoding="utf-8"))
    if errors := validate(claims):
        for error in errors:
            print(f"claims: {error}", file=sys.stderr)
        return 2

    transcript = Path(argv[1]).read_text(encoding="utf-8", errors="replace")

    # Everything before the epilogue. An empty body means the demo never got
    # that far, and every claim would then look like narration — so the split
    # falls back to the whole transcript rather than inventing findings.
    head, marker, _ = transcript.partition(EPILOGUE_MARKER)
    body = head if marker else transcript

    # A claim that needs a capability this machine lacks is exempt, but only
    # on the strength of the epilogue's own capability ledger saying so —
    # the line scripts/acceptance.sh prints for every not_exercised block.
    # Absent that line, the claim is judged like any other: a run on an
    # equipped machine that stops producing the evidence still fails.
    def unavailable(c: Claim) -> bool:
        return bool(c.needs) and f"absent capability: {c.needs} " in transcript

    missing = [
        c
        for c in claims
        if c.state == PROVEN and c.evidence not in transcript and not unavailable(c)
    ]
    narrated = [
        c
        for c in claims
        if c.state == PROVEN and c.evidence in transcript and c.evidence not in body
    ]
    pending = [c for c in claims if c.state == PENDING]
    # A pending claim whose evidence HAS appeared is not a failure — it is the
    # good news that somebody wired it up. Reported loudly, because the ledger
    # is now out of date and the guard is not protecting a feature that works.
    arrived = [c for c in pending if c.evidence in transcript]

    proven = sum(1 for c in claims if c.state == PROVEN)
    print(f"claims: {proven} proven, {len(pending)} pending, checked against {argv[1]}")

    if pending:
        print("\nclaims: mechanisms with no path from a running binary:")
        for claim in pending:
            print(f"  {claim.ident}  (#{claim.issue})")
            print(f"      {claim.why}")

    if arrived:
        print("\nclaims: PENDING CLAIMS THAT NOW HAVE EVIDENCE — move them to proven:")
        for claim in arrived:
            print(f"  {claim.ident}  (#{claim.issue}) — found {claim.evidence!r}")
        return 1

    if narrated:
        print(
            "\nclaims: EVIDENCE FOUND ONLY IN THE EPILOGUE, WHICH PRINTS REGARDLESS:",
            file=sys.stderr,
        )
        for claim in narrated:
            print(f"  {claim.ident}  ({claim.ref})", file=sys.stderr)
            print(f"      matched: {claim.evidence!r}", file=sys.stderr)
        print(
            "\n  That is narration, not evidence — the claim would pass against a run\n"
            "  that did nothing. Take the string from an assert_* description or from\n"
            "  a log line the code itself emits.",
            file=sys.stderr,
        )
        return 1

    if missing:
        print("\nclaims: PROVEN CLAIMS WITH NO EVIDENCE IN THIS RUN:", file=sys.stderr)
        for claim in missing:
            print(f"  {claim.ident}  ({claim.ref})", file=sys.stderr)
            print(f"      wanted: {claim.evidence!r}", file=sys.stderr)
            print(f"      why:    {claim.why}", file=sys.stderr)
        print(
            "\n  Either the feature stopped running, or the demo stopped saying so.\n"
            "  Both need a person. If the wording changed deliberately, update the\n"
            "  evidence string in scripts/claims.list in the same commit.",
            file=sys.stderr,
        )
        return 1

    print("\nclaims: every proven claim was exercised by this run")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
