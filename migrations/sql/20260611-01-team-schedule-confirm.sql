-- +migrate Up
-- Multi-participant scheduled summary (P0): confirm policy + pre-run reminder lead on
-- schedule template, plus runtime columns on task / personal_result. All columns carry
-- defaults that are equivalent to today's single-person behavior, so existing rows are
-- unaffected (online-safe add-column).
ALTER TABLE `summary_schedule`
    -- 0 = AUTO_ACCEPT（到点直接全员 accepted + 系统代补 submitted_at，无需人工确认；
    --     存量单人定时的等效语义，默认 0 实现无缝兼容）
    -- 1 = CONFIRM（P1，方案 B）：到点前 lead 分钟发提醒、开确认窗口
    -- 2 = CONFIRM_FALLBACK（P2，可选）：B 但「零确认」时降级为 AUTO
    ADD COLUMN `confirm_policy` TINYINT NOT NULL DEFAULT 0 AFTER `participant_config`,
    ADD COLUMN `confirm_lead_minutes` INT NOT NULL DEFAULT 0 AFTER `confirm_policy`;

-- R1：区分「系统代补 submitted_at」 vs「人工 /submit」。0=未表态/历史，1=人工，2=系统代补。
-- ⚠️ 表名是 summary_personal_result（带前缀），不是 personal_result —— 否则迁移失败。
-- retry_count：P0 Blocker-3 自愈。personal worker 失败时受控重试的计数（上限防无限重跑）。
ALTER TABLE `summary_personal_result`
    ADD COLUMN `submit_source` TINYINT NOT NULL DEFAULT 0 AFTER `submitted_at`,
    ADD COLUMN `retry_count` TINYINT NOT NULL DEFAULT 0 AFTER `worker_status`;

-- R3：预提醒幂等标志位（运行态）。NULL=本轮未发过提醒；已发则置 now。
-- 1->N 下每轮是全新 task，新 task 天生 NULL，无需「下轮重置」逻辑。
ALTER TABLE `summary_task`
    ADD COLUMN `reminder_sent_at` DATETIME NULL DEFAULT NULL AFTER `confirm_deadline`;

-- +migrate Down
ALTER TABLE `summary_task`            DROP COLUMN `reminder_sent_at`;
ALTER TABLE `summary_personal_result` DROP COLUMN `submit_source`, DROP COLUMN `retry_count`;
ALTER TABLE `summary_schedule`        DROP COLUMN `confirm_lead_minutes`, DROP COLUMN `confirm_policy`;
