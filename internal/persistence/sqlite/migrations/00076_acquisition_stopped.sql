-- +goose Up
-- A transfer the download client holds stopped (#883): beside trouble, and not
-- trouble, because a stopped transfer must not fail its want. The poll writes
-- what the client reports on every pass; acquisition status shows it once it
-- has lasted past a grace, with the reason.
--
-- stopped is '' while the transfer runs, 'held' for one the client added
-- stopped by its own default and that has never run (#882), and 'after_run'
-- for one stopped after it ran: an operator's pause, or a client with no way
-- for Heyarr to start it. stopped_since is when the poll first saw the
-- current reason, NULL while it runs; a change of reason restarts it.
ALTER TABLE acquisitions ADD COLUMN stopped TEXT NOT NULL DEFAULT ''
    CHECK (stopped IN ('', 'held', 'after_run'));
ALTER TABLE acquisitions ADD COLUMN stopped_since TEXT;

-- +goose Down
ALTER TABLE acquisitions DROP COLUMN stopped_since;
ALTER TABLE acquisitions DROP COLUMN stopped;
