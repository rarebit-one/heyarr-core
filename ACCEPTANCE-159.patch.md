# `scripts/acceptance.sh` — the section for #159

`scripts/acceptance.sh` is owned by another branch right now, so this section is
delivered as a patch to apply rather than as an edit. Nothing else in this PR
touches that file.

## Where to anchor it

Immediately **after** the `configuration layering` section — that is, after the
last `assert_refuses` of the refusal group (the one whose needle is
`"$WORK/nope.yaml"`, `assert_refuses "names the config path it could not read"`),
and **before** the next `note` that starts a server.

It is anchored there because it needs no running controller, no CAS and no
fixture library: it starts nothing, so it costs the demo about a second and
fails fast if the device surface is broken.

## Requirements it relies on

- `assert_eq`, which is currently defined at line ~637 — **below** this anchor.
  Either move the `assert_eq` definition up beside `assert_contains` (preferred:
  it is a helper, and helpers defined halfway down a script are a trap), or
  anchor this section after that definition instead. `assert_eq` is used rather
  than `assert_contains` for every enum-like value below, because
  `assert_contains "$OUT" "not_enrolled"` would also pass on
  `"enrolment_status": "not_enrolled_yet_but_actually_fine"`.
- `jq`, already required by the rest of the script.

## The section

```bash
# ---------------------------------------------------------------------------
# Device keys and the Personal MCP (§40, §73, ADR-0032, #159)
# ---------------------------------------------------------------------------
#
# The device key is a CLIENT concern: it belongs to the person at the keyboard,
# lives in their config directory, and authorises nothing yet. Every assertion
# below is about one of those three facts.
note "device keys (§40, ADR-0032)"

DEVDIR="$WORK/config/heyarr/device"
DEVLOG="$WORK/device-output.txt"
: > "$DEVLOG"

# A server data directory that actually has something in it, so "untouched"
# below is a claim about files rather than about an empty directory.
"$BIN" token create -c "$WORK/heyarr.yaml" device-acceptance --json >/dev/null
DATA_BEFORE=$(cd "$WORK/data" && find . -type f | sort | xargs -I{} sh -c 'printf "%s %s\n" "{}" "$(wc -c < "{}")"' | shasum | cut -d' ' -f1)

# HEYARR_DATA_DIR is exported deliberately: an implementation that consulted the
# server's configuration would be caught here rather than merely be unlikely.
DEV_JSON=$(HEYARR_DATA_DIR="$WORK/data" "$BIN" device generate --device-dir "$DEVDIR" --name acceptance --json)
printf '%s\n' "$DEV_JSON" >> "$DEVLOG"

DEV_ID=$(printf '%s' "$DEV_JSON" | jq -r '.id')
DEV_PUB=$(printf '%s' "$DEV_JSON" | jq -r '.public_key')

# The honesty labelling, as FIELDS. This is the deliverable, not the prose
# around it: a key that is not yet load-bearing and does not say so is worse
# than no key at all.
assert_eq "$(printf '%s' "$DEV_JSON" | jq -r '.enrolment_status')" "not_enrolled" \
  "the device record reports enrolment_status not_enrolled"
assert_eq "$(printf '%s' "$DEV_JSON" | jq -r '.unproven')" "true" \
  "the device record reports unproven"
assert_eq "$(printf '%s' "$DEV_JSON" | jq -r '.algorithm')" "ed25519" \
  "the device key is ed25519 (§40, ADR-0012)"
assert_eq "$(printf '%s' "$DEV_PUB" | grep -cE '^ed25519:[0-9a-f]{64}$')" "1" \
  "the public key is rendered ed25519:<64 lowercase hex>, as identity.FormatPublicKey does"

# The key is where it belongs, and owner-only. This is asserted BEFORE the
# data-directory assertion below: the other order passes trivially on a command
# that wrote nothing at all.
DEV_KEY="$DEVDIR/device_ed25519.key"
if [[ -f "$DEV_KEY" ]]; then
  pass "the private key is in the user's config directory"
else
  fail "the private key is not at $DEV_KEY — every assertion after this would pass vacuously"
fi
# stat's portable-ish spelling: BSD first, then GNU.
DEV_MODE=$(stat -f '%Lp' "$DEV_KEY" 2>/dev/null || stat -c '%a' "$DEV_KEY")
assert_eq "$DEV_MODE" "600" "the private key is mode 0600"

# And the server's data directory is byte-for-byte what it was.
DATA_AFTER=$(cd "$WORK/data" && find . -type f | sort | xargs -I{} sh -c 'printf "%s %s\n" "{}" "$(wc -c < "{}")"' | shasum | cut -d' ' -f1)
assert_eq "$DATA_AFTER" "$DATA_BEFORE" \
  "generating a device key left the server's data_dir untouched (§38, ADR-0032)"
assert_eq "$(find "$WORK/data" -name 'device_ed25519.key' | wc -l | tr -d ' ')" "0" \
  "no device key was written into the server's data_dir"

# A round trip, asserting the public key rather than the exit status.
LIST_ONE=$("$BIN" device list --device-dir "$DEVDIR" --json)
LIST_TWO=$("$BIN" device list --device-dir "$DEVDIR" --json)
printf '%s\n%s\n' "$LIST_ONE" "$LIST_TWO" >> "$DEVLOG"
assert_eq "$(printf '%s' "$LIST_ONE" | jq -r '.[0].public_key')" "$DEV_PUB" \
  "device list reports the key device generate created"
assert_eq "$LIST_TWO" "$LIST_ONE" \
  "a second device list is byte-identical to the first"
assert_eq "$(printf '%s' "$LIST_ONE" | jq -r 'length')" "1" \
  "one machine holds one device key"

SHOW_JSON=$("$BIN" device show "$DEV_ID" --device-dir "$DEVDIR" --json)
printf '%s\n' "$SHOW_JSON" >> "$DEVLOG"
assert_eq "$(printf '%s' "$SHOW_JSON" | jq -r '.public_key')" "$DEV_PUB" \
  "device show reports the same key"

# The human output carries the caveat too, because the CLI is where somebody
# actually reads it.
DEV_HUMAN=$("$BIN" device list --device-dir "$DEVDIR")
printf '%s\n' "$DEV_HUMAN" >> "$DEVLOG"
assert_contains "$DEV_HUMAN" "unproven" "device list says the key is unproven"
assert_contains "$DEV_HUMAN" "not_enrolled" "device list says the key is not enrolled"
assert_contains "$DEV_HUMAN" "bearer scope" \
  "device list says grants remain ADR-0011 bearer scopes until Milestone 8"

# ---------------------------------------------------------------------------
# The Personal MCP is LOCAL, over stdio, and is not the controller's MCP
# ---------------------------------------------------------------------------
note "the Personal MCP (§72, §73, ADR-0032)"

MCP_TOOLS=$(printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  | "$BIN" device mcp --device-dir "$DEVDIR" 2>/dev/null \
  | jq -r '.result.tools[].name' | paste -sd, -)
assert_eq "$MCP_TOOLS" "device_generate,device_list,device_remove,device_show" \
  "the Personal MCP publishes exactly its four key-management tools"

MCP_LIST=$(printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"device_list","arguments":{}}}' \
  | "$BIN" device mcp --device-dir "$DEVDIR" 2>/dev/null)
printf '%s\n' "$MCP_LIST" >> "$DEVLOG"
assert_eq "$(printf '%s' "$MCP_LIST" | jq -r '.result.structuredContent.devices[0].enrolment_status')" \
  "not_enrolled" "the MCP response carries enrolment_status not_enrolled"
assert_eq "$(printf '%s' "$MCP_LIST" | jq -r '.result.structuredContent.devices[0].unproven')" \
  "true" "the MCP response carries unproven"
assert_eq "$(printf '%s' "$MCP_LIST" | jq -r '.result.structuredContent.devices[0].public_key')" \
  "$DEV_PUB" "the MCP response reports the same public key the CLI does"

MCP_INIT=$(printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize"}' \
  | "$BIN" device mcp --device-dir "$DEVDIR" 2>/dev/null)
assert_eq "$(printf '%s' "$MCP_INIT" | jq -r '.result.serverInfo.name')" "heyarr-personal" \
  "the Personal MCP names itself distinctly from the controller's MCP"

# ---------------------------------------------------------------------------
# The private key never appears in ANY output
# ---------------------------------------------------------------------------
#
# Scanned, not reasoned about. $DEVLOG holds everything every command above
# printed, including the MCP transcripts.
DEV_SEED=$(sed 's/^heyarr-device-ed25519-seed://' "$DEV_KEY" | tr -d '\n')
if [[ -z "$DEV_SEED" ]]; then
  fail "the seed could not be read from $DEV_KEY — the scan below would prove nothing"
fi
assert_eq "$(grep -c -F "$DEV_SEED" "$DEVLOG" || true)" "0" \
  "the private key never appears in any command output or MCP response"
# The scan must have had something to find: on an empty log it would pass having
# proved nothing at all.
if grep -qF "$DEV_PUB" "$DEVLOG"; then
  pass "the captured output is non-empty — the public key is in it, the private key is not"
else
  fail "no public key in the captured output — the private-key scan above proves nothing"
fi

# ---------------------------------------------------------------------------
# Refusals — one case each
# ---------------------------------------------------------------------------
note "device key refusals"

assert_refuses "refuses to regenerate without --force" "already has a device key" \
  "$BIN" device generate --device-dir "$DEVDIR"

assert_refuses "refuses to remove a device it does not hold" "no such device" \
  "$BIN" device remove 01920000-0000-7000-8000-000000000000 --device-dir "$DEVDIR"

chmod 644 "$DEV_KEY"
assert_refuses "refuses a world-readable private key" "readable by more than its owner" \
  "$BIN" device list --device-dir "$DEVDIR"
chmod 600 "$DEV_KEY"

cp "$DEV_KEY" "$WORK/device-key.bak"
printf 'ssh-ed25519 AAAA\n' > "$DEV_KEY"
assert_refuses "refuses a key file that is not a key" "not a heyarr device key" \
  "$BIN" device list --device-dir "$DEVDIR"
cp "$WORK/device-key.bak" "$DEV_KEY"

# It still works after all that, or one of the refusals above left the store
# broken and every later run would start from a corrupt directory.
assert_eq "$("$BIN" device list --device-dir "$DEVDIR" --json | jq -r '.[0].public_key')" "$DEV_PUB" \
  "the device key survives the refusal cases intact"
```

## For the "what this run proves, and what it does not" block at the end

Add to the **NOT proven** list:

```
printf '         THE DEVICE KEY AUTHORISES NOTHING. It is generated, stored and\n'
printf '         labelled unproven/not_enrolled, and no request anywhere above was\n'
printf '         authorised by it — every credential in this run is an ADR-0011\n'
printf '         bearer token. Enrolment, delegation, wrapped space keys, pairing\n'
printf '         and recovery are Milestone 8 and 9 (§40, §41, ADR-0022, ADR-0032).\n'
```
