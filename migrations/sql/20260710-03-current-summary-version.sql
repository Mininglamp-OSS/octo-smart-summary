-- +migrate Up
ALTER TABLE `summary_task`
  ADD COLUMN `current_result_id` bigint NULL AFTER `schedule_id`,
  ADD KEY `idx_summary_task_current_result_id` (`current_result_id`);

UPDATE `summary_task` t
JOIN (
  SELECT r.`task_id`, r.`id`
  FROM `summary_result` r
  JOIN (
    SELECT `task_id`, MAX(`version`) AS `max_version`
    FROM `summary_result`
    GROUP BY `task_id`
  ) mv ON mv.`task_id` = r.`task_id` AND mv.`max_version` = r.`version`
  JOIN (
    SELECT `task_id`, `version`, MAX(`id`) AS `max_id`
    FROM `summary_result`
    GROUP BY `task_id`, `version`
  ) mi ON mi.`task_id` = r.`task_id` AND mi.`version` = r.`version` AND mi.`max_id` = r.`id`
) latest ON latest.`task_id` = t.`id`
SET t.`current_result_id` = latest.`id`
WHERE t.`current_result_id` IS NULL;

ALTER TABLE `summary_personal_result`
  ADD COLUMN `current_version_id` bigint NULL AFTER `model_version`,
  ADD KEY `idx_personal_result_current_version_id` (`current_version_id`);

UPDATE `summary_personal_result` pr
JOIN (
  SELECT v.`task_id`, v.`user_id`, v.`id`
  FROM `summary_personal_result_version` v
  JOIN (
    SELECT `task_id`, `user_id`, MAX(`version`) AS `max_version`
    FROM `summary_personal_result_version`
    GROUP BY `task_id`, `user_id`
  ) mv ON mv.`task_id` = v.`task_id` AND mv.`user_id` = v.`user_id` AND mv.`max_version` = v.`version`
  JOIN (
    SELECT `task_id`, `user_id`, `version`, MAX(`id`) AS `max_id`
    FROM `summary_personal_result_version`
    GROUP BY `task_id`, `user_id`, `version`
  ) mi ON mi.`task_id` = v.`task_id` AND mi.`user_id` = v.`user_id` AND mi.`version` = v.`version` AND mi.`max_id` = v.`id`
) latest ON latest.`task_id` = pr.`task_id` AND latest.`user_id` = pr.`user_id`
SET pr.`current_version_id` = latest.`id`
WHERE pr.`current_version_id` IS NULL;

-- +migrate Down
ALTER TABLE `summary_personal_result`
  DROP KEY `idx_personal_result_current_version_id`,
  DROP COLUMN `current_version_id`;

ALTER TABLE `summary_task`
  DROP KEY `idx_summary_task_current_result_id`,
  DROP COLUMN `current_result_id`;
