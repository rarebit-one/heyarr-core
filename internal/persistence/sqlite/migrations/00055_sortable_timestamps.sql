-- +goose Up
-- Rewrite the timestamps SQL compares or sorts into the fixed-width layout
-- (sqlite.TimestampLayout).
--
-- # Why
--
-- These columns were written as time.RFC3339Nano, which trims trailing zeros
-- from the fractional second, so ".1Z" sorts after ".15Z" and a whole second
-- ("…:05Z") sorts after every instant inside it. SQLite compares TEXT byte by
-- byte, so `run_after <= ?`, `expires_at > ?` and `ORDER BY …_at` were wrong
-- whenever two values fell in the same second. The writers now pad the fraction
-- to nine digits; this brings the rows already on disk into the same layout.
--
-- # Why rewrite rather than let old rows age out
--
-- Old and new values mixed are wrong in exactly the same one-second window as
-- old values alone, so skipping this would be no WORSE than before. But the
-- ordered columns (acquisitions.created_at, access_leases.issued_at) are
-- history, not transient state — their same-second mis-orderings would never
-- age out — and a column that is fixed-width only "from some date on" is an
-- invariant nobody can rely on. The rewrite is cheap, and idempotent: only a
-- value that is a UTC RFC 3339 timestamp shorter than the fixed width is
-- touched. A value in any other shape (an offset rather than "Z") is left as
-- it is; it was never sortable and is not made worse.
--
-- # Why 00055 and not the lowest free number (00021's policy)
--
-- This UPDATEs tables that 00032 and 00035 create. A fresh database applies
-- migrations in version order, so at 00026 those tables do not exist yet and
-- the UPDATE would fail. A data migration has to come after the DDL it touches.
--
-- # The rewrite
--
-- "2006-01-02T15:04:05Z" is 20 bytes, and the fixed-width form is 30. A value
-- with no fraction gains ".000000000"; a value with a 1-8 digit fraction gains
-- the zeros that pad it to nine. Padding with zeros does not change the
-- instant, so every reader parses the same time before and after.

UPDATE jobs SET run_after = CASE
    WHEN length(run_after) = 20 THEN substr(run_after, 1, 19) || '.000000000Z'
    ELSE substr(run_after, 1, length(run_after) - 1) || substr('000000000', 1, 30 - length(run_after)) || 'Z'
  END
  WHERE run_after GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
    AND (length(run_after) = 20 OR (substr(run_after, 20, 1) = '.' AND length(run_after) BETWEEN 22 AND 29));

UPDATE jobs SET lease_expires_at = CASE
    WHEN length(lease_expires_at) = 20 THEN substr(lease_expires_at, 1, 19) || '.000000000Z'
    ELSE substr(lease_expires_at, 1, length(lease_expires_at) - 1) || substr('000000000', 1, 30 - length(lease_expires_at)) || 'Z'
  END
  WHERE lease_expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
    AND (length(lease_expires_at) = 20 OR (substr(lease_expires_at, 20, 1) = '.' AND length(lease_expires_at) BETWEEN 22 AND 29));

UPDATE access_leases SET issued_at = CASE
    WHEN length(issued_at) = 20 THEN substr(issued_at, 1, 19) || '.000000000Z'
    ELSE substr(issued_at, 1, length(issued_at) - 1) || substr('000000000', 1, 30 - length(issued_at)) || 'Z'
  END
  WHERE issued_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
    AND (length(issued_at) = 20 OR (substr(issued_at, 20, 1) = '.' AND length(issued_at) BETWEEN 22 AND 29));

UPDATE access_leases SET expires_at = CASE
    WHEN length(expires_at) = 20 THEN substr(expires_at, 1, 19) || '.000000000Z'
    ELSE substr(expires_at, 1, length(expires_at) - 1) || substr('000000000', 1, 30 - length(expires_at)) || 'Z'
  END
  WHERE expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
    AND (length(expires_at) = 20 OR (substr(expires_at, 20, 1) = '.' AND length(expires_at) BETWEEN 22 AND 29));

UPDATE worker_capabilities SET expires_at = CASE
    WHEN length(expires_at) = 20 THEN substr(expires_at, 1, 19) || '.000000000Z'
    ELSE substr(expires_at, 1, length(expires_at) - 1) || substr('000000000', 1, 30 - length(expires_at)) || 'Z'
  END
  WHERE expires_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
    AND (length(expires_at) = 20 OR (substr(expires_at, 20, 1) = '.' AND length(expires_at) BETWEEN 22 AND 29));

UPDATE release_candidates SET searched_at = CASE
    WHEN length(searched_at) = 20 THEN substr(searched_at, 1, 19) || '.000000000Z'
    ELSE substr(searched_at, 1, length(searched_at) - 1) || substr('000000000', 1, 30 - length(searched_at)) || 'Z'
  END
  WHERE searched_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
    AND (length(searched_at) = 20 OR (substr(searched_at, 20, 1) = '.' AND length(searched_at) BETWEEN 22 AND 29));

UPDATE acquisitions SET last_seen_at = CASE
    WHEN length(last_seen_at) = 20 THEN substr(last_seen_at, 1, 19) || '.000000000Z'
    ELSE substr(last_seen_at, 1, length(last_seen_at) - 1) || substr('000000000', 1, 30 - length(last_seen_at)) || 'Z'
  END
  WHERE last_seen_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
    AND (length(last_seen_at) = 20 OR (substr(last_seen_at, 20, 1) = '.' AND length(last_seen_at) BETWEEN 22 AND 29));

UPDATE acquisitions SET created_at = CASE
    WHEN length(created_at) = 20 THEN substr(created_at, 1, 19) || '.000000000Z'
    ELSE substr(created_at, 1, length(created_at) - 1) || substr('000000000', 1, 30 - length(created_at)) || 'Z'
  END
  WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
    AND (length(created_at) = 20 OR (substr(created_at, 20, 1) = '.' AND length(created_at) BETWEEN 22 AND 29));

UPDATE acquisition_state SET phase_entered_at = CASE
    WHEN length(phase_entered_at) = 20 THEN substr(phase_entered_at, 1, 19) || '.000000000Z'
    ELSE substr(phase_entered_at, 1, length(phase_entered_at) - 1) || substr('000000000', 1, 30 - length(phase_entered_at)) || 'Z'
  END
  WHERE phase_entered_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
    AND (length(phase_entered_at) = 20 OR (substr(phase_entered_at, 20, 1) = '.' AND length(phase_entered_at) BETWEEN 22 AND 29));

-- +goose Down
-- Nothing to undo. The padded values are still RFC 3339 and still parse as
-- RFC3339Nano, so a binary from before this migration reads them unchanged, and
-- stripping the zeros back off would only reintroduce the mis-ordering.
SELECT 1;
