# shellcheck shell=bash
# A section of scripts/acceptance.sh: sourced by it, in order, never run alone.
# subtitle_extraction_demo proves ADR-0084 end to end: a video whose captions
# live INSIDE the container arrives with no subtitle asset, and ingest lifts each
# embedded TEXT track out into its own role='subtitle' asset — byte-identical in
# shape to a shipped .srt sidecar, so the players and the DLNA CaptionInfo.sec
# header (ADR-0040) serve it with no consumer change. This is the demo scene #490
# deferred as a follow-up.
#
# Self-contained — its own node, library and data_dir — so it adds nothing to the
# catalogue the order-sensitive counts above are measured against. Asserted from
# the node's own log because that is where the extraction records its result: the
# "extracted an embedded subtitle" line is written only AFTER the asset has been
# recorded on the video's edition (extractsubs.go), so the line IS that having
# happened, not a claim about it.
subtitle_extraction_demo() {
  if ! { command -v ffmpeg >/dev/null 2>&1 && command -v ffprobe >/dev/null 2>&1; }; then
    pass "no ffmpeg/ffprobe here, so there is no embedded subtitle to lift"
    not_exercised ffmpeg "an embedded TEXT subtitle track lifted out of a video into its own role='subtitle' asset at ingest (ADR-0084)"
    return 0
  fi

  local root lib srt mkv log pid waited=0
  root="$WORK/subs"; lib="$root/library/movies"
  mkdir -p "$lib/Embedded Tale (2024)"

  # A real, valid MKV carrying one real SubRip track, built the way the ffmpeg
  # integration test builds one: a tiny synthetic video plus a muxed .srt. Unlike
  # the random-byte fixtures elsewhere, this is a container ffprobe can actually
  # read, which is the whole point — there is a track in here to find.
  srt="$root/in.srt"
  printf '1\n00:00:00,000 --> 00:00:02,000\nInside the container.\n\n' > "$srt"
  mkv="$lib/Embedded Tale (2024)/Embedded.Tale.2024.1080p.mkv"
  ffmpeg -v error -y -f lavfi -i "testsrc=duration=1:size=320x240:rate=5" -i "$srt" \
    -map 0:v -map 1 -c:v mpeg4 -c:s srt -metadata:s:s:0 language=eng "$mkv" </dev/null

  cat > "$WORK/subs.yaml" <<YAML
data_dir: $root/data
peer:
  name: subs-node
log:
  level: info
  format: json
http:
  addr: ""
libraries:
  - name: acceptance-subs
    content_type: movie
    roots:
      - $root/library
YAML

  log="$WORK/subs-node.log"
  "$BIN" --config "$WORK/subs.yaml" all >"$log" 2>&1 &
  pid=$!

  # Wait for the extraction to have RUN, not merely for ingest: the
  # extract_subtitles job is enqueued AT ingest and runs after it, and this
  # summary line is the handler's last word on a video.
  while (( waited < 900 )); do
    grep -q '"msg":"extracted embedded subtitles"' "$log" 2>/dev/null && break
    sleep 0.1; waited=$(( waited + 1 ))
  done
  stop_peer_node "$pid"

  local sublog; sublog=$(cat "$log")
  if (( waited >= 900 )); then
    fail "the embedded subtitle was never extracted within the budget"; echo "$sublog"; return 1
  fi

  assert_contains "$sublog" '"msg":"extracted an embedded subtitle"' \
    "an embedded TEXT track was lifted out of the container into its own asset"
  assert_contains "$sublog" '"language":"eng"' \
    "the extracted subtitle kept the track's language, the label a caption consumer reads"
  assert_contains "$sublog" '"recorded":1' \
    "exactly one subtitle asset was recorded for the one text track the video carried"
}

