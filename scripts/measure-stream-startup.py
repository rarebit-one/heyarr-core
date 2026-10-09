#!/usr/bin/env python3
"""Interleave old/new FFmpeg settings and verify the first complete fragment.

Run on the serving node with its production FFmpeg and a representative source.
Times stop at the first complete moof/mdat pair, not the response header. This
measures server output availability; decoder, network and TV buffer time are not
included. Each prefix is decoded and audio language is checked before reporting.
"""
import argparse
import json
import os
from pathlib import Path
import statistics
import struct
import subprocess
import tempfile
import threading
import time


def exact(stream, size):
    chunks = bytearray()
    while len(chunks) < size:
        part = stream.read(size - len(chunks))
        if not part:
            raise RuntimeError("FFmpeg ended before a complete playable fragment")
        chunks.extend(part)
    return bytes(chunks)


def measure(args, variant, directory):
    command = ["ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
               "-ss", str(args.start), "-i", args.source,
               "-map", "0:v:0?", "-map", f"0:a:{args.audio_track}?",
               "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
               "-vf", f"scale=-2:'min({args.height},ih)'", "-c:a", "aac", "-ac", "2", "-b:a", "192k"]
    if variant == "short_fragments":
        command += ["-force_key_frames", "expr:gte(t,n_forced*1)"]
    command += ["-movflags", "frag_keyframe+empty_moov+default_base_moof+delay_moov"]
    if variant == "short_fragments":
        command += ["-frag_duration", "1000000"]
    command += ["-f", "mp4", "-"]
    error_path = directory / "ffmpeg.stderr"
    prefix = bytearray()
    start = time.monotonic()
    with error_path.open("wb") as stderr:
        process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=stderr)
        deadline = threading.Timer(60, process.kill)
        deadline.daemon = True
        deadline.start()
        try:
            fragment = False
            while True:
                header = exact(process.stdout, 8)
                size, kind = struct.unpack(">I4s", header)
                if size == 1:
                    extension = exact(process.stdout, 8)
                    header += extension
                    size = struct.unpack(">Q", extension)[0]
                if size < len(header) or size > 64 << 20:
                    raise RuntimeError(f"invalid or oversized MP4 box: {size}")
                prefix.extend(header)
                prefix.extend(exact(process.stdout, size - len(header)))
                fragment |= kind == b"moof"
                if fragment and kind == b"mdat":
                    elapsed = time.monotonic() - start
                    break
        finally:
            deadline.cancel()
            process.kill()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            process.stdout.close()
    if error_path.read_text().strip():
        raise RuntimeError(error_path.read_text())
    sample = directory / "first-fragment.mp4"
    sample.write_bytes(prefix)
    probe = json.loads(subprocess.check_output(["ffprobe", "-v", "error", "-show_streams", "-show_packets", "-of", "json", str(sample)]))
    audio = [s for s in probe["streams"] if s["codec_type"] == "audio"]
    video = [s for s in probe["streams"] if s["codec_type"] == "video"]
    assert len(audio) == 1 and audio[0]["codec_name"] == "aac"
    assert audio[0].get("tags", {}).get("language") in ("eng", "en")
    assert len(video) == 1 and video[0]["codec_name"] == "h264" and video[0]["height"] <= args.height
    subprocess.run(["ffmpeg", "-v", "error", "-i", str(sample), "-frames:v", "1", "-f", "null", "-"], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    pts = [float(p["pts_time"]) for p in probe["packets"] if p["codec_type"] == "video"]
    return {"variant": variant, "first_playable_fragment_seconds": elapsed,
            "fragment_bytes": len(prefix), "video_packets": len(pts), "video_pts_span_seconds": max(pts)-min(pts),
            "audio_language": audio[0]["tags"]["language"], "decoded_first_frame": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source")
    parser.add_argument("--start", type=float, default=600)
    parser.add_argument("--height", type=int, default=1080)
    parser.add_argument("--audio-track", type=int, default=1)
    parser.add_argument("--samples", type=int, default=5)
    args = parser.parse_args()
    if args.samples < 5:
        parser.error("at least five interleaved samples are required")
    load_before = os.getloadavg()
    source_stat = Path(args.source).stat()
    with tempfile.TemporaryDirectory(prefix="stream-startup-") as temporary:
        directory = Path(temporary)
        # Warm both paths once; keep their results out of the reported sample.
        for variant in ("baseline", "short_fragments"):
            measure(args, variant, directory)
        rows = []
        for _ in range(args.samples):
            for variant in ("baseline", "short_fragments"):
                rows.append(measure(args, variant, directory))
    after = Path(args.source).stat()
    assert (after.st_size, after.st_mtime_ns) == (source_stat.st_size, source_stat.st_mtime_ns)
    summaries = {}
    for variant in ("baseline", "short_fragments"):
        times = [r["first_playable_fragment_seconds"] for r in rows if r["variant"] == variant]
        summaries[variant] = {"median_seconds": statistics.median(times), "min_seconds": min(times), "max_seconds": max(times), "samples": len(times)}
    print(json.dumps({"measurement": "complete first playable fragment at FFmpeg stdout, before network/player buffering",
                      "ffmpeg_version": subprocess.check_output(["ffmpeg", "-version"], text=True).splitlines()[0],
                      "cpu_count": os.cpu_count(), "load_before": load_before, "load_after": os.getloadavg(),
                      "start_seconds": args.start, "height": args.height, "audio_track": args.audio_track,
                      "errors": 0, "source_unchanged": True, "summary": summaries, "runs": rows}, indent=2))


if __name__ == "__main__":
    main()
