-- +goose Up
-- `rom-indexed` gains its region preference (ADR-0121 §5). Seeding never
-- updates a profile that already exists, so an installation seeded before this
-- keeps `prefer = []` and would never rank by region. This brings an UNTOUCHED
-- seeded row up to the new default. The match is the whole row as it was
-- seeded, because an API edit keeps `seeded = 1`: a row an operator changed in
-- any way is theirs and is left alone.
UPDATE quality_profiles
   SET prefer = '[{"attribute":"region","op":"eq","value":"usa","weight":20},{"attribute":"region","op":"in","value":["usa","world"],"weight":10},{"attribute":"region","op":"neq","value":"usa","weight":10},{"attribute":"region","op":"nin","value":["usa","world","europe"],"weight":-10}]',
       description = 'Starting profile for searched ROMs. Accepts a nonempty release for the wanted platform, prefers USA, then World, then Europe, and stops once held.'
 WHERE name = 'rom-indexed'
   AND seeded = 1
   AND content_types = '["rom"]'
   AND prefer = '[]'
   AND accept = '[{"attribute":"size_bytes","op":"gte","value":1}]'
   AND terminal = '[{"attribute":"size_bytes","op":"gte","value":1}]'
   AND description = 'Starting profile for searched ROMs. Accepts a nonempty release for the wanted platform and stops once held; region and revision are not judged yet.';

-- +goose Down
UPDATE quality_profiles
   SET prefer = '[]',
       description = 'Starting profile for searched ROMs. Accepts a nonempty release for the wanted platform and stops once held; region and revision are not judged yet.'
 WHERE name = 'rom-indexed'
   AND seeded = 1
   AND content_types = '["rom"]'
   AND accept = '[{"attribute":"size_bytes","op":"gte","value":1}]'
   AND terminal = '[{"attribute":"size_bytes","op":"gte","value":1}]'
   AND prefer = '[{"attribute":"region","op":"eq","value":"usa","weight":20},{"attribute":"region","op":"in","value":["usa","world"],"weight":10},{"attribute":"region","op":"neq","value":"usa","weight":10},{"attribute":"region","op":"nin","value":["usa","world","europe"],"weight":-10}]'
   AND description = 'Starting profile for searched ROMs. Accepts a nonempty release for the wanted platform, prefers USA, then World, then Europe, and stops once held.';
