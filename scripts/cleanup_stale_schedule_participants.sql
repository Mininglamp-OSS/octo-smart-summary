-- =============================================================================
-- cleanup_stale_schedule_participants.sql
--
-- One-time cleanup of EXISTING stale rows left behind by the old
-- "leave/remove does nothing under a schedule" bug.
--
-- ROOT CAUSE (already fixed in CODE for NEW leaves/removes):
--   A schedule (summary_schedule) spawns many rounds of summary_task
--   (first round trigger_type=1 + each scheduled round trigger_type=2). Each
--   member historically got one summary_participant / one
--   summary_personal_result row PER ROUND (per task_id). The old
--   leave/removeMember handlers only deleted the CLICKED round's rows, so the
--   member lingered in the OTHER rounds of the same schedule. The fixed code
--   (Leave / RemoveMember) now (a) strips the user from the schedule's
--   participant_config via stripScheduleParticipant, AND (b) deletes the user's
--   participant / personal_result rows across ALL rounds of that schedule.
--
-- --------------------------------------------------------------------------- --
-- NEW, CORRECT "stale" DEFINITION (roster-authoritative)
-- --------------------------------------------------------------------------- --
--   The single source of truth for "who is currently a member of a schedule" is
--   the schedule's participant_config (summary_schedule.participant_config).
--   stripScheduleParticipant removes a user from that JSON the moment they
--   leave / are removed. Therefore:
--
--     A residual (participant / personal_result) row is STALE iff
--       * it belongs to a task whose schedule_id IS NOT NULL, AND
--       * its user_id is NOT the schedule's creator_id, AND
--       * its user_id is NOT present in that schedule's participant_config
--         roster (the authoritative current member list).
--
--   We DO NOT use the old "present in some rounds but missing from others"
--   (partial-presence) heuristic. That heuristic WRONGLY deletes legitimate
--   LATE JOINERS: e.g. member_b who was not in round 1 but was added by the
--   creator later and so only appears in rounds 2/3. A late joiner is STILL in
--   participant_config, so the roster-authoritative test correctly KEEPS them,
--   while the old partial-presence test would have deleted them. This is the
--   precise defect the reviewer flagged, and the fix is to judge membership by
--   the authoritative roster, never by per-round presence patterns.
--
--   participant_config JSON shapes handled (verified against the live DB and
--   internal/model ParseScheduleParticipantConfig):
--     * V5 object form:   {"participants":[{"user_id":"..."},...],"confirm_gate_passed":bool}
--     * legacy obj array: [{"user_id":"...","user_name":"..."}, ...]
--     * legacy str array: ["u_a","u_b", ...]
--   The roster expression COALESCEs these into one JSON array of user_id strings.
--
-- --------------------------------------------------------------------------- --
-- SAFETY GUARDS
-- --------------------------------------------------------------------------- --
--   * Creator is ALWAYS excluded (creator_id); the creator cannot leave/be
--     removed, so they are never a "leave residual".
--   * Scoped per (schedule_id, space_id); never crosses schedules/spaces.
--   * Ignores soft-deleted task rounds (summary_task.deleted_at IS NULL).
--   * EMPTY/NULL-roster guard: a schedule is only eligible for cleanup when it
--     has a parseable, NON-EMPTY participant_config roster (JSON_LENGTH > 0).
--     This prevents mass-deletion on legacy schedules whose participant_config
--     was simply never populated (NULL) — without this guard, an empty roster
--     would flag every non-creator participant as stale.
--   * ONE shared victim set (the `victim` derived table below) drives BOTH the
--     participant delete and the personal_result delete, so there is no 2a/2b
--     ordering dependency and the two deletes can never disagree.
--   * Two stages: run STAGE 1 (read-only preview), eyeball it, THEN run STAGE 2.
--   * STAGE 2 is wrapped in a transaction; COMMIT is commented out — verify row
--     counts first, then COMMIT (or ROLLBACK).
--   * This script does NOT recompute team summaries. After cleanup, affected
--     schedules' latest rounds may need a meta_summary recompute, done OUT OF
--     BAND.
--
-- DO NOT RUN AUTOMATICALLY. Orchestrator must review and execute manually.
-- MySQL 8.0 syntax (JSON_EXTRACT / JSON_CONTAINS / JSON_QUOTE / CTE).
-- =============================================================================


-- =============================================================================
-- STAGE 1: READ-ONLY PREVIEW. Shows exactly what STAGE 2 would delete.
--          Run these SELECTs first and review the output.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- 1a) Per (schedule_id, space_id): non-creator user_ids that are OUTSIDE the
--     authoritative participant_config roster yet still have residual rows,
--     with their residual task_ids and a user name for human verification.
-- -----------------------------------------------------------------------------
WITH schedule_roster AS (
    -- Authoritative current roster of user_ids per schedule, normalized across
    -- all three historical participant_config JSON shapes.
    SELECT
        s.id         AS schedule_id,
        s.creator_id AS creator_id,
        COALESCE(
            JSON_EXTRACT(s.participant_config, '$.participants[*].user_id'), -- V5 object form
            JSON_EXTRACT(s.participant_config, '$[*].user_id'),             -- legacy array of objects
            CASE WHEN JSON_TYPE(s.participant_config) = 'ARRAY'             -- legacy array of strings
                 THEN s.participant_config END,
            JSON_ARRAY()                                                    -- NULL / unparseable -> empty
        ) AS roster_ids
    FROM summary_schedule s
),
victim AS (
    -- The ONE authoritative victim set: (schedule_id, space_id, user_id) tuples
    -- of non-creator users who are NOT in the schedule's roster but still have a
    -- residual participant row in some non-deleted round of that schedule.
    SELECT DISTINCT
        t.schedule_id,
        t.space_id,
        sp.user_id
    FROM summary_participant sp
    JOIN summary_task t
        ON t.id = sp.task_id
       AND t.deleted_at IS NULL
       AND t.schedule_id IS NOT NULL
    JOIN schedule_roster r
        ON r.schedule_id = t.schedule_id
    WHERE JSON_LENGTH(r.roster_ids) > 0                 -- non-empty roster guard
      AND sp.user_id <> r.creator_id                    -- never the creator
      AND NOT JSON_CONTAINS(r.roster_ids, JSON_QUOTE(sp.user_id))  -- outside roster
)
SELECT
    v.schedule_id,
    v.space_id,
    v.user_id,
    MAX(sp.user_name)                                AS user_name,
    GROUP_CONCAT(DISTINCT sp.task_id ORDER BY sp.task_id) AS residual_task_ids
FROM victim v
JOIN summary_task t
    ON t.schedule_id = v.schedule_id
   AND t.space_id    = v.space_id
   AND t.deleted_at IS NULL
JOIN summary_participant sp
    ON sp.task_id = t.id
   AND sp.user_id = v.user_id
GROUP BY v.schedule_id, v.space_id, v.user_id
ORDER BY v.schedule_id, v.user_id;


-- -----------------------------------------------------------------------------
-- 1b) Exact summary_participant rows STAGE 2 will delete (one row per leftover).
-- -----------------------------------------------------------------------------
WITH schedule_roster AS (
    SELECT
        s.id         AS schedule_id,
        s.creator_id AS creator_id,
        COALESCE(
            JSON_EXTRACT(s.participant_config, '$.participants[*].user_id'),
            JSON_EXTRACT(s.participant_config, '$[*].user_id'),
            CASE WHEN JSON_TYPE(s.participant_config) = 'ARRAY'
                 THEN s.participant_config END,
            JSON_ARRAY()
        ) AS roster_ids
    FROM summary_schedule s
),
victim AS (
    SELECT DISTINCT t.schedule_id, t.space_id, sp.user_id
    FROM summary_participant sp
    JOIN summary_task t
        ON t.id = sp.task_id
       AND t.deleted_at IS NULL
       AND t.schedule_id IS NOT NULL
    JOIN schedule_roster r
        ON r.schedule_id = t.schedule_id
    WHERE JSON_LENGTH(r.roster_ids) > 0
      AND sp.user_id <> r.creator_id
      AND NOT JSON_CONTAINS(r.roster_ids, JSON_QUOTE(sp.user_id))
)
SELECT sp.id AS participant_id, sp.task_id, t.schedule_id, t.space_id,
       sp.user_id, sp.user_name
FROM summary_participant sp
JOIN summary_task t
    ON t.id = sp.task_id
   AND t.deleted_at IS NULL
   AND t.schedule_id IS NOT NULL
JOIN victim v
    ON v.schedule_id = t.schedule_id
   AND v.space_id    = t.space_id
   AND v.user_id     = sp.user_id
ORDER BY t.schedule_id, sp.user_id, sp.task_id;


-- -----------------------------------------------------------------------------
-- 1c) Exact summary_personal_result rows STAGE 2 will delete (same victim set).
-- -----------------------------------------------------------------------------
WITH schedule_roster AS (
    SELECT
        s.id         AS schedule_id,
        s.creator_id AS creator_id,
        COALESCE(
            JSON_EXTRACT(s.participant_config, '$.participants[*].user_id'),
            JSON_EXTRACT(s.participant_config, '$[*].user_id'),
            CASE WHEN JSON_TYPE(s.participant_config) = 'ARRAY'
                 THEN s.participant_config END,
            JSON_ARRAY()
        ) AS roster_ids
    FROM summary_schedule s
),
victim AS (
    SELECT DISTINCT t.schedule_id, t.space_id, sp.user_id
    FROM summary_participant sp
    JOIN summary_task t
        ON t.id = sp.task_id
       AND t.deleted_at IS NULL
       AND t.schedule_id IS NOT NULL
    JOIN schedule_roster r
        ON r.schedule_id = t.schedule_id
    WHERE JSON_LENGTH(r.roster_ids) > 0
      AND sp.user_id <> r.creator_id
      AND NOT JSON_CONTAINS(r.roster_ids, JSON_QUOTE(sp.user_id))
)
SELECT pr.id AS personal_result_id, pr.task_id, t.schedule_id, t.space_id,
       pr.user_id
FROM summary_personal_result pr
JOIN summary_task t
    ON t.id = pr.task_id
   AND t.deleted_at IS NULL
   AND t.schedule_id IS NOT NULL
JOIN victim v
    ON v.schedule_id = t.schedule_id
   AND v.space_id    = t.space_id
   AND v.user_id     = pr.user_id
ORDER BY t.schedule_id, pr.user_id, pr.task_id;


-- =============================================================================
-- STAGE 2: DELETE (mutating). Run ONLY after reviewing STAGE 1 output.
--          Both deletes JOIN the SAME victim set, so they are order-independent
--          and cannot diverge. Transaction-wrapped; COMMIT is commented out.
--
-- NOTE on MySQL: a multi-table DELETE cannot read the table being deleted from
-- inside its own subquery, so the victim set is materialized via a derived
-- table (a SELECT over participant + roster) that is JOINed to the delete
-- target. This is safe because the derived table is evaluated before the delete.
-- =============================================================================

-- START TRANSACTION;

-- -----------------------------------------------------------------------------
-- 2a) Delete leftover summary_participant rows for victim users.
-- -----------------------------------------------------------------------------
DELETE sp
FROM summary_participant sp
JOIN summary_task t
    ON t.id = sp.task_id
   AND t.deleted_at IS NULL
   AND t.schedule_id IS NOT NULL
JOIN (
    -- victim set (materialized derived table; identical predicate to STAGE 1)
    SELECT DISTINCT t2.schedule_id, t2.space_id, sp2.user_id
    FROM summary_participant sp2
    JOIN summary_task t2
        ON t2.id = sp2.task_id
       AND t2.deleted_at IS NULL
       AND t2.schedule_id IS NOT NULL
    JOIN (
        SELECT
            s.id         AS schedule_id,
            s.creator_id AS creator_id,
            COALESCE(
                JSON_EXTRACT(s.participant_config, '$.participants[*].user_id'),
                JSON_EXTRACT(s.participant_config, '$[*].user_id'),
                CASE WHEN JSON_TYPE(s.participant_config) = 'ARRAY'
                     THEN s.participant_config END,
                JSON_ARRAY()
            ) AS roster_ids
        FROM summary_schedule s
    ) r
        ON r.schedule_id = t2.schedule_id
    WHERE JSON_LENGTH(r.roster_ids) > 0
      AND sp2.user_id <> r.creator_id
      AND NOT JSON_CONTAINS(r.roster_ids, JSON_QUOTE(sp2.user_id))
) victim
    ON victim.schedule_id = t.schedule_id
   AND victim.space_id    = t.space_id
   AND victim.user_id     = sp.user_id;

-- -----------------------------------------------------------------------------
-- 2b) Delete the corresponding summary_personal_result rows for the SAME victims.
--     (Same materialized victim derived table; no dependency on 2a having run.)
-- -----------------------------------------------------------------------------
DELETE pr
FROM summary_personal_result pr
JOIN summary_task t
    ON t.id = pr.task_id
   AND t.deleted_at IS NULL
   AND t.schedule_id IS NOT NULL
JOIN (
    SELECT DISTINCT t2.schedule_id, t2.space_id, sp2.user_id
    FROM summary_participant sp2
    JOIN summary_task t2
        ON t2.id = sp2.task_id
       AND t2.deleted_at IS NULL
       AND t2.schedule_id IS NOT NULL
    JOIN (
        SELECT
            s.id         AS schedule_id,
            s.creator_id AS creator_id,
            COALESCE(
                JSON_EXTRACT(s.participant_config, '$.participants[*].user_id'),
                JSON_EXTRACT(s.participant_config, '$[*].user_id'),
                CASE WHEN JSON_TYPE(s.participant_config) = 'ARRAY'
                     THEN s.participant_config END,
                JSON_ARRAY()
            ) AS roster_ids
        FROM summary_schedule s
    ) r
        ON r.schedule_id = t2.schedule_id
    WHERE JSON_LENGTH(r.roster_ids) > 0
      AND sp2.user_id <> r.creator_id
      AND NOT JSON_CONTAINS(r.roster_ids, JSON_QUOTE(sp2.user_id))
) victim
    ON victim.schedule_id = t.schedule_id
   AND victim.space_id    = t.space_id
   AND victim.user_id     = pr.user_id;

-- Verify the row counts above match STAGE 1 (1b / 1c), then:
-- COMMIT;
-- (or ROLLBACK; if anything looks off)

-- =============================================================================
-- POST-CLEANUP: affected schedules' latest rounds may need a meta_summary
-- recompute (team summary). Do that OUT OF BAND, not from this script.
-- =============================================================================
