-- +migrate Up
ALTER TABLE summary_agent_save_idempotency
 ADD COLUMN result_id BIGINT NOT NULL DEFAULT 0,
 ADD COLUMN result_version INT NOT NULL DEFAULT 0,
 ADD COLUMN result_kind VARCHAR(24) NOT NULL DEFAULT '';
ALTER TABLE summary_result ADD COLUMN snapshot_json MEDIUMTEXT NULL;
ALTER TABLE summary_personal_result_version ADD COLUMN snapshot_json MEDIUMTEXT NULL;
-- +migrate Down
ALTER TABLE summary_result DROP COLUMN snapshot_json;
ALTER TABLE summary_personal_result_version DROP COLUMN snapshot_json;
ALTER TABLE summary_agent_save_idempotency DROP COLUMN result_id, DROP COLUMN result_version, DROP COLUMN result_kind;
