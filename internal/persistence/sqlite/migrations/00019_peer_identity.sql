-- +goose Up
-- The local peer's Ed25519 identity (§26, ADR-0012, ADR-0010, M4-03).
--
-- The first migration of Milestone 4. peers.public_key has existed since
-- 00002_core.sql marked "reserved for M4" and nothing has ever written it;
-- this is the migration that makes writing it meaningful.
--
-- # What is NOT here, and why
--
-- The private key. It is a file in the data directory at 0600 and it is never
-- a column, because the database is the thing operators copy: backups stream
-- to peers (§48, ADR-0003), and a restore that carries the private key with it
-- produces two machines able to authenticate as one peer. That is the same
-- failure ADR-0010's two-places refusal exists to catch, arriving through the
-- one door the refusal cannot see. The public key is in the database precisely
-- because it is safe to copy — a peer membership record pins it (ADR-0012).
--
-- # Why an algorithm column for a decision already made
--
-- ADR-0012 chose Ed25519. Recording it anyway costs one TEXT column and is the
-- difference between a future rotation to another curve being a migration plus
-- a re-enrolment of every peer, and being a second accepted value. A key with
-- no algorithm beside it is a bag of bytes whose meaning lives in a comment.

ALTER TABLE peers ADD COLUMN key_algo TEXT;

-- When this peer's keypair was generated. Provenance an operator can read
-- without opening the CAS marker: "was this identity established before or
-- after the restore we are arguing about?" is the first question asked when
-- the refusal fires.
ALTER TABLE peers ADD COLUMN key_generated_at TEXT;

-- +goose Down
ALTER TABLE peers DROP COLUMN key_generated_at;
ALTER TABLE peers DROP COLUMN key_algo;
