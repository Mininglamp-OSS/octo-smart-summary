-- +migrate Up
-- PR#62 r5 Blocker1b / r7 Blocker1: enforce the one-to-one task<->schedule
-- binding at the DB layer as a backstop to the application-level FOR UPDATE
-- serialization in internal/api/handler/schedule.go.
--
-- live_schedule_id is a STORED generated column equal to schedule_id ONLY while
-- the row is live (deleted_at IS NULL AND schedule_id IS NOT NULL), NULL
-- otherwise. A UNIQUE index over it ignores unbound and soft-deleted tasks and
-- enforces at most ONE live, bound task per schedule.
--
-- r7 Blocker1: self-heal historical double-bound rows BEFORE ADD UNIQUE so the
-- migration never fails on pre-existing dirty data (it previously required
-- manual ops cleanup). For each schedule with >1 live bound task, keep the
-- smallest id (deterministic) and unbind the rest (schedule_id -> NULL).
-- Idempotent and re-runnable. The derived-table wrapper is required: MySQL
-- forbids referencing the UPDATE target table directly in its own subquery.
UPDATE summary_task t
JOIN (
    SELECT schedule_id, MIN(id) AS keep_id
    FROM summary_task
    WHERE deleted_at IS NULL AND schedule_id IS NOT NULL
    GROUP BY schedule_id
    HAVING COUNT(*) > 1
) keep ON keep.schedule_id = t.schedule_id
SET t.schedule_id = NULL
WHERE t.deleted_at IS NULL
  AND t.schedule_id IS NOT NULL
  AND t.id <> keep.keep_id;

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
-- The Up unbind is irreversible (original duplicate bindings are not recorded);
-- noted in the deploy report. Only the index/column are dropped here.
ALTER TABLE summary_task
    DROP INDEX uk_live_schedule_binding;

ALTER TABLE summary_task
    DROP COLUMN live_schedule_id;
