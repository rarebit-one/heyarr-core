-- +goose Up
-- How a candidate's source is fetched (#836): torrent, usenet or direct. The
-- grab offers the selected release only to download clients of that protocol,
-- because an http(s) URL is a .torrent to one client and the payload to
-- another, and both accept it. Empty on rows written before this migration and
-- on releases whose provider did not say; those are offered to every client,
-- as they always were.
ALTER TABLE release_candidates ADD COLUMN protocol TEXT NOT NULL DEFAULT ''
    CHECK (protocol IN ('', 'torrent', 'usenet', 'direct'));

-- +goose Down
ALTER TABLE release_candidates DROP COLUMN protocol;
