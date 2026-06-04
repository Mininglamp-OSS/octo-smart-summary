-- +migrate Up
ALTER TABLE summary_schedule
    ADD COLUMN interval_days INT NOT NULL DEFAULT 0 COMMENT '间隔天数: 0=走 cron, >0=按固定间隔天数(如14=每两周)';

-- +migrate Down
ALTER TABLE summary_schedule DROP COLUMN interval_days;
