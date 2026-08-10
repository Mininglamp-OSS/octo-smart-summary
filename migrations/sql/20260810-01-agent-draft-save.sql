-- +migrate Up
--
-- SUM-BE2 (Agent 草稿与安全落库) — schema for the trusted, idempotent
-- Agent draft save path.
--
-- Two orthogonal additions:
--
-- 1. Record which Agent chat + which assistant message + which snapshot
--    version produced each Agent-created summary. This is what SUM-9 flagged
--    as missing when the shared validator dropped AgentMessageID /
--    SnapshotVersion for lack of storage columns; BE-2 owns that storage.
--
--    The columns are nullable + default-empty so existing rows (bot / task /
--    scheduled paths) stay valid, and the Agent write path fills them
--    explicitly. They are read by SUM-10 review and by the future Refine
--    pipeline (parent_snapshot_version lookup).
--
-- 2. A dedicated idempotency table for Agent save, shaped after
--    summary_bot_create_idempotency (see 20260728-01) so the on-conflict
--    replay contract is identical: same key + same body => replay, same key
--    + different body => HTTP 409 with the original task_id. Space + user +
--    key is the unique tuple (Agent save is user-owned, not bot-owned).

ALTER TABLE summary_task
    ADD COLUMN agent_session_id VARCHAR(128) NOT NULL DEFAULT ''
    COMMENT 'agent chat session that produced this summary; empty for non-agent tasks (SUM-BE2)',
    ADD COLUMN agent_message_id BIGINT NOT NULL DEFAULT 0
    COMMENT 'agent_message.id of the assistant reply saved as this summary; 0 for non-agent tasks (SUM-BE2)',
    ADD COLUMN snapshot_version INT NOT NULL DEFAULT 0
    COMMENT 'snapshot version the client confirmed on save; matches personal_result.snapshot_json.snapshot_version (SUM-BE2)';

CREATE INDEX idx_summary_task_agent_session
    ON summary_task (agent_session_id);

CREATE TABLE summary_agent_save_idempotency (
    id BIGINT NOT NULL AUTO_INCREMENT,
    space_id VARCHAR(64) NOT NULL,
    user_id VARCHAR(64) NOT NULL,
    idempotency_key VARCHAR(128) NOT NULL,
    request_hash CHAR(64) NOT NULL DEFAULT '',
    task_id BIGINT NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_agent_save_idempotency (space_id, user_id, idempotency_key),
    KEY idx_agent_save_idempotency_task (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS summary_agent_save_idempotency;
DROP INDEX idx_summary_task_agent_session ON summary_task;
ALTER TABLE summary_task
    DROP COLUMN snapshot_version,
    DROP COLUMN agent_message_id,
    DROP COLUMN agent_session_id;
