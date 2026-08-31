# qBittorrent daemon-in-the-loop harness

The honest proof behind the `acquires-over-daemon-clients` claim for qBittorrent
— *"a real qBittorrent transfer completed"* — that the merge-path demo cannot
make because a download daemon is out of the demo budget by design (ADR-0026,
amended by [ADR-0052](../../../docs/adr/0052-a-disposable-download-daemon-earns-a-scheduled-lane.md)).

## What it stands up

Two disposable services on one private network (`docker-compose.yml`):

- **qbittorrent** — a real qBittorrent-nox Web API, the daemon under test. Its
  WebUI is published to `127.0.0.1:8080` so the Go harness test drives it over
  the host, and its `/downloads` save directory is bind-mounted to a host path
  so the completed bytes are readable by the test that offered them. Auth is
  bypassed for the private subnet (`config/qBittorrent/qBittorrent.conf`), so
  the client authenticates with empty credentials.
- **webseed** — a plain nginx serving a directory the test writes into. The
  generated `.torrent` carries a BEP-19 web seed (`url-list`) pointing here, so
  the transfer completes from one HTTP source with no tracker, no peer and no
  DHT. Deterministic and offline.

## What it exercises

`TestHarnessQBittorrentTransfer` (in `internal/downloads`) drives the whole arc
the fake stands in for: **connect + authenticate** against the real daemon,
generate a real single-file web-seed `.torrent`, **add** it through the ordinary
client `Add` path, **watch it complete** through real qBittorrent state, and
assert the completed bytes are **byte-identical** to what the web seed offered —
resolving the path through the client's own path map.

## Running it

```bash
make daemon-acceptance      # or: ./scripts/daemon-acceptance.sh
```

Needs Docker (with the compose plugin) and the Go toolchain. It stands the stack
up, waits for the Web API, runs the test, and tears everything down (`down -v`).

It is **not on the merge path**. In CI it runs from the scheduled
`daemon-acceptance` workflow (nightly + `workflow_dispatch`), never on a pull
request — so a container pull never gates a merge or hangs a PR (the failure
mode ADR-0026 rejected a container lane to avoid). On the ordinary merge path
`go test ./...` finds the harness env unset and skips the test; the generator it
relies on is still proven there by `TestMakeWebseedTorrent`.

## Reproducibility: pin the images to digests

`nginx` is pinned to a version tag; the qBittorrent image currently tracks
`latest`. Once the scheduled lane has run green, read the resolved digests from
its log (`docker compose images` / the pull lines) and replace the tags with
`image: …@sha256:…` in `docker-compose.yml`. Until then a moved upstream tag
surfaces as a scheduled-run diff, never as a red PR.

## Not yet here: SABnzbd and NZBGet

§58 lists them, but no `providers.Downloader` implements them yet. Standing up
those daemons now would be a daemon with no caller — the defect this project
guards against. When a SABnzbd or NZBGet client lands it gets a compose service
and a harness test of the same shape, and its transfer leg flips from
documented-pending to proven the way qBittorrent's did. See ADR-0052 and #379.
