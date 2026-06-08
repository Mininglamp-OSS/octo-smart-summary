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
ALTER TABLE `summary_participant`
    ADD UNIQUE KEY `uk_summary_participant_task_user` (`task_id`, `user_id`);

-- +migrate Down
ALTER TABLE `summary_participant`
    DROP INDEX `uk_summary_participant_task_user`;

ALTER TABLE `summary_task`
    DROP INDEX `uk_live_schedule_binding`,
    DROP COLUMN `live_schedule_id`;
