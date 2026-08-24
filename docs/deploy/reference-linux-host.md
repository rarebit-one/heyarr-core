# Deploying to the reference Linux host

The reference deployment — one ordinary x86_64 Linux box with a large ext4
media filesystem, described by its shape rather than its name because this repo
is public. Everything here was measured on the machine rather than assumed, and
where the measurement contradicted the plan, the measurement wins and the plan
is marked as such.

## What the machine actually is

| | |
|---|---|
| OS | Ubuntu 24.04.4 LTS, kernel 6.8 |
| Architecture | x86_64 |
| CPU / RAM | 16 cores / 31 GB |
| Root filesystem | ext4 on LVM, 913 GB |
| Media filesystem | **ext4**, 9.1 TB at `/srv/media` |
| systemd | 255 |

**There is no ZFS on this machine.** No pools, no datasets, no `zpool` binary.
That matters more than it sounds, because it invalidates the layout this
document was originally scoped to describe.

## The reflink question, measured

ADR-0014 materialises an ingested file by trying copy-on-write reflink, then
hardlink, then a byte copy. Its premise is that on a filesystem with block
cloning, adopting a 60 GB remux costs metadata — which is the difference between
Heyarr being adoptable against an existing library and demanding you double your
storage first.

Measured on `/srv/media` using **free space, not `du`**. `du` reports the full
logical size for a clone on every filesystem that has them, so it would report a
100% failure for an operation that consumed nothing. The instrument was
validated with a control copy first:

| operation | free space consumed |
|---|---|
| control: ordinary `cp` of a 256 MiB file | **262 144 KiB** — the instrument works |
| `cp --reflink=always` | **refused**, `Operation not supported` |
| `ln` (hardlink) | **0 KiB** |
| Heyarr's own `Link(Reflink)`, degrading | **12 KiB** for a 256 MiB file |

So:

- **Adoption is free *only if the CAS is on the media filesystem*.** Ingesting
  an existing library costs ~12 KiB per file — but that measurement was taken
  with the source and the store on the same disk, which is the only arrangement
  in which it is true. See the warning below; getting this wrong is the single
  most expensive mistake available in this deployment.
- **The first rung is dead here.** ext4 has no block cloning. Nothing Heyarr
  does can change that; the ladder degrading is the designed behaviour and the
  mode actually achieved is recorded per blob.
- **The hardlink consequence is now the default case, not an edge case.** ADR-0014
  already notes that a hardlink means the CAS and the original path share bytes,
  so an external tool writing in place corrupts a blob. On this host that is what
  happens to *every* ingested file. Integrity scanning (§57) and quarantine
  (ADR-0018) are therefore routine operational requirements here, not a safety
  net — schedule `heyarr fsck --deep` and mean it.

Tracked as [#43](https://github.com/rarebit-one/heyarr-core/issues/43).

### If ZFS is adopted later

Not the current state. Recorded because the reasoning is the same reasoning that
would justify the change, and because the two datasets genuinely want different
settings:

| dataset | `recordsize` | why |
|---|---|---|
| `.../heyarr/cas` | `1M` | Blobs are written once, read sequentially and in large ranges. A large record means fewer, bigger IOs and less metadata per byte. |
| `.../heyarr/db` | `64K` | SQLite writes 4 KiB pages in a WAL. A 1M record turns every small page write into a 1 MiB read-modify-write. |

Putting both on one dataset makes both worse, which is the whole reason to split
them. Block cloning additionally needs `zpool set feature@block_cloning=enabled`
and ZFS ≥ 2.2; without it, ingest degrades to hardlink exactly as it does on
ext4 today and nothing else changes.

## The one thing to get right: where the CAS lives

**The content store must be on the same filesystem as the library.** Not nearby,
not on faster storage — the same filesystem.

Both cheap rungs of ADR-0014's ladder require it. Reflink because block cloning
is a filesystem operation; hardlink because a hardlink is a second name for one
inode, and inodes do not span devices. Put them on different filesystems and
every rung fails, so **every ingest is a full byte copy** and adopting a library
consumes a second complete copy of it. On this host that is 4 TB.

It is easy to get wrong because the *default* gets it wrong here. `data_dir`
puts the store at `<data_dir>/cas`, so the shipped default of
`/var/lib/heyarr` lands the CAS on the root NVMe while the library sits on
`/dev/sdb1`:

```
$ stat -c '%d %n' /var/lib/heyarr /srv/media
64512 /var/lib/heyarr
2065  /srv/media
```

Different devices, so `os.Link` returns `EXDEV`, the ladder degrades past
hardlink, and ingest copies. Silently — one file at a time.

So on this host, override the store's location and leave the database where it
is:

```yaml
data_dir: /var/lib/heyarr        # database and socket, on the fast NVMe
cas:
  root: /srv/media/heyarr/cas    # the store, beside the library it adopts
```

That split is right on its own merits, not just for reflink: SQLite wants low
latency and small writes, the CAS wants to be next to the bytes it shares.

The unit must then be told the store is writable, since `ProtectSystem=strict`
makes everything else read-only:

```ini
[Service]
ReadWritePaths=/srv/media/heyarr
```

Heyarr warns about this at startup, once per library root — `ingest from this
library will COPY every file rather than share its bytes` — which is the
warning ADR-0014 always promised and did not implement until this was found.

## Filesystem layout

FHS, so that a package and a hand install put things in the same places:

```
/usr/local/bin/heyarr              the binary
/etc/heyarr/config.yaml            configuration, 0640 root:heyarr
/var/lib/heyarr/heyarr.db          the controller database
/srv/media/heyarr/cas/             the content-addressed store — see above,
                                   it must share a filesystem with the library
/run/heyarr/                       the unix socket
/srv/media/<library>/              the media, read-only to Heyarr
```

`StateDirectory=heyarr` creates and owns `/var/lib/heyarr`, so systemd handles
its ownership and mode rather than a postinstall script that runs once and then
never again.

## Service account, group and ACLs (§74)

Application-level capabilities authorise the *caller*. These contain the
*process*, and both are necessary.

```bash
sudo useradd --system --home-dir /var/lib/heyarr --shell /usr/sbin/nologin heyarr
sudo groupadd -f media
sudo usermod -aG media heyarr
```

Heyarr reads the library through **group membership**, never by owning it. An
ingest bug then cannot rewrite the originals, which on this host is not
hypothetical: every managed blob is a hardlink to the original file, so a write
through either name is a write through both.

The unit mounts the library read-only (`ReadOnlyPaths=/srv/media` — change it to
your mount) and Milestone 1 never needs to write there. Only relax that when you
adopt a workflow that genuinely requires it.

Where a media tree has mixed ownership from years of accumulation, a default ACL
is less brittle than a recursive `chgrp`:

```bash
sudo setfacl -R  -m g:media:rX /srv/media/<library>
sudo setfacl -R -d -m g:media:rX /srv/media/<library>   # inherited by new files
```

`rX` rather than `rx`: capital X grants execute on directories only, so it does
not mark every media file executable.

## The unit

`deploy/systemd/heyarr.service`, installed to `/etc/systemd/system/`.

```bash
sudo install -m0644 deploy/systemd/heyarr.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now heyarr
```

Verified on this host with systemd 255:

```
$ systemd-analyze verify heyarr.service
(no output — clean)

$ systemd-analyze security heyarr.service
→ Overall exposure level for heyarr.service: 1.3 OK 🙂
```

The remaining findings are inherent to what Heyarr is rather than things left
undone, and are recorded here so nobody has to rediscover why:

| finding | why it stays |
|---|---|
| `RestrictAddressFamilies=~AF_(INET\|INET6)` | It is an HTTP server. |
| `RestrictAddressFamilies=~AF_UNIX` | The unix socket is the preferred local transport (ADR-0011). |
| `PrivateNetwork=` | Same: it serves the network. |
| `SupplementaryGroups=` | The `media` group is the whole read model above. |
| `PrivateUsers=` | Incompatible with reading the library through a supplementary group. |
| `IPAddressDeny=` | An allow list depends on the deployment; set it in a drop-in if your peers are known. |

Two directives are deliberately absent, and both would look like improvements:

- **No `Type=notify`.** Heyarr does not speak `sd_notify`. Claiming otherwise
  makes systemd wait for a `READY=1` that never arrives and then call a healthy
  service failed.
- **No `ExecReload`.** Heyarr handles `SIGINT` and `SIGTERM` and nothing else, so
  the obvious `kill -HUP $MAINPID` would not reload it — Go terminates on an
  unhandled `SIGHUP`. It would kill the service while telling you it was
  reloading. Restart instead.

`TimeoutStopSec=120` is longer than the supervisor's own drain, so systemd does
not kill the process mid-drain and turn finished work into retried work.

## Verifying the deployment

`scripts/acceptance.sh` is the gate, and it runs against the packaged binary on
this host, not only in CI. There is **no Go toolchain on the reference host**,
so both the binary and the fixture generator are cross-compiled and shipped:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o heyarr ./cmd/heyarr
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o genlibrary \
  ./internal/testutil/fixtures/cmd/genlibrary
scp heyarr genlibrary scripts/acceptance.sh <host>:/srv/media/heyarr-acceptance/
ssh <host> 'cd /srv/media/heyarr-acceptance && TMPDIR=$PWD/tmp ./scripts/acceptance.sh'
```

`TMPDIR` points at the media filesystem on purpose: running the demo on `/tmp`
would exercise the root filesystem and prove nothing about the disk the library
actually lives on.

Recorded result, on ext4 at `/srv/media`, with the full 200 MiB streaming
fixture:

```
87 assertions passed, exit 0, 16 seconds
```

## Operating notes

- **Schedule `heyarr fsck --deep`.** Not optional here — see the hardlink
  consequence above. It exits non-zero on damage, so it can drive an alert
  directly.
- **`heyarr gc` changes nothing by default.** It is dry-run at three separate
  levels; you have to ask for the destructive behaviour, and the grace window
  still applies.
- **Back up `/var/lib/heyarr/heyarr.db` only while it is at rest.** After a clean
  shutdown the file stands alone — SQLite removes the WAL on last close, and the
  acceptance demo asserts that. Copying a live database beside a populated `-wal`
  gives you a silently stale backup (§50).
