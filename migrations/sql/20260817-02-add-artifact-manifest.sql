-- +migrate Up
-- SS-04: immutable evidence artifacts + frozen citation manifests.
--
-- Fixes the citation-drift defect: today getSessionMessagePool (mid-run) and
-- buildCitationsForSession (save-time) each re-sort the live evidence set and
-- re-derive CitationIndex, so any evidence row added between the two phases
-- shifts the ordinals and misaligns [n] markers. These tables freeze the pool
-- (artifact) and the ordinal mapping (manifest) so both phases read stable
-- numbers instead of recomputing.
--
-- SS-04 only builds the tables + freeze store (dark, additive). Wiring the
-- mid-run / save-time reads to the manifest lands with prepare_evidence (SS-05).
--
-- Serialized by the existing GET_LOCK('smart_summary_migration').

CREATE TABLE `agent_evidence_artifact` (
    `artifact_id`       VARCHAR(64)  NOT NULL COMMENT 'stable artifact id (UUID)',
    `run_id`            VARCHAR(64)  NOT NULL COMMENT 'owning run (SS-03)',
    `user_id`           VARCHAR(64)  NOT NULL COMMENT 'owner uid; all reads owner-scoped',
    `session_id`        VARCHAR(128) NOT NULL DEFAULT '' COMMENT 'agent session id',
    `revision`          INT          NOT NULL COMMENT 're-fetch revision; old revisions never mutated',
    `content_hash`      CHAR(64)     NOT NULL COMMENT 'sha256 of canonical ordered (channel_id,message_seq) list; idempotency key',
    `message_count`     INT          NOT NULL DEFAULT 0,
    `channel_count`     INT          NOT NULL DEFAULT 0,
    `actual_time_range` JSON         NOT NULL COMMENT '{start,end} unix seconds of the frozen pool',
    `failed_channels`   JSON         NOT NULL COMMENT 'channels that failed to fetch (coverage disclosure)',
    `truncated`         TINYINT(1)   NOT NULL DEFAULT 0,
    `created_at`        DATETIME(3)  NOT NULL,
    PRIMARY KEY (`artifact_id`),
    UNIQUE KEY `uk_artifact_hash` (`run_id`, `content_hash`),
    KEY `idx_artifact_run_rev` (`run_id`, `revision`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

CREATE TABLE `agent_citation_manifest` (
    `manifest_id`  VARCHAR(64)  NOT NULL COMMENT 'stable manifest id (UUID)',
    `artifact_id`  VARCHAR(64)  NOT NULL COMMENT 'the artifact revision this manifest freezes',
    `run_id`       VARCHAR(64)  NOT NULL,
    `user_id`      VARCHAR(64)  NOT NULL COMMENT 'owner uid; owner-scoped reads',
    `revision`     INT          NOT NULL,
    `entries`      MEDIUMTEXT   NOT NULL COMMENT 'JSON array [{channel_id,message_seq}] in frozen citation order; ordinal = index+1',
    `entry_count`  INT          NOT NULL DEFAULT 0,
    `created_at`   DATETIME(3)  NOT NULL,
    PRIMARY KEY (`manifest_id`),
    UNIQUE KEY `uk_manifest_artifact` (`artifact_id`),
    KEY `idx_manifest_run_rev` (`run_id`, `revision`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS `agent_citation_manifest`;
DROP TABLE IF EXISTS `agent_evidence_artifact`;
