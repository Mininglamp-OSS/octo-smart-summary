-- +migrate Up
-- PR#62 r5 Blocker1b (Jerry-Xin + lml2468 carry-over): enforce the one-to-one
-- task<->schedule binding at the DB layer as a backstop to the application-level
-- FOR UPDATE serialization in internal/api/handler/schedule.go.
--
-- Why a generated column instead of a plain UNIQUE(schedule_id):
--   * summary_task.schedule_id is nullable: an UNBOUND task has schedule_id =
--     NULL. MySQL allows many NULLs in a unique index, so unbound tasks never
--     conflict -- good.
--   * BUT a SOFT-DELETED task (deleted_at IS NOT NULL) KEEPS its schedule_id
--     (DeleteSummary cascades the schedule soft-delete but does NOT null the
--     task's schedule_id; DeleteSchedule nulls bound tasks' schedule_id on the
--     unbind path, but the task-delete cascade path does not). A plain
--     UNIQUE(schedule_id) would let a dead task permanently occupy the slot and
--     block legitimately re-binding that schedule to a new live task.
--
-- Solution: a STORED generated column `live_schedule_id` that equals
-- schedule_id ONLY while the row is live (deleted_at IS NULL AND schedule_id IS
-- NOT NULL), and NULL otherwise. A UNIQUE index over `live_schedule_id` then:
--   * ignores unbound tasks  (schedule_id NULL  -> live_schedule_id NULL)
--   * ignores soft-deleted tasks (deleted_at set -> live_schedule_id NULL)
--   * enforces at most ONE live, bound task per schedule (the invariant).
-- This requires no change to existing soft-delete semantics and is fully
-- reversible (see Down). MySQL 5.7+ / 8.0 supports STORED generated columns and
-- indexing them.
--
-- NOTE on existing data: if any schedule currently has >1 live bound tasks
-- (the exact dirty state this migration prevents going forward), ADD UNIQUE will
-- fail. The application FOR UPDATE guard (Blocker1a) is the primary fix and is
-- effective immediately on deploy; this index is the secondary backstop. If a
-- deploy hits a duplicate-key error here, operators must first clean the
-- pre-existing double-bound rows (unbind the stale duplicate) and re-run. This
-- is intentional: the failure surfaces latent corruption rather than hiding it.
ALTER TABLE summary_task
    ADD COLUMN live_schedule_id BIGINT
        GENERATED ALWAYS AS (
            CASE WHEN deleted_at IS NULL AND schedule_id IS NOT NULL
                 THEN schedule_id
                 ELSE NULL
            END
        ) STORED;

ALTER TABLE summary_task
    ADD UNIQUE KEY uk_live_schedule_binding (live_schedule_id);

-- +migrate Down
ALTER TABLE summary_task
    DROP INDEX uk_live_schedule_binding;

ALTER TABLE summary_task
    DROP COLUMN live_schedule_id;
