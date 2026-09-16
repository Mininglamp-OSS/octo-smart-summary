-- +migrate Up
ALTER TABLE summary_task ADD COLUMN generation_requirement TEXT NULL;

-- +migrate Down
ALTER TABLE summary_task DROP COLUMN generation_requirement;
