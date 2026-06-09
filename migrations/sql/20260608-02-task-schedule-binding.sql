-- +migrate Up
-- Scheduled summary binding & participant uniqueness.
--
-- 1) live_schedule_id is a STORED generated column equal to schedule_id only
--    while the task is live (not soft-deleted) and bound; otherwise NULL. The
--    UNIQUE key over it guarantees a schedule binds to at most one live task
--    (soft-deleted/unbound NULL rows are exempt). The API maps a 1062 here to 409.
ALTER TABLE `summary_task`
    ADD COLUMN `live_schedule_id` BIGINT
        GENERATED ALWAYS AS (
            CASE WHEN `deleted_at` IS NULL AND `schedule_id` IS NOT NULL
                 THEN `schedule_id`
                 ELSE NULL
            END
        ) STORED AFTER `schedule_id`,
    ADD UNIQUE KEY `uk_live_schedule_binding` (`live_schedule_id`);

-- 2) A participant is unique per (task, user); required by the worker's
--    ON CONFLICT(task_id,user_id) DO NOTHING upsert of the creator participant.
ALTER TABLE `summary_participant`
    ADD UNIQUE KEY `uk_summary_participant_task_user` (`task_id`, `user_id`);

-- +migrate Down
ALTER TABLE `summary_participant`
    DROP INDEX `uk_summary_participant_task_user`;

ALTER TABLE `summary_task`
    DROP INDEX `uk_live_schedule_binding`,
    DROP COLUMN `live_schedule_id`;
