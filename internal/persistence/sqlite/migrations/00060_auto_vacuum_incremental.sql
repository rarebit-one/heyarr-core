-- +goose NO TRANSACTION
-- +goose Up
-- Switch auto_vacuum from NONE to INCREMENTAL so that PRAGMA incremental_vacuum
-- (run after each retention prune cycle, #721) actually returns free pages to
-- the OS rather than leaving them in the freelist.
--
-- IMPLEMENTATION NOTE — why this migration needs NO TRANSACTION
-- VACUUM cannot run inside a transaction; goose wraps every migration in one by
-- default. The NO TRANSACTION annotation disables that wrapper for this file.
--
-- IMPLEMENTATION NOTE — why we VACUUM here, and what happens if we skip it
-- Setting auto_vacuum only changes the header value stored in the database
-- file. On a database that was originally created with auto_vacuum=NONE the
-- freelist tracking pages do not exist; PRAGMA incremental_vacuum is a no-op
-- until a full VACUUM rebuilds the file with those pages. Running VACUUM here,
-- once at migration time, means every subsequent incremental_vacuum call
-- actually reclaims space.
--
-- ON A LARGE DATABASE this VACUUM will take minutes and will temporarily
-- require roughly as much free disk space as the database itself. On a
-- production node with a 6.4 GB DB, budget ~10 minutes and ~7 GB of scratch
-- space. The controller refuses to start while a migration is in flight, so
-- plan this upgrade for a maintenance window.
--
-- ALTERNATIVE: if the one-time VACUUM is too disruptive, an operator may skip
-- the VACUUM by running the binary before the VACUUM and accepting that
-- incremental_vacuum will be a no-op until they run
--
--   sqlite3 /path/to/heyarr.db "PRAGMA auto_vacuum=INCREMENTAL; VACUUM;"
--
-- manually (while heyarr is stopped). The retention beat still runs and still
-- deletes rows; it just cannot return the freed pages to the OS until that
-- manual step is done. See #721 for the full rationale.

PRAGMA auto_vacuum = INCREMENTAL;
VACUUM;

-- +goose Down
PRAGMA auto_vacuum = NONE;
VACUUM;
