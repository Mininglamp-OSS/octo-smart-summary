-- +migrate Up
-- Additive compatibility schema only. This migration does not activate writers,
-- switch current pointers, delete history, or infer historical provenance.
ALTER TABLE summary_task
  ADD COLUMN created_via VARCHAR(16) NOT NULL DEFAULT 'unknown',
  ADD COLUMN generation_spec_json JSON NULL,
  ADD COLUMN config_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN content_revision BIGINT NOT NULL DEFAULT 0;

ALTER TABLE summary_personal_result
  ADD COLUMN content_revision BIGINT NOT NULL DEFAULT 0;

ALTER TABLE summary_result
  ADD COLUMN content_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN base_content_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN generation_spec_snapshot JSON NULL,
  ADD COLUMN generation_id VARCHAR(36) NULL,
  ADD COLUMN edited_by VARCHAR(64) NOT NULL DEFAULT '',
  ADD COLUMN restored_from_version_id BIGINT NULL,
  ADD COLUMN restored_at DATETIME(6) NULL,
  ADD UNIQUE INDEX uk_result_generation (generation_id);

ALTER TABLE summary_personal_result_version
  ADD COLUMN content_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN base_content_revision BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN generation_spec_snapshot JSON NULL,
  ADD COLUMN generation_id VARCHAR(36) NULL,
  ADD COLUMN edited_at DATETIME(6) NULL,
  ADD COLUMN edited_by VARCHAR(64) NOT NULL DEFAULT '',
  ADD COLUMN restored_from_version_id BIGINT NULL,
  ADD COLUMN restored_at DATETIME(6) NULL,
  ADD UNIQUE INDEX uk_personal_generation_user (generation_id, user_id);

-- Initial revisions reflect the number of retained rows, not MAX(version):
-- historical pruning means the two are not necessarily equal. Missing formal
-- personal history remains a read-only provisional V1 until the first write.
UPDATE summary_task t
JOIN (SELECT task_id, COUNT(*) AS revision FROM summary_result GROUP BY task_id) v
  ON v.task_id = t.id
SET t.content_revision = v.revision;

UPDATE summary_personal_result p
LEFT JOIN (SELECT task_id, user_id, COUNT(*) AS revision
           FROM summary_personal_result_version GROUP BY task_id, user_id) v
  ON v.task_id = p.task_id AND v.user_id = p.user_id
SET p.content_revision = COALESCE(v.revision, IF(TRIM(p.content) = '', 0, 1));

UPDATE summary_result r JOIN summary_task t
  ON r.id = t.current_result_id AND r.task_id = t.id
SET r.content_revision = t.content_revision;
UPDATE summary_personal_result_version v JOIN summary_personal_result p
  ON v.id = p.current_version_id AND v.task_id = p.task_id AND v.user_id = p.user_id
SET v.content_revision = p.content_revision;

-- Historical result (task_id, version) duplicates must be inspected before a
-- separate write-enablement migration adds that unique key. Never drop or
-- renumber duplicate user history to make an index succeed.

-- +migrate Down
-- Forward-only compatibility schema: rollback must retain new data and must
-- not restore binaries that prune history or switch pointers on restore.
SELECT 1;
