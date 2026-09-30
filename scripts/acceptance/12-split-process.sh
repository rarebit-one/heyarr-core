# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# ADR-0002: the roles must be independently runnable as OS processes, and the
# only way that stays true is running the real checks in both configurations.
# Otherwise one of the two is never exercised and the split is decorative.
split_process_demo() {
  local root="$WORK/split-full"
  local lib="$root/library" data="$root/data"
  SOCK="$data/heyarr.sock"
  MANIFEST="$root/manifest.json"
  mkdir -p "$root"
  "$GEN" -out "$lib" -manifest "$MANIFEST" -large-size 1048576 >/dev/null

  local files blobs
  files=$(jq -r '.ingestable_files' "$MANIFEST")
  blobs=$(jq -r '.ingestable_blobs' "$MANIFEST")

  cat > "$WORK/split-full.yaml" <<YAML
data_dir: $data
peer:
  name: acceptance-split
  site: test
log:
  level: info
  format: json
# The socket is the whole transport here. Binding a fixed TCP port would make
# two runs on one machine collide, and a leaked server from an interrupted run
# would break every later run with a bind error that says nothing about why —
# which is exactly what happened while this section was being written.
http:
  addr: ""
libraries:
  - name: films
    content_type: movie
    roots: ["$lib/movies"]
  - name: shows
    content_type: series
    roots: ["$lib/tv"]
  - name: albums
    content_type: music
    roots: ["$lib/music"]
  - name: shelf
    content_type: book
    roots: ["$lib/books"]
YAML

  TOKEN=$("$BIN" --config "$WORK/split-full.yaml" token create acceptance --scopes admin --json | jq -r .token)

  FULL_LOG="$WORK/split-full.log"
  FULL_PIDS=()
  local role
  for role in controller worker peer; do
    "$BIN" --config "$WORK/split-full.yaml" "$role" >>"$FULL_LOG" 2>&1 &
    FULL_PIDS+=($!)
  done

  local waited=0
  while (( waited < 600 )); do
    curl -sf --unix-socket "$SOCK" http://heyarr/readyz >/dev/null 2>&1 && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  if (( waited >= 600 )); then
    fail "split-process: /readyz never became ready"; tail -30 "$FULL_LOG"; stop_full; return 1
  fi
  pass "three separate processes serve a ready API"

  wait_for_ingest split-full "$files" || { stop_full; return 1; }

  local a b
  a=$(api_all /api/v1/assets '.items[].id' | sort -u | wc -l | tr -d ' ')
  b=$(find "$data/cas/blobs" -type f 2>/dev/null | wc -l | tr -d ' ')
  assert_eq "$a" "$files" "split-process mode ingests the same assets"
  assert_eq "$b" "$blobs" "split-process mode deduplicates the same blobs"

  local big_hash
  big_hash=$(jq -r --arg p "$(jq -r .largest_path "$MANIFEST")" '.files[] | select(.path == $p) | .hash' "$MANIFEST")
  assert_eq "$(api "/api/v1/blobs/$big_hash/content" -H 'Range: bytes=0-1023' -o /dev/null -w '%{http_code}')" \
    "206" "split-process mode range-serves bytes"
  stop_full
}

