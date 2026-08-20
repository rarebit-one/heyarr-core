#!/usr/bin/env bash
# Regenerate the committed media fixtures (M2-02).
#
# Milestone 1's fixtures were structurally valid containers that no decoder had
# ever opened — deliberately, because building a "decodable" MP4 by hand on a
# machine with no ffprobe anywhere to check it against would have bought false
# confidence and presented the bill in Milestone 2, looking like a probing bug.
# Milestone 2 has a decoder. This is that debt being paid.
#
# The output is COMMITTED, so an ordinary `go test ./...` needs no external
# binary and the pure-Go suite stays hermetic. This script exists for when a
# fixture genuinely has to change, and for the determinism check.
#
# # Everything here is synthetic
#
# testsrc2 and sine, so the bytes are ours and the licensing of a public AGPL
# repository stays simple. No third-party sample files, ever.
#
# # Reproducibility
#
# -fflags +bitexact -flags +bitexact drops the encoder string and the creation
# timestamp, which is what makes two runs on one machine byte-identical.
# TestRegeneratingTheFixturesIsDeterministic asserts that.
#
# It does NOT make two runs on DIFFERENT ffmpeg versions identical, and cannot:
# the pinned toolchain is 7.0.2-static on linux and 6.0 on darwin (ADR-0023,
# scripts/toolchain.lock). So the committed bytes are whatever the machine that
# last regenerated them produced, and what every platform asserts is that they
# PROBE to the right thing — not that they would be re-encoded identically.
set -euo pipefail
cd "$(dirname "$0")/.."

FF=${FFMPEG:-$PWD/.toolchain/bin/ffmpeg}
if [ ! -x "$FF" ]; then
  echo "genmedia: no ffmpeg at $FF — run scripts/toolchain.sh" >&2
  exit 1
fi
# GENMEDIA_OUT lets the determinism test generate into a t.TempDir rather than
# over the committed files, so running the check cannot itself change the thing
# it is checking.
OUT=${GENMEDIA_OUT:-internal/testutil/fixtures/media}
mkdir -p "$OUT"

# Position matters and is easy to get wrong. -fflags before -i configures the
# INPUT demuxer; the muxer that writes the random Matroska SegmentUID needs it
# as an OUTPUT option, after the inputs. Getting that wrong produced .mkv files
# whose bytes changed on every run while the .mp4 files were stable — caught by
# the determinism check, not by reading the manual.
BITEXACT_IN="-fflags +bitexact"
BITEXACT_OUT="-fflags +bitexact -flags +bitexact"
Q="-hide_banner -loglevel error -y -nostdin"

# Two DISTINCT H.264/AAC MP4s. Distinct by content rather than by appended
# junk: the ingest fixtures need two files that must not deduplicate, and
# padding a real container to make it different is how a fixture stops being a
# representative of the thing it stands for.
#
# 160x120 at 10fps for one second. These are hashed, ranged over and copied by
# a large fraction of the test suite; they are not a place to put megabytes.
for i in 1 2; do
  # shellcheck disable=SC2086 # the flag groups are intentionally word-split
  "$FF" $Q $BITEXACT_IN \
    -f lavfi -i "testsrc2=size=160x120:rate=10:duration=1" \
    -f lavfi -i "sine=frequency=$((220 * i)):sample_rate=44100:duration=1" \
    -filter_complex "[0:v]hue=h=$((i * 90))[v]" -map "[v]" -map 1:a \
    -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
    -c:a aac -b:a 32k -movflags +faststart $BITEXACT_OUT \
    "$OUT/h264_aac_$i.mp4"
done

# The same streams in Matroska. This is the REMUX case the planner exists to
# recognise: right codecs, wrong container for a device that only takes MP4.
for i in 1 2; do
  # shellcheck disable=SC2086
  "$FF" $Q $BITEXACT_IN -i "$OUT/h264_aac_$i.mp4" -c copy $BITEXACT_OUT "$OUT/h264_aac_$i.mkv"
done

# HEVC, for the TRANSCODE case: a codec an ordinary profile refuses.
# shellcheck disable=SC2086
"$FF" $Q $BITEXACT_IN \
  -f lavfi -i "testsrc2=size=160x120:rate=10:duration=1" \
  -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=1" \
  -c:v libx265 -preset ultrafast -pix_fmt yuv420p -x265-params log-level=none \
  -c:a aac -b:a 32k -tag:v hvc1 -movflags +faststart $BITEXACT_OUT \
  "$OUT/hevc_aac.mp4"

# Audio, for listening rather than watching.
# shellcheck disable=SC2086
"$FF" $Q $BITEXACT_IN -f lavfi -i "sine=frequency=440:sample_rate=44100:duration=2" \
  -c:a flac -sample_fmt s16 $BITEXACT_OUT "$OUT/flac.flac"
# shellcheck disable=SC2086
"$FF" $Q $BITEXACT_IN -f lavfi -i "sine=frequency=330:sample_rate=44100:duration=2" \
  -c:a libmp3lame -b:a 128k -write_xing 0 -id3v2_version 0 $BITEXACT_OUT "$OUT/mp3.mp3"

# Record what produced these bytes. The pinned toolchain is a different version
# on linux and darwin (ADR-0023), so "regenerating produces the committed
# bytes" is only a meaningful claim on the platform that last did it — and
# without this file there is no way to know which that was.
"$FF" -version | head -1 | awk '{print $3}' > "$OUT/GENERATED_BY"

ls -l "$OUT"
