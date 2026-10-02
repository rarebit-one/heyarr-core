## heyarr space rewrap

Rewrap every space key from the gen1 recovery blob to gen2 (void-which-binds ADR-0022 cutover)

### Synopsis

Move every space key from gen1 (Voidbind) to gen2 (Void-Which-Binds) during the
cutover (void-which-binds-go ADR-0022, C2 steps 4 to 7). The space keys do not
change and no content is re-encrypted: each key is sealed afresh to the gen2
laptop device and the gen2 recovery key. Three modes, one at a time:

--from <gen1.blob> --stage <dir> --expect-db <db copy> [--expect <export.json>]
    OFFLINE, no server. Opens the gen1 heyarr-recovery-blob-v1 written by the
    pre-cutover `heyarr space export-recovery` with the gen1 secret, through
    void-which-binds-go's read-only migrate/gen1 package. Seals each key to this
    machine's device key (the configured custody backend; it must unwrap here,
    offline) and to the gen2 recovery key, and writes <dir>, which must not
    exist: manifest.json, recovery.blob (a gen2 recovery blob), SHA256SUMS and
    STAGE-MAC. The staged spaces must be EXACTLY the rows of encrypted_spaces
    in --expect-db (a copy of the frozen controller's backup, opened
    read-only), and exactly the space_ids of --expect when given. Any missing
    or extra space is a hard stop and nothing is written. It then proves the
    stage, and prints its stage id (128 random bits, new for every stage) and
    the sha256 of the gen1 blob. Record the stage id, and check the sha256
    against the export's.

--prove <dir> --stage-id <id>
    Checks STAGE-MAC under the gen2 secret, checks the stage's id is --stage-id,
    opens every staged wrap with the
    gen2 secret alone and with this device, opens recovery.blob with the gen2
    secret, and checks all three give the same key for every space and that
    SHA256SUMS match.

--upload <dir> --stage-id <id>
    ONLINE, as the enrolled gen2 device. Proves the stage as --prove does,
    checks the controller holds exactly the staged spaces, checks each key
    opens the space's newest content (its latest snapshot, else its newest
    change; a space with no content is reported uploaded-empty), then uploads
    the device and gen2 recovery wraps and reads them back. Re-running it is
    safe. It never deletes a wrap, gen1 ones included. The controller accepts
    the recovery wrap only once `heyarr admin user rekey` has pinned the
    gen2 recovery key.

STAGE-MAC is an HMAC-SHA256 over the other three files, keyed from the gen2
secret, so every mode needs --gen2-secret-file: a stage that was changed after
--stage, by anyone without the gen2 secret, is refused before anything in it is
used. The MAC covers the stage id, and --prove and --upload refuse a stage
whose id is not --stage-id, the one this run's --stage printed: an older stage
made with the same gen2 secret (one from before a space key was rotated, say)
cannot stand in for it. Together they make a space with no content safe to
upload, with nothing on the controller to check its key against.

Secrets are read from FILES only, never argv or a prompt: --gen1-secret-file
and --gen2-secret-file, either of which (not both) may be "-" for standard
input. Each holds the recovery secret, or its SLIP-39 shares one per line.

```
heyarr space rewrap (--from <gen1.blob> --stage <dir> | --prove <dir> --stage-id <id> | --upload <dir> --stage-id <id>) --gen2-secret-file <f> [flags]
```

### Options

```
      --addr string               where the API is: a unix socket path, unix:///path, http://host:port or host:port (default: the unix socket in the data directory)
      --expect string             with --from: the gen1 export's --json output; the staged spaces must equal its space_ids
      --expect-db string          with --from: a COPY of the frozen controller database; the staged spaces must equal its encrypted_spaces
      --from string               stage from this gen1 recovery blob (offline)
      --gen1-secret-file string   with --from: read the gen1 recovery secret or shares from this file ("-": standard input)
      --gen2-secret-file string   every mode: read the gen2 recovery secret or shares from this file ("-": standard input); it keys the stage MAC
      --identity-dir string       with --from: where your gen2 user identity lives; when one is there, its recovery key must be the gen2 secret's (default: your config directory; VOID_WHICH_BINDS_IDENTITY_DIR overrides)
      --json                      emit machine-readable JSON
      --prove string              prove this stage directory (offline)
      --stage string              with --from: the stage directory to create (must not exist)
      --stage-id string           with --prove and --upload: the stage id --stage printed for this run; any other stage is refused
      --timeout duration          how long one request may take; streaming reads and the event stream are exempt (default 30s)
      --token string              bearer token (prefer HEYARR_TOKEN: a token in argv is visible in ps and shell history)
      --token-file string         read the bearer token from this file (default: <data_dir>/cli.token when it exists)
      --upload string             upload this stage directory's wraps as this enrolled device (online)
```

### Options inherited from parent commands

```
  -c, --config string       path to the configuration file (default: $HEYARR_CONFIG, else /etc/heyarr/config.yaml if present, else built-in defaults plus HEYARR_ environment)
      --device-dir string   where this machine's device key lives (default: your config directory; VOID_WHICH_BINDS_DEVICE_DIR overrides)
```

### See also

* [heyarr space](heyarr_space.md)	 - Create and read encrypted personal-state spaces (§38, §42, ADR-0049)
