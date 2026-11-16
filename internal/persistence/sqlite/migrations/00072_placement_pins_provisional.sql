-- +goose Up
-- Provisional upload pins (ADR-0096 amendment, #714). A vault write uploads its
-- blobs before it pushes the drive change that names them, so a write that
-- fails in between left the blobs pinned for ever with nothing naming them.
--
-- provisional_until: NULL is a permanent pin, and every existing row keeps
-- NULL, so nothing already pinned changes meaning. A timestamp
-- (sqlite.FormatTimestamp, fixed width so it compares as text) is an upload
-- pin its writer has not yet confirmed; once it is in the past the pin-expiry
-- beat removes the row and GC's ordinary release and grace window take over.
--
-- uploader: the principal that uploaded the blob, so a restricted executor can
-- confirm only what it uploaded itself (ADR-0104 carve-out). NULL for rows
-- written before this migration and for pins that never came from an upload.
ALTER TABLE placement_pins ADD COLUMN provisional_until TEXT;
ALTER TABLE placement_pins ADD COLUMN uploader TEXT;

CREATE INDEX placement_pins_provisional ON placement_pins (provisional_until)
    WHERE provisional_until IS NOT NULL;

-- +goose Down
DROP INDEX placement_pins_provisional;
ALTER TABLE placement_pins DROP COLUMN uploader;
ALTER TABLE placement_pins DROP COLUMN provisional_until;
