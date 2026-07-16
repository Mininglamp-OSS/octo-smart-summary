-- +migrate Up
-- Blocker 1 (SUM-158): agent_message 加 user_id 列 + owner-scoped 索引。
-- 因为 fresh feature、上游 main 无存量记录，直接 DROP + 重建最干净：
--   - 存量本地测试数据被丢弃（重跑一遍会话即可，无生产影响）
--   - 直接建 NOT NULL 强约束，避免遗留 NULL 数据混入后无法回收
--   - 主查询走 idx_user_session_created(user_id, session_id, id)，权限过滤+对话时序一次命中
--   - idx_session_created(session_id, id) 保留以兼容极少数纯 session 查询（诊断/管理面）
--   - 不加 UNIQUE(user_id, session_id)：会话本身多条消息，UNIQUE 会阻止同会话追加
--     跨用户 session_id 撞车靠 handler owner check 兜底：即使字面撞了各自过滤各自的
DROP TABLE IF EXISTS `agent_message`;

CREATE TABLE `agent_message` (
    `id`           BIGINT        NOT NULL AUTO_INCREMENT,
    `session_id`   VARCHAR(128)  NOT NULL,
    `user_id`      VARCHAR(64)   NOT NULL COMMENT '消息属主 uid，由服务端从鉴权中间件注入；session_id 查询必须组合 user_id',
    `role`         VARCHAR(16)   NOT NULL COMMENT 'user | assistant | tool (system 不落库，运行时注入)',
    `content`      MEDIUMTEXT        NULL,
    `tool_calls`   JSON              NULL COMMENT 'assistant 轮的 tool_calls 原样存，回喂需要',
    `tool_call_id` VARCHAR(128)      NULL COMMENT 'role=tool 时对应的 assistant tool_call id',
    `name`         VARCHAR(128)      NULL COMMENT 'role=tool 时的工具名',
    `created_at`   DATETIME      NOT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_user_session_created` (`user_id`, `session_id`, `id`),
    KEY `idx_session_created` (`session_id`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS `agent_message`;
