-- +migrate Up
-- Stage 3 Blocker C (SUM-158 PR#158): persist citation evidence to DB.
-- Problem: buildCitationsForSession relies on 30-minute in-memory cache;
-- cache miss (expiry / eviction / restart) silently produces empty citations.
-- Solution: agent_message_evidence stores full pipeline.Message snapshots as JSON;
-- cache becomes acceleration layer; DB fallback ensures citations survive restarts.
CREATE TABLE `agent_message_evidence` (
    `user_id`    VARCHAR(64)   NOT NULL COMMENT 'message owner uid (from auth middleware)',
    `session_id` VARCHAR(128)  NOT NULL COMMENT 'agent session ID',
    `handle`     VARCHAR(128)  NOT NULL COMMENT 'cache handle from fetch_channel/peek_channel tool return',
    `evidence`   MEDIUMTEXT    NOT NULL COMMENT 'JSON-serialized []pipeline.Message snapshot (enriched with SenderName/SourceName/ChannelType)',
    `created_at` DATETIME      NOT NULL,
    `updated_at` DATETIME      NOT NULL,
    PRIMARY KEY (`user_id`, `session_id`, `handle`),
    KEY `idx_session_handle` (`session_id`, `handle`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS `agent_message_evidence`;
