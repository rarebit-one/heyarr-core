-- +goose Up
-- GET /works pages in title order and Home browses in creation order.
-- The original type index omits the id tie breaker, so SQLite sorts every
-- page even when content_type is fixed. These indexes match both keysets.
CREATE INDEX works_by_title ON works (sort_title, id);
CREATE INDEX works_by_recent ON works (created_at DESC, id DESC);
DROP INDEX works_by_type_and_title;
CREATE INDEX works_by_type_and_title ON works (content_type, sort_title, id);

-- +goose Down
DROP INDEX works_by_type_and_title;
CREATE INDEX works_by_type_and_title ON works (content_type, sort_title);
DROP INDEX works_by_recent;
DROP INDEX works_by_title;
