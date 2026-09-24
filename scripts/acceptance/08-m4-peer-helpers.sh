# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# ---------------------------------------------------------------------------
# THE SECOND PEER: placement, proven (§56, §64, M4-11, ADR-0010, ADR-0027)
# ---------------------------------------------------------------------------
#
# This is the section the milestone exists for. Everything above it runs on ONE
# peer, where placement is satisfied the moment content is because there is
# nowhere else for bytes to be — and every claim about `converging` up to this
# point was a claim about a table test with a synthetic peer set.
#
# Here there are two real nodes. Two data directories, two databases, two
# content stores, two Ed25519 identities, and an mTLS peer surface between them
# that each has enrolled the other's public key on. A blob is DELETED from one
# of them, the gap is observed from the other node's API as `converging`, the
# transfer runs, and the same API is asked again.
#
# # Why it is its own pair of nodes
#
# The same reason the health beat is: isolation. It configures its own two-file
# library under $WORK, so it creates no Work, no asset and no blob in the demo
# catalogue and cannot shift a count asserted anywhere else — the trap recorded
# at `note "  the CLI (M1-17)"`. It also needs a node whose peer set it can
# GROW, and the full demo node has been a fabric of one since before its first
# API call.
#
# # Why the ports are :0
#
# The peer surface binds a real TCP port, and a fixed one makes two runs on one
# machine collide — which is the same lesson `http.addr: ""` records above. It
# binds :0 and logs the address it actually got; the helper below reads it back
# out of the log, so nothing here guesses a port.
#
# # Every enum check is assert_eq
#
# "not_satisfied" CONTAINS "satisfied". A substring match on a satisfaction
# value passes for the opposite meaning, and it has shipped in this file once
# already. The same applies to the garbage-collection refusals at the end:
# "no_other_peer" contains "other_peer".
#
# # Why it runs twice (M4-16)
#
# Once with each node as `heyarr all`, and once with each node as three separate
# role processes. ADR-0002 says the roles must be independently runnable as OS
# processes, and the split-process section below has kept that honest for the
# single-node path since Milestone 1. Everything this milestone added — the peer
# surface, enrolment, inventory exchange, the transfer, the collector's
# placement precondition — is new surface that the split has never been run
# against, and a split nothing exercises is decorative.

# peer_listen_addr reads the address a node's peer surface actually bound.
peer_listen_addr() { # logfile
  local waited=0 line
  while (( waited < 600 )); do
    line=$(grep '"msg":"peer surface listening"' "$1" 2>/dev/null | tail -1)
    if [[ -n "$line" ]]; then jq -r '.addr' <<<"$line"; return 0; fi
    sleep 0.1; waited=$(( waited + 1 ))
  done
  return 1
}

# peer_holds counts a blob's copies in a node's content store.
#
# By digest, over the store's own fanout, rather than by reconstructing the
# path: the layout is private to the storage fabric (ADR-0006) and a test that
# hard-coded it would be asserting on an implementation detail it does not own.
peer_holds() { # cas-root blob-hash
  find "$1/blobs" -name "${2#blake3:}" -type f 2>/dev/null | wc -l | tr -d ' '
}

# replicate_job_succeeded — 0 once node B has a replicate_blob job in
# `succeeded`. That is the LAST of the things the transfer section asserts:
# bytes, then the transfer record, then the log line, then the job state.
# Waiting on it covers all four; waiting on the bytes covers only the first
# (#207). Reads node B through api_b, which the two-peer arc defines.
replicate_job_succeeded() {
  [[ "$(api_b "/api/v1/jobs?type=replicate_blob" | jq -r '[.items[] | select(.state == "succeeded")] | length > 0')" == "true" ]]
}

# start_peer_node starts one two-peer node in the mode this pass is exercising,
# and reports the PIDs it started in NODE_PIDS.
#
# `all` and three role processes are the same node from every angle this section
# asserts from. That is ADR-0002's claim, and the only thing that keeps it true
# is running the milestone's arc under both rather than asserting it once under
# one and trusting the split — which is how a role that stopped being
# independently runnable would go unnoticed until somebody deployed it.
start_peer_node() { # config logfile mode
  local cfg=$1 log=$2 mode=$3 role
  NODE_PIDS=()
  if [[ "$mode" == "all" ]]; then
    "$BIN" --config "$cfg" all >>"$log" 2>&1 &
    NODE_PIDS+=($!)
  else
    for role in controller worker peer; do
      "$BIN" --config "$cfg" "$role" >>"$log" 2>&1 &
      NODE_PIDS+=($!)
    done
  fi
  PEER_PIDS+=("${NODE_PIDS[@]}")
}

# stop_peer_node stops one node and WAITS for it, so that "the peer is down" is
# a fact rather than a signal that has been sent. A refusal asserted against a
# peer that is still finishing its last request proves nothing.
stop_peer_node() { # pid...
  local p
  for p in "$@"; do kill -TERM "$p" 2>/dev/null || true; done
  for p in "$@"; do wait "$p" 2>/dev/null || true; done
}

# ctrl_blob_bytes counts blob-content reads served by a node's CLIENT API,
# from the chi route pattern on its own /metrics.
#
# It is used to prove an instrument is LIVE, and never on its own to prove a
# negative. See peer_blob_reads for why that distinction is the whole point of
# this pair of helpers.
#
# The label is matched with `grep -F`, not a regex. The route pattern contains
# `{hash}`, and a brace in a regex is an interval operator on some awks and a
# literal on others — the same class of portability trap as `stat -f`, which is
# a valid GNU flag meaning something else entirely. A fixed-string match means
# the same thing on darwin and on the Linux runner.
ctrl_blob_bytes() { # metrics-text
  local rows
  rows=$(grep '^heyarr_http_requests_total{' <<<"$1" |
    grep -F 'route="/api/v1/blobs/{hash}/content"') || true
  awk '{ s += $NF } END { printf "%d", s + 0 }' <<<"$rows"
}

# peer_blob_reads prints the peer names a node SERVED a blob to on its peer
# surface, one line per read.
#
# # Why this is not the client API's counter
#
# The first version of the "controller carried no bytes" assertion counted
# ctrl_blob_bytes across the transfer and required it not to move. That is
# wrong, and it is wrong in the most dangerous way available: it passed here,
# five runs in a row, and failed in CI.
#
# The two surfaces SHARE the blob handler, deliberately (ADR-0013) — one
# implementation of ranges, validators and flat memory use rather than two that
# drift silently. What they do not share is the credential. But the client API's
# metrics label a request by its chi route pattern, and
# `/api/v1/blobs/{hash}/content` is the pattern for reads that arrived on a
# bearer token AND the string a reader would assume covers the peer fabric too.
# So that counter cannot answer "which listener served these bytes", which is
# the only question the assertion was asking.
#
# What actually moved it in CI was PROBING. internal/media/probe fetches blob
# bytes from its own node over HTTP Range to hand them to ffprobe, and those
# reads land on the client API's blob route like any other. The jobs are
# enqueued by ingest and run whenever the worker reaches them, so the count
# drifts by an amount that tracks load and elapsed time. This machine has no
# ffprobe; the probe job carries a RequiredCapability, is never claimed, and the
# reads never happen. The assertion was measuring a mechanism that was absent
# locally — the same shape as three other things caught the same day.
#
# So the claim is made POSITIVELY instead, from the sending side, on the surface
# that actually carried the bytes: node A recording that it served this blob, by
# GET, to a peer that presented a pinned certificate. Probing cannot reach that
# record, because probing has no certificate and never touches this listener.
peer_blob_reads() { # logfile method blob-hash
  grep -F '"msg":"served blob content to a peer"' "$1" 2>/dev/null |
    jq -r --arg m "$2" --arg h "$3" 'select(.method == $m and .blob_hash == $h) | .peer_name' || true
}

# peer_blob_read_count counts those reads.
#
# `grep -c .` rather than `wc -l`, because a here-string of "" is one empty line
# to wc and would report a surface that served nothing as having served once.
peer_blob_read_count() { # logfile method blob-hash
  peer_blob_reads "$1" "$2" "$3" | grep -c . || true
}

# peer_served_bytes is how many CONTENT bytes this node has sent for a blob,
# summed over every GET its peer surface answered.
#
# THE SOURCE'S SIDE, and that is the whole point. A destination's account of
# what it fetched is a claim about itself: a transfer that fetched nothing and
# published the wrong file would report a very good number. What left the
# source is a fact about the source, and since #218 the source records it —
# `bytes` on "served blob content to a peer", counted by the response recorder
# the client API's access log already uses.
#
# GET only. A HEAD is the durability precondition asking whether a blob is here
# and carries no body, and counting it would put a zero in every sum.
#
# Used as a DELTA around an operation rather than as an absolute. This arc
# moves the same blob more than once, so a total answers "how much has ever
# left" when the question is "how much did THAT cost".
peer_served_bytes() { # logfile blob-hash
  # The grep is braced with `|| true` rather than trailing the pipeline with
  # `|| echo 0`. Under `pipefail` a grep that matches nothing fails the whole
  # pipeline AFTER jq has already printed its 0, so the fallback would APPEND a
  # second value and every arithmetic use of this would then be a syntax error
  # on a two-line number.
  { grep -F '"msg":"served blob content to a peer"' "$1" 2>/dev/null || true; } |
    jq -s --arg h "$2" '[.[] | select(.method == "GET" and .blob_hash == $h) | .bytes] | add // 0'
}

# pct is a percentage as an integer, for a message a person reads.
#
# Integer arithmetic deliberately: bash has no floats, and every threshold this
# file asserts is a coarse one — "under a tenth of the blob" rather than
# "1.07%". A fraction that needs a decimal point to be convincing is a fraction
# that is too close to its threshold to be asserted at all.
pct() { # part whole
  if (( $2 == 0 )); then echo 0; else echo $(( $1 * 100 / $2 )); fi
}

