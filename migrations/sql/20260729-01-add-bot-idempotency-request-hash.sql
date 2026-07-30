-- +migrate Up
-- Add request_hash to the bot-create idempotency table so a client that
-- reuses the same (space_id, bot_id, idempotency_key) tuple with a different
-- body gets HTTP 409/40009 instead of a silent return of the original task,
-- mirroring summary_share_snapshot.RequestHash (see share.go:244).
--
-- Split from 20260728-01 because sql-migrate keys applied migrations by
-- filename and does not checksum contents: editing an already-applied
-- migration in place would leave dev/staging databases that ran the earlier
-- version stuck on the column-less schema, breaking every create and every
-- replay for the affected environment. See PR #181 round-3 review P1-2.
--
-- DEFAULT '' is intentional: rows written before this column existed had no
-- hash to compare against. The handler treats any binding with an empty
-- persisted hash as "different intent" on the next replay, which is the safe
-- conservative choice for pre-existing bindings (worst case: a legitimate
-- replay returns 40009 once, the client picks a fresh key, and moves on —
-- never silent misroute of a different request).
ALTER TABLE summary_bot_create_idempotency
    ADD COLUMN request_hash CHAR(64) NOT NULL DEFAULT ''
    COMMENT 'sha256(canonical request) for idempotency-body binding';

-- +migrate Down
ALTER TABLE summary_bot_create_idempotency DROP COLUMN request_hash;
