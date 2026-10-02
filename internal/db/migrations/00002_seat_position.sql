-- +goose Up
-- Preserve the admin's seat order: sorting by label puts A10 before A2.
ALTER TABLE seats ADD COLUMN position INT NOT NULL DEFAULT 0;
ALTER TABLE seats ALTER COLUMN position DROP DEFAULT;
CREATE INDEX seats_show_position_idx ON seats (show_id, position);

-- +goose Down
DROP INDEX seats_show_position_idx;
ALTER TABLE seats DROP COLUMN position;
