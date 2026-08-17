-- +migrate Up
-- SS-03: persist SummaryRun / SummarySpec.
--
-- Before SS-03, agent-summary run state lived only in context.Context + an
-- in-process cache and was thrown away when the request ended. That made runs
-- impossible to deduplicate, serialize, or resume, and left the "same request
-- replayed after an SSE downgrade" path re-fetching and re-summarizing.
--
-- These two tables move that state into MySQL as a versioned, idempotent,
-- resumable contract. Shipped dark behind AGENT_SUMMARY_V2_MODE (default off);
-- later stages (SS-05+) make the tools read the persisted Spec.
--
-- Migration is serialized by the existing GET_LOCK('smart_summary_migration')
-- in internal/db/migrate.go, so no separate migration job is needed.

CREATE TABLE `agent_summary_run` (
    `run_id`        VARCHAR(64)   NOT NULL COMMENT 'stable run identity (UUID); not auto-increment',
    `user_id`       VARCHAR(64)   NOT NULL COMMENT 'owner uid (from auth middleware); all reads owner-scoped',
    `session_id`    VARCHAR(128)  NOT NULL COMMENT 'agent session id',
    `request_id`    VARCHAR(128)  NOT NULL COMMENT 'client-generated per-submit id; idempotency key',
    `spec_id`       VARCHAR(64)   NOT NULL DEFAULT '' COMMENT 'current SummarySpec id',
    `spec_version`  INT           NOT NULL DEFAULT 0  COMMENT 'current SummarySpec version',
    `scope_policy`  VARCHAR(16)   NOT NULL DEFAULT 'closed' COMMENT 'closed | open',
    `status`        VARCHAR(32)   NOT NULL DEFAULT 'created' COMMENT 'task-flow state',
    `finish_status` VARCHAR(16)   NOT NULL DEFAULT '' COMMENT 'generation verdict COMPLETE|PARTIAL|FAILED, written by SS-07; column reserved now',
    `version`       INT           NOT NULL DEFAULT 0  COMMENT 'optimistic-lock counter for serialized updates',
    `created_at`    DATETIME(3)   NOT NULL,
    `updated_at`    DATETIME(3)   NOT NULL,
    PRIMARY KEY (`run_id`),
    UNIQUE KEY `uk_run_request` (`user_id`, `session_id`, `request_id`),
    KEY `idx_run_user_session` (`user_id`, `session_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE `agent_summary_spec` (
    `spec_id`       VARCHAR(64)   NOT NULL COMMENT 'stable spec identity (UUID)',
    `run_id`        VARCHAR(64)   NOT NULL COMMENT 'owning run',
    `version`       INT           NOT NULL COMMENT 'spec version within the run; new goal => new version',
    `spec_hash`     CHAR(64)      NOT NULL COMMENT 'sha256 of the canonical spec; change/reuse detection',
    `objective`     VARCHAR(1000) NOT NULL DEFAULT '' COMMENT 'summary objective',
    `topic`         VARCHAR(500)  NOT NULL DEFAULT '',
    `audience`      VARCHAR(200)  NOT NULL DEFAULT '',
    `language`      VARCHAR(32)   NOT NULL DEFAULT '',
    `detail_level`  VARCHAR(32)   NOT NULL DEFAULT '',
    `spec_json`     JSON          NOT NULL COMMENT 'full structured spec: output_sections/exclusions/channels/time_range/citation_policy',
    `field_sources` JSON          NOT NULL COMMENT 'per-field provenance: user|ui|default|inferred',
    `user_request`  TEXT          NOT NULL COMMENT 'original natural-language request',
    `created_at`    DATETIME(3)   NOT NULL,
    PRIMARY KEY (`spec_id`),
    UNIQUE KEY `uk_spec_run_version` (`run_id`, `version`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS `agent_summary_spec`;
DROP TABLE IF EXISTS `agent_summary_run`;
