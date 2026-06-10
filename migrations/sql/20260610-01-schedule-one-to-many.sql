-- +migrate Up
-- 为 worker upsert creator participant 提供 ON CONFLICT(task_id,user_id) 所需唯一键。
ALTER TABLE `summary_participant`
    ADD UNIQUE KEY `uk_summary_participant_task_user` (`task_id`, `user_id`);

-- 折叠列表查询索引：按 schedule_id 取每组最新 task，混合手动 task 分页时走索引。
ALTER TABLE `summary_task`
    ADD INDEX `idx_space_schedule_deleted_id` (`space_id`, `schedule_id`, `deleted_at`, `id`);

-- +migrate Down
ALTER TABLE `summary_task`
    DROP INDEX `idx_space_schedule_deleted_id`;

ALTER TABLE `summary_participant`
    DROP INDEX `uk_summary_participant_task_user`;
