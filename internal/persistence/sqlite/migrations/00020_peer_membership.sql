-- +goose Up
-- Peer membership: the enrolment record, and the constraint that makes a
-- public key an identity (§26, §7, ADR-0012, M4-04).
--
-- 00002_core.sql already models peers multi-peer (ADR-0010) and 00019 gave the
-- local one a keypair. What was missing is everything about a peer that is NOT
-- this node: when an operator admitted it, and the database-level guarantee
-- that one key names one peer.
--
-- # Why the unique index is the load-bearing line in this file
--
-- ADR-0012: peer public keys are "pinned by the controller-issued peer
-- membership record", and membership is the only trust root in the inter-peer
-- path. A key registered against two peer rows would mean one presented key
-- authorising two identities, and the code path that picks which one wins
-- would be deciding a trust question by ORDER BY. It is refused in the schema
-- rather than only in Go, because the schema is the half that also holds when
-- a row arrives through a migration, a restore, or a repair by hand.
--
-- It is partial because a peer legitimately has no key for a while: the self
-- row is created at first start (ADR-0010) and gets its key later, when
-- identity.Ensure runs. A full unique index would work in SQLite, which treats
-- NULLs as distinct, but says the wrong thing about intent.
CREATE UNIQUE INDEX peers_unique_public_key
    ON peers (public_key) WHERE public_key IS NOT NULL;

-- When this peer was admitted to the fabric.
--
-- Distinct from created_at, which is when the row appeared. For the self peer
-- they are the same instant; for every other peer they are the same instant
-- today and stop being so the moment a peer is removed and re-enrolled, which
-- is exactly the event an operator is reconstructing when they ask this
-- question. Revocation is deletion (ADR-0012), so re-enrolment is a new row
-- with a new id — and enrolled_at is what says the fabric's membership changed
-- rather than the catalog's.
ALTER TABLE peers ADD COLUMN enrolled_at TEXT;

-- Reachability (§35, M4-10). Reserved here rather than in a later migration
-- for the reason ADR-0010 gives about peers themselves: a column added later
-- is a migration plus a rewrite of every query that assumed it was absent.
-- NOTHING writes these yet. M4-10 owns the probe, the transitions and the
-- peer.health_changed emission; this file owns only where the answer lives.
--
-- 'unknown' rather than 'reachable' as the default is deliberate: a peer that
-- has never been probed has not been shown to be up, and a placement decision
-- that reads an unprobed peer as healthy is the failure this column exists to
-- make impossible.
ALTER TABLE peers ADD COLUMN health TEXT NOT NULL DEFAULT 'unknown'
    CHECK (health IN ('unknown', 'reachable', 'unreachable'));
ALTER TABLE peers ADD COLUMN last_seen_at TEXT;

-- Backfill: the self peer was enrolled when its row was written.
--
-- It stays nullable afterwards rather than becoming NOT NULL, and that is not
-- laziness. The self peer row is created by whichever role starts first, and
-- the worker and the peer start against the LOWEST schema they can work at
-- rather than the newest (they wait for the controller to migrate). A column
-- from the newest migration is therefore a column that may not exist when that
-- INSERT runs, so the catalog does not write it. Readers treat NULL as
-- created_at, which for a self peer is the same instant: the row appearing IS
-- when this node joined.
UPDATE peers SET enrolled_at = created_at WHERE enrolled_at IS NULL;

-- +goose Down
ALTER TABLE peers DROP COLUMN last_seen_at;
ALTER TABLE peers DROP COLUMN health;
ALTER TABLE peers DROP COLUMN enrolled_at;
DROP INDEX peers_unique_public_key;
