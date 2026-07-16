-- +migrate Up
CREATE TABLE IF NOT EXISTS `agent_message` (
    `id`           BIGINT        NOT NULL AUTO_INCREMENT,
    `session_id`   VARCHAR(128)  NOT NULL,
    `role`         VARCHAR(16)   NOT NULL COMMENT 'user | assistant | tool (system 不落库，运行时注入)',
    `content`      MEDIUMTEXT        NULL,
    `tool_calls`   JSON              NULL COMMENT 'assistant 轮的 tool_calls 原样存，回喂需要',
    `tool_call_id` VARCHAR(128)      NULL COMMENT 'role=tool 时对应的 assistant tool_call id',
    `name`         VARCHAR(128)      NULL COMMENT 'role=tool 时的工具名',
    `created_at`   DATETIME      NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_session_created` (`session_id`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS `agent_message`;
