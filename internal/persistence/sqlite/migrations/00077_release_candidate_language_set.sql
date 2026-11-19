-- +goose Up
-- A release's language is a set (#888): "Org Hindi DDP 2.0 Eng" is {hi, en},
-- the way a ROM dump's region is. A candidate recorded before this carries a
-- single name; it becomes a set of one, so every stored candidate has the one
-- shape a search now writes. Scoring reads both shapes alike (a single name
-- holds a rule exactly as a set of one does), so this changes no verdict.
UPDATE release_candidates
   SET attributes = json_set(attributes, '$.language',
                             json_array(json_extract(attributes, '$.language')))
 WHERE json_type(attributes, '$.language') = 'text';

-- +goose Down
-- Nothing to undo: the code before this reads a set of names for any text
-- attribute and scores it the same way, so a set of one is already understood,
-- and narrowing a multi-language set back to one name would be a guess.
SELECT 1;
