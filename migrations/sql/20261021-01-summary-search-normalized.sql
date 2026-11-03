-- +migrate Up
ALTER TABLE summary_task
  ADD COLUMN search_title TEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
  ADD COLUMN search_topic TEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL;
-- Go backfill runs under the migration lock before API/worker startup. SQL
-- LOWER/collations cannot reproduce Go's Unicode width and full case folding.

-- +migrate Down
ALTER TABLE summary_task DROP COLUMN search_title, DROP COLUMN search_topic;
