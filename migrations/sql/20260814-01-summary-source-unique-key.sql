-- +migrate Up
-- R10 (yujiawei, issue comment 5280351017): structural idempotency for
-- summary_source. The worker backfill is read-then-insert; scanStuckTasks
-- can flip Processing -> Pending on lease expiry without signalling the
-- original worker, so two workers can reach backfillSourcesFromMessages
-- concurrently and both persist the same rows. A unique key on
-- (task_id, source_type, source_id) makes duplicates impossible and lets
-- the writer use ON DUPLICATE KEY UPDATE semantics (INSERT IGNORE).
--
-- Step 1: remove pre-existing duplicates (keep the lowest id). Such rows
-- are semantically identical (same task + source triple); first writer wins.
DELETE s1 FROM summary_source s1
INNER JOIN summary_source s2
  ON s1.task_id = s2.task_id
 AND s1.source_type = s2.source_type
 AND s1.source_id = s2.source_id
 AND s1.id > s2.id;

-- Step 2: the unique key itself (name matches the GORM model tag so
-- AutoMigrate in tests and this migration agree).
ALTER TABLE summary_source
  ADD UNIQUE INDEX uk_summary_source_task_type_id (task_id, source_type, source_id);

-- +migrate Down
ALTER TABLE summary_source DROP INDEX uk_summary_source_task_type_id;
