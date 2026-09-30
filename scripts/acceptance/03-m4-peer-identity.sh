# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# ---------------------------------------------------------------------------
# The peer identity, across the same two starts (M4-03, ADR-0010, ADR-0012)
# ---------------------------------------------------------------------------
#
# Anchored here deliberately: $WORK/all.log and $WORK/restart.log are two real
# starts against one data directory that this script has ALREADY paid for, so
# "byte-identical across restarts" costs no extra process. Asserting it
# anywhere else would mean starting the binary twice more to prove something
# these two runs already demonstrate.
#
# ADR-0012 said Milestone 1's only cost was "a peers.public_key column and
# generating the local keypair". The column existed from 00002_core.sql and
# NOTHING wrote it until 00019. These assertions are what keep that true.

PK1=$(grep -aom1 '"peer_public_key":"[^"]*"' "$WORK/all.log" | cut -d'"' -f4)
PK2=$(grep -aom1 '"peer_public_key":"[^"]*"' "$WORK/restart.log" | cut -d'"' -f4)
PEER1=$(grep -aom1 '"peer_id":"[^"]*"' "$WORK/all.log" | cut -d'"' -f4)

# Byte-identical, not merely "it started again". A node that regenerates its
# keypair keeps its peer id and changes its identity, which every peer that
# pinned the old key rejects — silently, at replication time, hours later.
assert_eq "$PK2" "$PK1" "the peer public key is byte-identical across a restart"

# The shape an operator copies to the other site: algorithm-prefixed hex, the
# same idiom as a blob digest so the two are not confused in a terminal.
if [[ "$PK1" =~ ^ed25519:[0-9a-f]{64}$ ]]; then
  pass "the public key is published as ed25519:<64 hex>"
else
  fail "the public key is not ed25519:<64 hex> — got '$PK1'"
fi

# Both places agree. This is the pair the refusal below compares.
MARKER_PEER=$(jq -r '.peer_id // ""' "$WORK/data/cas/HEYARR_CAS")
assert_eq "$MARKER_PEER" "$PEER1" "the CAS root marker names the same peer as the database"

# The private key: 0600, in the data directory, and NOT in the CAS root — the
# CAS is the thing that gets rebuilt, restored and moved between filesystems.
KEYFILE="$WORK/data/peer_ed25519.key"
if [[ -f "$KEYFILE" ]]; then pass "the private key is in the data directory"; else fail "no private key at $KEYFILE"; fi
# %Lp on BSD/macOS stat, %a on GNU stat. Neither is portable, both are here.
# GNU stat first, BSD second, and the order is load-bearing: on Linux `stat -f`
# is a VALID flag meaning "filesystem status", so it SUCCEEDS and the `||` never
# fires — capturing a block of filesystem info instead of a mode. BSD stat has
# no `-c`, so it fails cleanly and falls through. Tried the other way round and
# CI caught it.
KEYMODE=$(stat -c '%a' "$KEYFILE" 2>/dev/null || stat -f '%Lp' "$KEYFILE")
assert_eq "$KEYMODE" "600" "the private key is 0600"
if [[ -e "$WORK/data/cas/peer_ed25519.key" ]]; then
  fail "the private key is inside the CAS root, which travels between hosts"
else
  pass "the private key is not in the CAS root"
fi

# And it never appears in output. Scanned, not reasoned about: the assertion is
# over the captured bytes, so a future change that logs the key fails here even
# if nobody reads this comment again.
SEED=$(cut -d: -f2 <"$KEYFILE" | tr -d '[:space:]')
if [[ ${#SEED} -ne 64 ]]; then
  fail "could not read the private key material to scan for it (got ${#SEED} chars)"
else
  assert_eq "$(grep -c -- "$SEED" "$WORK/all.log" || true)" "0" \
    "the private key never appears in a log line"
  # -a because the database is binary and -c must still report a count; NOT
  # -r, which prefixes the filename and turns "0" into "path:0".
  assert_eq "$(grep -ac -- "$SEED" "$WORK/data/heyarr.db" 2>/dev/null || true)" "0" \
    "the private key never reaches the database"
fi

note "peer identity: the refusal when the two places disagree (M4-03, ADR-0010)"
# "The local peer identity is persisted in two places — the database and the CAS
# root marker. If they ever disagree, the process refuses to start: that
# disagreement is exactly how a deployment silently ends up with two peers
# claiming one identity, and it is unrecoverable once replication has run."
#
# That sentence was in ADR-0010 for three milestones with nothing behind it.
# This is what stops it going back to being decorative.
#
# Everything this section changes, it restores — and the restoration is
# asserted — so the sections after it see the data directory they would have
# seen anyway. It creates no library, no Work, no asset and no blob.

MARKER="$WORK/data/cas/HEYARR_CAS"
OTHER_PEER="01990000-0000-7000-8000-0000000000ff"
cp "$MARKER" "$WORK/marker.orig"
jq --arg p "$OTHER_PEER" '.peer_id = $p' "$WORK/marker.orig" > "$MARKER"

# Assert the CORRUPTION APPLIED before believing anything that follows. A
# residue check that never changed the file passes for the wrong reason, and it
# passes forever.
assert_eq "$(jq -r '.peer_id' "$MARKER")" "$OTHER_PEER" \
  "the CAS marker was actually rewritten to name another peer"

assert_refuses "a CAS marker naming another peer refuses to start" \
  "$OTHER_PEER" "$BIN" --config "$WORK/heyarr.yaml" controller
# The error must name BOTH identities: an operator told only that something
# disagrees cannot tell which of the two machines to correct.
assert_contains "$REPLY" "$PEER1"  "the refusal names the database's peer"
assert_contains "$REPLY" "$MARKER" "the refusal says where the second identity is written"
# ...and it must be a refusal, not a warning it carried on past.
assert_not_contains "$REPLY" "controller started" \
  "the refusal happens before the controller reports itself started"

# Restore, and assert the RESTORATION APPLIED too — otherwise the "it starts
# again" check below could be passing against a still-corrupt marker for some
# unrelated reason.
cp "$WORK/marker.orig" "$MARKER"
assert_eq "$(jq -r '.peer_id' "$MARKER")" "$PEER1" "the CAS marker was restored"

run_and_term identity-restored 5 all >/dev/null
PK3=$(grep -aom1 '"peer_public_key":"[^"]*"' "$WORK/identity-restored.log" | cut -d'"' -f4)
assert_eq "$PK3" "$PK1" "restoring the marker lets the node start again, as the same peer"

# The same refusal for the other half of the identity: a private key that does
# not belong to the recorded public key. Same node, same peer id, and it cannot
# prove it is the peer its own catalog says it is.
#
# printf '%064d' 1 is 64 hex-safe digits, so it parses as a seed and is
# certainly not the real one — the only thing wrong with it is which keypair it
# is, which is exactly the condition under test.
cp "$KEYFILE" "$WORK/peerkey.orig"
printf 'ed25519-seed:%s\n' "$(printf '%064d' 1)" > "$KEYFILE"
chmod 600 "$KEYFILE"
assert_eq "$(cut -d: -f2 <"$KEYFILE" | tr -d '[:space:]')" "$(printf '%064d' 1)" \
  "the private key was actually replaced"

assert_refuses "a private key that does not match the recorded public key refuses to start" \
  "$PK1" "$BIN" --config "$WORK/heyarr.yaml" controller

cp "$WORK/peerkey.orig" "$KEYFILE"
assert_eq "$(cut -d: -f2 <"$KEYFILE" | tr -d '[:space:]')" "$SEED" "the private key was restored"
run_and_term identity-key-restored 5 all >/dev/null
assert_eq "$(grep -aom1 '"peer_public_key":"[^"]*"' "$WORK/identity-key-restored.log" | cut -d'"' -f4)" \
  "$PK1" "restoring the private key lets the node start again, as the same peer"

