# Deploying a peer on macOS

**Read the support level first, because it is the whole point of this page.**

macOS is a **first-class build and test target and a second-class deployment
target** (#204). The binary is built for `darwin/arm64` on every release, the
CI matrix runs the full test suite on `macos-latest`, and the storage fabric
handles APFS on purpose — reflink via `Clonefile`, the 104-byte socket-path
limit, `unix`-tagged listeners. Peer-to-peer mTLS, pinning and revocation were
exercised between a macOS arm64 host and a Linux host during M4 with identical
BLAKE3 digests computed on both. **The fabric works on macOS.**

What does **not** exist on macOS is the confinement the Linux `systemd` unit
provides. This page ships a `launchd` unit so a Mac peer can at least be
*supervised* rather than run under `nohup` in a terminal that must stay open —
but it is deliberately weaker than the Linux unit, and the sections below say
exactly where. Deploy a macOS peer knowing that; do not assume `deploy/` gives
every platform parity.

## The unit

[`deploy/launchd/one.rarebit.heyarr.plist`](../../deploy/launchd/one.rarebit.heyarr.plist).
Read its comments before changing it, the same way the systemd unit asks.

## What confinement you get, and what you do not

The Linux unit scores **1.3** on `systemd-analyze security` (M1-19). There is no
equivalent number on macOS because there is no equivalent mechanism. This is the
honest mapping, not a claim of parity:

| systemd directive | launchd equivalent | reality on macOS |
|---|---|---|
| `User=` / `Group=` / `SupplementaryGroups=media` | `UserName` / `GroupName` | **Kept.** A dedicated unprivileged `_heyarr` account owns the data dir and reads the library through group membership, never by owning it (§74). |
| `Restart=on-failure` + `RestartSec` | `KeepAlive` + `ThrottleInterval` | **Kept**, and stronger: `KeepAlive=true` restarts a clean exit too, which a peer meant to stay a live site (ADR-0038) wants. |
| `TimeoutStopSec=120` | `ExitTimeOut` | **Kept.** SIGTERM drains in-flight jobs; the ceiling outlasts the supervisor's grace. |
| `ProtectSystem=strict` + `ReadWritePaths=` | — | **No equivalent.** launchd cannot make the filesystem read-only and carve writable exceptions. The account being unprivileged is the only thing standing between an ingest bug and the rest of the disk. |
| `ProtectHome=yes` | — | **No equivalent** beyond ordinary POSIX permissions. |
| `NoNewPrivileges=yes` | — | **No equivalent.** |
| `PrivateTmp` / `PrivateDevices` / `ProtectKernel*` / `ProtectProc` / `RestrictNamespaces` / `RestrictSUIDSGID` / `LockPersonality` / `MemoryDenyWriteExecute` / `RemoveIPC` | — | **No equivalent.** These are Linux namespace and kernel-hardening features with no launchd analogue. Apple's own answer is the App Sandbox, which requires entitlements baked into a code-signed bundle — a different packaging story this repo does not yet ship. |
| `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6` / `CapabilityBoundingSet=` / `SystemCallFilter=` | — | **No equivalent.** No syscall filtering, no capability bounding. |
| `MemoryMax` / `MemoryHigh` / `IOWeight` / `CPUWeight` / `TasksMax` | — | **No hard equivalent.** launchd has no memory ceiling; the closest levers are coarse (`ProcessType`, which this unit sets to `Interactive` so ingest is not throttled into invisibility). A runaway is not capped the way `MemoryMax=2G` caps it. |
| `RestrictRealtime` / `ProtectClock` / `ProtectHostname` / `ProtectControlGroups` | — | **No equivalent.** |

The one-line summary: **you get a supervised, unprivileged service and nothing
below it.** If a site needs the confinement the systemd unit provides, that site
should be a Linux peer.

## Two macOS-specific hazards a Linux operator will not expect

**App Nap and idle sleep.** A backgrounded macOS process can be throttled by App
Nap, and a Mac will sleep on its own idle timer — either of which takes a peer
that is supposed to stay reachable offline. The plist sets `ProcessType` to
`Interactive` to keep App Nap off the process, but **sleep is a machine setting a
plist cannot express.** A Mac expected to stay up as a site must be told to:

```
sudo pmset -a sleep 0 disablesleep 1     # a Mac mini kept awake as a peer
```

or, less drastically, run under `caffeinate` — but `pmset` is the durable
choice for a dedicated node. This is the macOS counterpart of a detail the
Linux unit never has to mention.

**No journal, so logs are files and rotation is manual.** launchd has no
`journald`. The unit writes stdout/stderr to `heyarr.log` under the data
directory; set up `newsyslog` (macOS's log rotator) or the file grows without
bound — another thing the systemd journal did for free.

## Storage: APFS clones work, the tuning does not exist

The ADR-0014 ladder is the same shape on macOS — `Clonefile` gives copy-on-write
reflink on APFS, degrading to hardlink then a byte copy off-APFS or across
filesystems, exactly as `FICLONE` does on Linux. So **adoption is free on APFS**
when the store and the library share the volume, on the same rule as Linux: put
`cas.root` on the same filesystem as the library (see
[`reference-linux-host.md`](reference-linux-host.md) for why getting this wrong
is the most expensive mistake available).

What does **not** carry over is the ZFS tuning
`reference-linux-host.md` describes — `recordsize`, the `block_cloning` feature,
the dataset split. APFS has clones without any of those knobs; there is nothing
to tune and nothing to enable. The §74 POSIX-ACL model is also a different animal
on macOS (`chmod +a`, not `setfacl`), which the account-setup steps below account
for.

## Install

```
# 1. A dedicated, unprivileged, login-less role account and group.
sudo dscl . -create /Groups/_heyarr
sudo dscl . -create /Groups/_heyarr PrimaryGroupID 470
sudo dscl . -create /Users/_heyarr
sudo dscl . -create /Users/_heyarr UserShell /usr/bin/false
sudo dscl . -create /Users/_heyarr PrimaryGroupID 470
sudo dscl . -create /Users/_heyarr NFSHomeDirectory /usr/local/var/heyarr

# 2. The data directory it owns, and config it can read but not write.
sudo mkdir -p /usr/local/var/heyarr /usr/local/etc/heyarr
sudo chown -R _heyarr:_heyarr /usr/local/var/heyarr
sudo cp your-config.yaml /usr/local/etc/heyarr/config.yaml
sudo chmod 640 /usr/local/etc/heyarr/config.yaml
sudo chown root:_heyarr /usr/local/etc/heyarr/config.yaml

# 3. Grant the account READ access to the library through the group, the §74
#    way — it must never own or be able to write the originals. macOS uses ACLs,
#    not setfacl:
sudo chmod -R +a "_heyarr allow read,execute,list,search,readattr" /path/to/library

# 4. The binary, the plist, and load it.
sudo cp heyarr /usr/local/bin/heyarr
sudo cp deploy/launchd/one.rarebit.heyarr.plist /Library/LaunchDaemons/
sudo chown root:wheel /Library/LaunchDaemons/one.rarebit.heyarr.plist
sudo launchctl bootstrap system /Library/LaunchDaemons/one.rarebit.heyarr.plist
```

`launchctl bootstrap system …` is the modern load verb (the old `launchctl load`
is deprecated). To check it, stop it, or read why it will not start:

```
launchctl print system/one.rarebit.heyarr
sudo launchctl bootout system/one.rarebit.heyarr
```

If it will not start, read the first line of `heyarr.log`: on any platform the
most common cause is the same refusal the Linux deployment hits — Heyarr will not
serve the library unauthenticated on a routable address (ADR-0011), a refusal to
start rather than a warning.

## Verifying

`scripts/acceptance.sh` — `make demo` — runs on macOS and is how you verify a
build on the Mac you just deployed to, the same as anywhere else. The CI
`macos-latest` leg runs it on every PR, so a build that reaches you has already
passed it once.
