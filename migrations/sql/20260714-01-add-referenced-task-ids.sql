-- +migrate Up
ALTER TABLE summary_task
    ADD COLUMN referenced_task_ids TEXT DEFAULT NULL COMMENT 'agent chat 引用的已有总结 task_id JSON 数组,首次生成或不引用时为 NULL';

-- +migrate Down
ALTER TABLE summary_task DROP COLUMN referenced_task_ids;
