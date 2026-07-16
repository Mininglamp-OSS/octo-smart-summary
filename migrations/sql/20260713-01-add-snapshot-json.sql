-- +migrate Up
ALTER TABLE summary_personal_result
    ADD COLUMN snapshot_json MEDIUMTEXT DEFAULT NULL COMMENT 'agent 生成本 version 时的完整快照 JSON,仅 trigger_type=agent 的记录填充';

-- +migrate Down
ALTER TABLE summary_personal_result DROP COLUMN snapshot_json;
