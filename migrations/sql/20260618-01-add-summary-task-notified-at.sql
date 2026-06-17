-- +migrate Up
-- OCT-43: crash-safe idempotency anchor for the summary-notify tip. A
-- single-writer CAS (UPDATE ... SET notified_at = NOW() WHERE notified_at IS
-- NULL) guarantees exactly-once emit per task across worker restarts. NULL =
-- not yet notified.
ALTER TABLE `summary_task`
    ADD COLUMN `notified_at` DATETIME NULL DEFAULT NULL AFTER `confirm_deadline`;

-- +migrate Down
ALTER TABLE `summary_task` DROP COLUMN `notified_at`;
