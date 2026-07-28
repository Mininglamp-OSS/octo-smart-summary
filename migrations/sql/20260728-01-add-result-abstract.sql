-- +migrate Up
-- Add a short plain-text abstract to summary results. Generated best-effort via
-- a lightweight LLM call (see internal/service/abstract.go) and rendered as the
-- "AI 摘要" callout at the top of the summary detail. Nullable-by-default (empty
-- string) so existing rows and generation failures simply show no callout.
ALTER TABLE `summary_result` ADD COLUMN `abstract` VARCHAR(300) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';
ALTER TABLE `summary_personal_result` ADD COLUMN `abstract` VARCHAR(300) COLLATE utf8mb4_unicode_ci NOT NULL DEFAULT '';

-- +migrate Down
ALTER TABLE `summary_result` DROP COLUMN `abstract`;
ALTER TABLE `summary_personal_result` DROP COLUMN `abstract`;
