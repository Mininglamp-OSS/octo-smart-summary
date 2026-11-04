-- +migrate Up
-- Only the current version with an identical payload and self-consistent
-- provenance can inherit a legacy materialized snapshot. Other NULL history
-- remains unknown; it must never borrow the current generation's sources.
UPDATE summary_personal_result_version AS v
JOIN summary_personal_result AS p
 ON p.current_version_id = v.id AND p.task_id = v.task_id AND p.user_id = v.user_id
SET v.snapshot_json = p.snapshot_json
WHERE v.snapshot_json IS NULL AND p.snapshot_json IS NOT NULL
 AND BINARY v.content = BINARY p.content
 AND BINARY v.citations_json <=> BINARY p.citations_json
 AND v.msg_count = p.msg_count AND v.total_token_used = p.total_token_used
 AND BINARY v.model_version = BINARY p.model_version
 AND JSON_VALID(p.snapshot_json)
 AND JSON_UNQUOTE(JSON_EXTRACT(IF(JSON_VALID(p.snapshot_json), p.snapshot_json, '{}'), '$.task_id')) = CAST(p.task_id AS CHAR)
 AND JSON_UNQUOTE(JSON_EXTRACT(IF(JSON_VALID(p.snapshot_json), p.snapshot_json, '{}'), '$.content_version')) = CAST(v.version AS CHAR);
-- +migrate Down
-- Data repair only. Preserve recovered provenance on rollback.
SELECT 1;
