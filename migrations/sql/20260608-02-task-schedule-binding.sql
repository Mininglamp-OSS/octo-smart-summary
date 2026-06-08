-- +migrate Up
-- Scheduled summary binding & dedup integrity.
--
-- 1) One-to-one live binding between a task and its schedule.
--    live_schedule_id is a STORED generated column that equals schedule_id only
--    while the task is live (not soft-deleted) and actually bound; otherwise
--    NULL. A UNIQUE key over it therefore guarantees a schedule is bound to at
--    most one live task, while soft-deleted / unbound tasks (NULL) are exempt
--    from the constraint. The API maps a 1062 on this index to a clean 409.
ALTER TABLE `summary_task`
    ADD COLUMN `live_schedule_id` BIGINT
        GENERATED ALWAYS AS (
            CASE WHEN `deleted_at` IS NULL AND `schedule_id` IS NOT NULL
                 THEN `schedule_id`
                 ELSE NULL
            END
        ) STORED AFTER `schedule_id`,
    ADD UNIQUE KEY `uk_live_schedule_binding` (`live_schedule_id`);

-- 2) A participant is unique per (task, user). The worker upserts the creator
--    participant with ON CONFLICT(task_id,user_id) DO NOTHING, which requires
--    this unique key to exist.
--
--    Existing databases may already contain duplicate (task_id,user_id) rows
--    created before this constraint existed (CreateSummary did not de-dup
--    non-creator participants). Adding the UNIQUE key directly would fail on
--    such data, so first collapse duplicates to a single best row, keeping the
--    participant that actually carries a personal result (completed/submitted or
--    non-empty content); among content-bearing rows keep the largest id, else
--    fall back to MIN(id). Dependent personal-result / chunk / source rows of
--    the discarded participants are removed in the same statement.
DELETE sp, pr, sc, ss
FROM `summary_participant` sp
JOIN (
    SELECT ranked.task_id, ranked.user_id, ranked.id AS keep_id
    FROM (
        SELECT scored.id, scored.task_id, scored.user_id,
               ROW_NUMBER() OVER (
                   PARTITION BY scored.task_id, scored.user_id
                   ORDER BY
                       scored.has_content DESC,
                       CASE WHEN scored.has_content = 1 THEN scored.id ELSE NULL END DESC,
                       CASE WHEN scored.has_content = 0 THEN scored.id ELSE NULL END ASC
               ) AS rn
        FROM (
            SELECT sp0.id, sp0.task_id, sp0.user_id,
                   COALESCE(MAX(CASE
                       WHEN pr0.worker_status = 2
                         OR pr0.submitted_at IS NOT NULL
                         OR (pr0.content IS NOT NULL AND pr0.content <> '')
                       THEN 1 ELSE 0
                   END), 0) AS has_content
            FROM `summary_participant` sp0
            LEFT JOIN `summary_personal_result` pr0
                ON pr0.participant_ref_id = sp0.id
            GROUP BY sp0.id, sp0.task_id, sp0.user_id
        ) scored
    ) ranked
    WHERE ranked.rn = 1
) keep ON keep.task_id = sp.task_id AND keep.user_id = sp.user_id
LEFT JOIN `summary_personal_result` pr ON pr.participant_ref_id = sp.id
LEFT JOIN `summary_chunk` sc ON sc.participant_id = sp.id
LEFT JOIN `summary_source` ss ON ss.participant_id = sp.id
WHERE sp.id <> keep.keep_id;

ALTER TABLE `summary_participant`
    ADD UNIQUE KEY `uk_summary_participant_task_user` (`task_id`, `user_id`);

-- +migrate Down
ALTER TABLE `summary_participant`
    DROP INDEX `uk_summary_participant_task_user`;

ALTER TABLE `summary_task`
    DROP INDEX `uk_live_schedule_binding`,
    DROP COLUMN `live_schedule_id`;
