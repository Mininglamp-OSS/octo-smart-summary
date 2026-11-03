-- +migrate Up
ALTER TABLE summary_result ADD COLUMN citation_owner_id VARCHAR(64) NOT NULL DEFAULT '';
-- Compatibility backfill for incremental deployments that already wrote trusted
-- optimization bindings before citation_owner_id was introduced. On a first
-- upgrade applying all three migrations before serving new code, the binding
-- columns have default values and this UPDATE normally matches no rows.
-- It does NOT reconstruct ownership for arbitrary pre-feature legacy results.
-- Backfill only versions with a server-authenticated optimization save binding.
-- CreatedBy alone is not proof of authorization to the underlying raw messages.
CREATE INDEX idx_save_result_task_kind ON summary_agent_save_idempotency (result_id, task_id, result_kind);
UPDATE summary_result AS r
JOIN summary_agent_save_idempotency AS s
  ON s.result_id = r.id AND s.task_id = r.task_id AND s.result_kind = 'team'
SET r.citation_owner_id = s.user_id
WHERE r.citation_owner_id = '';
-- +migrate Down
DROP INDEX idx_save_result_task_kind ON summary_agent_save_idempotency;
ALTER TABLE summary_result DROP COLUMN citation_owner_id;
