-- +migrate Up
ALTER TABLE summary_source
  ADD COLUMN source_version VARCHAR(128) NOT NULL DEFAULT '',
  ADD COLUMN source_hash CHAR(64) NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS summary_source_snapshot (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  summary_source_id BIGINT NOT NULL,
  content MEDIUMTEXT NOT NULL,
  content_bytes INT NOT NULL,
  content_hash CHAR(64) NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uk_summary_source_snapshot (summary_source_id),
  KEY idx_summary_source_snapshot_created_at (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- +migrate Down
DROP TABLE IF EXISTS summary_source_snapshot;

ALTER TABLE summary_source
  DROP COLUMN source_hash,
  DROP COLUMN source_version;
