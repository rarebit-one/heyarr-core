# `scripts/acceptance.sh` — the section for #130 (the search scheduler)

`scripts/acceptance.sh` is held by several branches at once, so this change is
written down rather than applied. Everything below is a whole new block; no
existing assertion is edited or moved.

## Anchor

Insert **immediately after** the last assertion of the existing
`note "  the search job (§60, §63, M3-12)"` section — i.e. after the
`a completed search reaches the event log` pass/fail block (currently around
line 1830, ending the `if grep -q 'acquisition.search_completed' …` block) and
**before** the next `note "  …"` heading.

That position matters for the same reason the M3-12 section's own comment gives:
wanting content Heyarr has never seen CREATES A WORK, which shifts every catalog
count. The block below wants a work the fixture library already holds, and it
sits below the counts regardless.

## Preconditions it relies on (all already true in the file)

- The `full.yaml` config already registers a **fake indexer** and a real
  Torznab entry pointing at nothing (ADR-0026 — no real indexer, ever).
- `api`, `api_all`, `assert_eq`, `pass`, `fail`, `note` are already defined.
- The full-library controller has been running since the top of
  `full_library_demo`, so the search beat's startup pass has already happened
  and only its 30-second ticks remain.

## The block

```bash
  note "  the search scheduler (#130) — deciding WHEN to look"
  # #130's actual defect was not that searching was impossible. It was that a
  # want in MISSING with no search in flight was INDISTINGUISHABLE from one
  # being worked on: it sat there forever and nothing in the system said so.
  #
  # Every other search assertion in this file begins with a human POSTing
  # /search. This one must not touch that endpoint at all — the whole claim is
  # that nobody has to.
  local sched_work sched_want sched_id
  sched_work=$(api_all /api/v1/works '.items[] | select(.title == "Blue Harvest") | .id' | head -1)
  if [[ -z "$sched_work" ]]; then
    fail "the fixture library has no 'Blue Harvest' work for the scheduler demo to want"
    return 1
  fi
  # A DIFFERENT profile from the M3-12 section's want: desired_items is unique
  # over (target, profile), so re-using 'living-room' here would be a 409 and
  # the section would assert on an error document.
  sched_want=$(api /api/v1/desired -X POST -H 'Content-Type: application/json' \
    -d "{\"work_id\":\"$sched_work\",\"quality_profile\":\"archival\"}")
  sched_id=$(jq -r '.id' <<<"$sched_want")
  assert_eq "$(jq -r '.acquisition.state' <<<"$sched_want")" "MISSING" \
    "a fresh want starts MISSING"

  # THE ASSERTION THIS SECTION EXISTS FOR. No /search is ever issued for
  # $sched_id. The beat ticks every 30s (searchBeatInterval), so the bound is
  # two ticks plus slack — and it waits for the CONDITION rather than sleeping
  # a fixed duration, which is the house rule everywhere else in this file.
  #
  # It waits on a search_release JOB carrying this want, not on the want's
  # state: a search that has already run and returned the want to rest leaves
  # the state exactly where it started (see the M3-12 section's two flakes on
  # precisely that), so the state is not an arrival condition.
  local sched_waited=0 sched_jobs=0
  while (( sched_waited < 900 )); do
    sched_jobs=$(api "/api/v1/jobs?type=search_release" |
      jq --arg id "$sched_id" '[.items[] | select(.payload.desired_item_id == $id)] | length')
    (( sched_jobs >= 1 )) && break
    sleep 0.1; sched_waited=$(( sched_waited + 1 ))
  done
  if (( sched_jobs >= 1 )); then
    pass "a MISSING want nobody asked about is searched unprompted (#130)"
  else
    fail "a MISSING want was never searched — nothing decides when to look"
  fi

  # Invariant 9, from the outside. The want is still resting and still due
  # nothing new, so however many beats have ticked since, there is exactly ONE
  # search job for it. assert_eq on the count, not a contains: "at least one"
  # is the assertion a duplicate-producing scheduler passes.
  assert_eq "$(api "/api/v1/jobs?type=search_release" |
    jq --arg id "$sched_id" '[.items[] | select(.payload.desired_item_id == $id)] | length')" "1" \
    "and repeated scheduler passes produce exactly one search for it (invariant 9)"

  # The M3-12 want, created earlier in this section, must ALSO have exactly one
  # search — the operator's POST and the beat must not each produce one.
  assert_eq "$(api "/api/v1/jobs?type=search_release" |
    jq --arg id "$search_id" '[.items[] | select(.payload.desired_item_id == $id)] | length')" "1" \
    "an operator's search and a scheduled search for one want are one job, not two"
```

## Notes for whoever applies it

- **`assert_eq`, never `assert_contains`, for the enum-like values** — the
  `MISSING` state above and both job counts are compared with `assert_eq`.
- The 90-second bound (`sched_waited < 900`, at 0.1s) is generous against a
  30-second beat. Measured cost on three runs of the current file: the want is
  searched on the first tick, so the loop exits in well under a second and the
  demo stays at its measured 78–85s against a 240s budget.
- If `GET /api/v1/jobs` does not expose `payload` in its item shape, substitute
  a filter on `dedupe_key == "search:" + $id` — the dedupe key is
  `acquisition.SearchDedupeKey`, which is stable and is exactly what the
  one-job claim is about. Do **not** substitute a filter on the total number of
  `search_release` jobs: other wants in this file have their own.
