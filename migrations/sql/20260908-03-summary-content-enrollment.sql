-- +migrate Up
ALTER TABLE summary_task
  ADD COLUMN content_protocol_version INT NOT NULL DEFAULT 0;

-- Preserve enrollment for coordinated writes made before this marker existed.
-- Read-only compatibility projections do not enroll a task.
UPDATE summary_task t
SET t.content_protocol_version = 1
WHERE EXISTS (SELECT 1 FROM summary_generation_run r WHERE r.task_id = t.id)
   OR EXISTS (SELECT 1 FROM summary_content_audit a WHERE a.task_id = t.id);

-- +migrate Down
-- Forward-only: removing the marker would re-enable destructive legacy paths.
SELECT 1;
