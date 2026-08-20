# The media toolchain

FFmpeg and ffprobe are the only binaries Heyarr needs and does not contain.
[ADR-0023](../adr/0023-the-external-media-toolchain-is-optional.md) records why
they are optional; this page is how to install them.

## Anywhere

```sh
./scripts/toolchain.sh          # installs into ./.toolchain/bin, prints the path
export PATH="$PWD/.toolchain/bin:$PATH"
```

The version and digest come from `scripts/toolchain.lock`. A checksum mismatch
is a hard failure: it means either a corrupt download or a replaced release
asset, and those need different responses from a human, so the script refuses
to guess.

## `hyperion-1`

The deployment host has no Go toolchain and no CI, which is why the installer
is a shell script with no build step rather than a Go program. Run it in the
checkout, or copy `.toolchain/bin/{ffmpeg,ffprobe}` alongside the `heyarr`
binary and point at them explicitly:

```yaml
media:
  ffprobe_path: /opt/heyarr/bin/ffprobe
  ffmpeg_path:  /opt/heyarr/bin/ffmpeg
```

A configured path that does not work is a **startup failure**, not a silent
fall back to `PATH`. That is deliberate: naming a binary and quietly getting a
different one is worse than not starting.

## Not installing it at all

Also supported, and tested on every build. Leave both paths empty and put
nothing on `PATH`. Heyarr starts, scans, ingests, catalogues, verifies,
garbage-collects and serves byte ranges; probe and remux jobs stay pending
until a worker that has the toolchain joins.

Check what a node resolved:

```sh
curl -s localhost:7777/api/v1/system -H "Authorization: Bearer $TOKEN" | jq .media
```

Note that this describes **the node answering the request**. In a split-process
deployment the worker that would claim a probe job is a different machine, and
`/api/v1/jobs` showing pending probes is the other half of the picture.
