# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# The provider health beat (#164) — the assertions that were vacuous until it
# existed.
#
# # Why this is its own node
#
# The claim being made is EMPTY, THEN POPULATED, and the emptiness half has to
# be deterministic or it asserts nothing. On the full demo node the health beat
# has been running since before the first API call, so "checked_at is absent"
# there is a race that is already lost. Here the node is started as a
# CONTROLLER ALONE first: the controller enqueues the pass and there is no
# worker in existence to claim it, so "never checked" is a fact about the
# system rather than a fact about how fast the machine is.
#
# It configures no fixture library and wants nothing, so it creates no Work, no
# asset and no blob — nothing here can shift a catalogue count asserted
# elsewhere. It costs three short process starts.
#
# # What was wrong before
#
# providers.HealthJobType was declared and its handler was registered at
# internal/worker/worker.go and NOTHING ENQUEUED IT. Every assertion anywhere
# that read provider health was therefore reading a value nothing ever wrote —
# unfalsifiable, and reading as coverage. It was found by a sabotage to the
# indexer client's error path failing to fire.
provider_health_beat_demo() {
  local root="$WORK/healthbeat" data lib sock
  data="$root/data"; lib="$root/library"; sock="$data/heyarr.sock"
  mkdir -p "$data" "$lib"

  cat > "$WORK/healthbeat.yaml" <<YAML
data_dir: $data
peer:
  name: acceptance-health
  site: test
log:
  level: info
  format: json
# Loopback socket, no auth, for the same reason the bare node above does it:
# this section is about a beat, not about tokens, and a fixed TCP port would
# make two runs on one machine collide.
http:
  addr: ""
  unix_socket: $sock
  auth:
    enabled: false
libraries:
  - name: films
    content_type: movie
    roots: ["$lib"]
providers:
  # A fake that answers, so "checked and healthy" is reachable, and a REAL
  # torznab client pointed at port 9 — discard, reserved, refusing connections
  # everywhere — so "checked and unreachable" is reachable too. ADR-0026: a
  # real indexer can never run here.
  - name: health-indexer
    type: fake
    capabilities: [indexer]
  - name: health-torznab
    type: torznab
    endpoint: http://127.0.0.1:9/api
    api_key: not-a-real-key-and-nothing-will-read-it
YAML

  hb_api() { curl -sS --unix-socket "$sock" "http://heyarr$1"; }
  hb_jobs() { "$BIN" --config "$WORK/healthbeat.yaml" jobs list --type provider_health --json; }
  hb_entry() { jq -c --arg n "$1" '[.providers[] | select(.name == $n)] | .[0]' <<<"$(hb_api /api/v1/providers)"; }
  # hb_entry_checked <name> — 0 once that provider has been observed at least
  # once, for wait_for. Per-provider, because the beat records each check as it
  # makes it and one provider's observation says nothing about another's.
  hb_entry_checked() { [[ "$(jq -r '.checked_at // "never"' <<<"$(hb_entry "$1")")" != "never" ]]; }
  hb_wait_ready() {
    local waited=0
    while (( waited < 600 )); do
      curl -sf --unix-socket "$sock" http://heyarr/readyz >/dev/null 2>&1 && return 0
      sleep 0.1; waited=$(( waited + 1 ))
    done
    return 1
  }

  # ---- 1. A controller alone: the pass is SCHEDULED and nothing has run it --
  local hb_ctrl hb_worker
  "$BIN" --config "$WORK/healthbeat.yaml" controller >"$root/controller.log" 2>&1 &
  hb_ctrl=$!
  if ! hb_wait_ready; then
    fail "the health-beat node never became ready"; kill -KILL "$hb_ctrl" 2>/dev/null || true; return 1
  fi

  # THE EMPTY HALF, and it is deterministic: no worker process exists.
  assert_eq "$(jq -r '.checked_at // "never"' <<<"$(hb_entry health-torznab)")" "never" \
    "before any worker has run, an indexer is never-checked rather than unhealthy"
  assert_eq "$(jq -r '.checked_at // "never"' <<<"$(hb_entry health-indexer)")" "never" \
    "and so is one that would answer"

  # THE ASSERTION #164 EXISTS FOR. A job, pending, of the right type — the row
  # that did not exist on any node for the whole of M3. Asserted through the
  # operator's own view of the queue rather than by reading SQLite, because
  # that is the view somebody debugging "why is nothing being checked" has.
  assert_eq "$(hb_jobs | jq 'length')" "1" \
    "a controller enqueues exactly one provider health pass at startup"
  assert_eq "$(hb_jobs | jq -r '.[0].state')" "pending" \
    "and it is pending, waiting for a worker rather than already spent"

  # ---- 2. A worker arrives: the pass runs and checked_at is populated -------
  "$BIN" --config "$WORK/healthbeat.yaml" worker >"$root/worker.log" 2>&1 &
  hb_worker=$!

  # Waited for on a CONDITION. This poll is not dead time: the thing it waits
  # for now arrives, which is the whole of this issue.
  local waited=0 hb_tz hb_fake
  while (( waited < 300 )); do
    hb_tz=$(hb_entry health-torznab)
    [[ "$(jq -r '.checked_at // "never"' <<<"$hb_tz")" != "never" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if [[ "$(jq -r '.checked_at // "never"' <<<"$hb_tz")" == "never" ]]; then
    fail "checked_at never became populated — the health pass was enqueued and never ran"
    kill -KILL "$hb_ctrl" "$hb_worker" 2>/dev/null || true; return 1
  fi
  pass "a worker claims the beat's job and checked_at becomes populated"

  # A SECOND PROVIDER IS A SECOND OBSERVATION (#207). The wait above is about
  # health-torznab; the two assertions below are about health-indexer, which the
  # pass checks separately and therefore records at its own moment. Asserting
  # one provider's observation on the strength of another's is the same
  # wait-for-A-assert-B shape, with the subjects side by side rather than in
  # sequence — and it would fail as "healthy: null", reading as a health check
  # that got the wrong answer rather than one that had not run.
  wait_for "the health beat never recorded an observation for provider 'health-indexer' — the two assertions below are about what that check saw" \
    300 hb_entry_checked health-indexer
  hb_fake=$(hb_entry health-indexer)
  assert_eq "$(jq -r '.healthy' <<<"$hb_fake")" "true" \
    "a provider that answers is reported healthy, having actually been asked"
  assert_eq "$(jq -r '.version' <<<"$hb_fake")" "fake" \
    "and reports the version its handshake returned"

  # VACUOUS ASSERTION 1, now falsifiable: it is made against a check that has
  # demonstrably run, on this node, in this run.
  assert_eq "$(jq -r '.healthy' <<<"$hb_tz")" "false" \
    "an unreachable indexer is not reported as healthy"
  # ...and the refusal names the network rather than the credential. The API
  # key three lines up in this file is real-looking and wrong; saying "the key
  # was rejected" for a connection that was never made is the report that
  # sends an operator to the wrong page.
  assert_eq "$(jq -r '.detail' <<<"$hb_tz")" "unreachable" \
    "and says so as a network failure rather than a credential one"
  assert_not_contains "$(jq -r '.detail' <<<"$hb_tz")" "not implemented" \
    "the torznab kind is a real client rather than a placeholder"

  # VACUOUS ASSERTION 2 (#131), now falsifiable: THE CACHE MUST NOT
  # MANUFACTURE AN OBSERVATION. Check() refreshes the capabilities cache and a
  # capabilities document is where the reported version comes from. This
  # endpoint refuses connections and has never produced one, so a version
  # appearing here would mean a remembered document passed off as something
  # just observed.
  #
  # assert_eq on "absent", not assert_contains: version is enum-like, and a
  # substring match on an absent field matches nothing and passes.
  assert_eq "$(jq -r '.version // "absent"' <<<"$hb_tz")" "absent" \
    "an indexer that has never handshaked reports no version"

  # ---- 3. A second pass, from the beat, on a restart -----------------------
  #
  # Procured by restarting the CONTROLLER rather than by waiting out the beat
  # interval: the interval is a minute (see internal/controller/healthbeat.go
  # for why a minute) and this script has a budget. A restart is a real beat
  # enqueue on the real path — startProviderHealth enqueues at startup — and it
  # asserts the thing a fixed sleep could not: that the dedupe key which makes
  # two roles produce one check has not quietly turned the beat into a
  # one-shot.
  local hb_first
  hb_first=$(jq -r '.checked_at' <<<"$hb_tz")
  kill -TERM "$hb_ctrl" 2>/dev/null || true; wait "$hb_ctrl" 2>/dev/null || true
  "$BIN" --config "$WORK/healthbeat.yaml" controller >"$root/controller2.log" 2>&1 &
  hb_ctrl=$!
  if ! hb_wait_ready; then
    fail "the health-beat node did not come back"; kill -KILL "$hb_ctrl" "$hb_worker" 2>/dev/null || true; return 1
  fi

  # The two halves are asserted SEPARATELY and in this order, because they
  # fail for completely different reasons and a single assertion over both
  # would report whichever one it noticed. "A second job exists" is a claim
  # about the beat; "the observation moved" is a claim about the check.
  waited=0
  while (( waited < 300 )); do
    [[ "$(hb_jobs | jq 'length')" == "2" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  assert_eq "$(hb_jobs | jq 'length')" "2" \
    "the beat enqueues a second pass on a later start, rather than being a one-shot"

  waited=0
  local hb_second
  while (( waited < 300 )); do
    hb_second=$(hb_entry health-torznab)
    [[ "$(jq -r '.checked_at' <<<"$hb_second")" != "$hb_first" ]] && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if [[ "$(jq -r '.checked_at' <<<"$hb_second")" == "$hb_first" ]]; then
    fail "the second pass reported the first pass's observation — a replay, not a check"
    kill -KILL "$hb_ctrl" "$hb_worker" 2>/dev/null || true; return 1
  fi
  pass "and the second pass records a new observation rather than the first one again"

  # VACUOUS ASSERTION 3 (#131), now falsifiable: A SECOND CHECK IS A SECOND
  # OBSERVATION, NOT A REPLAY OF THE FIRST. Decision 3 in
  # internal/indexers/client.go makes the health check WRITE the capabilities
  # cache and never read it. If that inverted, an indexer that answered once
  # would stay healthy for the TTL after it stopped answering — and here,
  # where it has never answered at all, the report must be false on every pass
  # rather than only on the first.
  assert_eq "$(jq -r '.healthy' <<<"$hb_second")" "false" \
    "a second health pass observes the indexer again rather than replaying the first"
  assert_eq "$(jq -r '.version // "absent"' <<<"$hb_second")" "absent" \
    "and still reports no version on the second pass"

  kill -TERM "$hb_ctrl" "$hb_worker" 2>/dev/null || true
  wait "$hb_ctrl" "$hb_worker" 2>/dev/null || true
}


