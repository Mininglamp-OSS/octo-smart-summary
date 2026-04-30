-- Migration: Add indexes for batch status polling API performance
-- Issue: https://github.com/Mininglamp-OSS/octo-smart-summary/issues/2

-- Composite index for batch authorization participant check
CREATE INDEX idx_participant_task_user ON summary_participant(user_id, task_id);

-- Covering index for progress subquery (latest event per task)
CREATE INDEX idx_event_task_id ON summary_event(task_id, id DESC);
