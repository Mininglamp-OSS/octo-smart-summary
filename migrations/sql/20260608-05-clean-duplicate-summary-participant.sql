-- +migrate Up
DELETE sp, pr, sc, ss
FROM summary_participant sp
JOIN (
    SELECT task_id, user_id, MIN(id) AS keep_id
    FROM summary_participant
    GROUP BY task_id, user_id
    HAVING COUNT(*) > 1
) keep ON keep.task_id = sp.task_id AND keep.user_id = sp.user_id
LEFT JOIN summary_personal_result pr ON pr.participant_ref_id = sp.id
LEFT JOIN summary_chunk sc ON sc.participant_id = sp.id
LEFT JOIN summary_source ss ON ss.participant_id = sp.id
WHERE sp.id <> keep.keep_id;

-- +migrate Down
SELECT 1;
