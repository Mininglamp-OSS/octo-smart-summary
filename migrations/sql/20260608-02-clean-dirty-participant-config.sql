-- +migrate Up
-- PR#62 r7 Blocker3: normalize historical participant_config to runtime
-- single-person semantics. The API guards (Create/Update/bind/toggle) only stop
-- NEW dirty writes; pre-existing rows whose participant_config contains a
-- non-creator member would still inflate to multi-person once toggled/bound.
--
-- A normal single-person schedule leaves participant_config NULL (CreateSchedule
-- only writes it when participants are supplied), so any element with a non-empty
-- user_id != creator_id is dirty. This matches participantsSubsetOfCreator: empty
-- user_id == creator, so only-creator / empty configs are left untouched.
-- Dirty rows are cleared to NULL (the worker treats len(raw)==0 as {creator}).
-- Idempotent and re-runnable. MySQL 8 JSON functions required.
UPDATE summary_schedule
SET participant_config = NULL
WHERE deleted_at IS NULL
  AND participant_config IS NOT NULL
  AND JSON_VALID(participant_config)
  AND JSON_TYPE(participant_config) = 'ARRAY'
  AND EXISTS (
      SELECT 1
      FROM JSON_TABLE(
          participant_config,
          '$[*]' COLUMNS (uid VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci PATH '$.user_id')
      ) AS jt
      WHERE jt.uid IS NOT NULL
        AND jt.uid <> ''
        AND jt.uid <> summary_schedule.creator_id
  );

-- +migrate Down
-- Irreversible: the original dirty member lists are not recorded before clearing.
-- Noted in the deploy report. No-op down.
SELECT 1;
