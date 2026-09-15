-- +goose Up
-- Placement pins: an opaque, control-plane-visible desire that a ciphertext blob
-- should live on a peer (ADR-0096). A pin carries NO space id, path, or asset
-- link — recording which space a blob belongs to would be the membership leak the
-- vault design exists to deny (ADR-0021). The control plane learns "this blob
-- belongs on this peer" and never which vault or file it is.
--
-- A pin is what keeps a vault blob alive: a vault blob has no `assets` row by
-- design, and the drive-CRDT reference that would keep it is encrypted personal
-- state the control plane cannot read, so a placement pin is the only
-- control-plane-visible reason to retain the bytes (garbage collection counts a
-- pin as a reference) and to replicate them to the pinned peer.
--
-- No foreign keys, deliberately — the same reasoning as 00028_durability_evidence.
-- A pin is a desired fact that must outlive churn in `blobs` and `peers`: it may
-- be recorded before the bytes are present, and it must survive a `blobs` row
-- vanishing rather than being silently cascaded away. It is a ledger of intent,
-- not a relation; a stale pin is reconciled by its author (the device), not by a
-- cascade.
CREATE TABLE placement_pins (
    blob_hash  TEXT NOT NULL CHECK (blob_hash GLOB 'blake3:[0-9a-f]*' AND length(blob_hash) = 71),
    peer_id    TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (blob_hash, peer_id)
) STRICT;

CREATE INDEX placement_pins_by_peer ON placement_pins (peer_id);

-- +goose Down
DROP INDEX placement_pins_by_peer;
DROP TABLE placement_pins;
