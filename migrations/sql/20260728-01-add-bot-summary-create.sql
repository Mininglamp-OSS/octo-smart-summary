-- +migrate Up
ALTER TABLE summary_task
    ADD COLUMN creator_bot_id VARCHAR(64) NOT NULL DEFAULT ''
    COMMENT 'personal bot uid when created through the bot API';

CREATE INDEX idx_summary_task_creator_bot_id
    ON summary_task (creator_bot_id);

CREATE TABLE summary_bot_create_idempotency (
    id BIGINT NOT NULL AUTO_INCREMENT,
    space_id VARCHAR(64) NOT NULL,
    bot_id VARCHAR(64) NOT NULL,
    idempotency_key VARCHAR(128) NOT NULL,
    request_hash CHAR(64) NOT NULL,
    task_id BIGINT NOT NULL,
    created_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uk_bot_summary_idempotency (space_id, bot_id, idempotency_key),
    KEY idx_bot_summary_idempotency_task (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS summary_bot_create_idempotency;
DROP INDEX idx_summary_task_creator_bot_id ON summary_task;
ALTER TABLE summary_task DROP COLUMN creator_bot_id;
