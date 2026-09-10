-- Only execute against the disposable summary_versioning_test database.
INSERT IGNORE INTO summary_task
  (id, task_no, space_id, creator_id, summary_mode, time_range_start, time_range_end, status,
   current_result_id, content_revision)
VALUES
  (101, 'contract-group', 'fixture-space', 'owner', 1, NOW(), NOW(), 3, 207, 7),
  (102, 'contract-personal', 'fixture-space', 'owner', 2, NOW(), NOW(), 3, NULL, 0),
  (103, 'contract-team', 'fixture-space', 'owner', 2, NOW(), NOW(), 3, 301, 2);

INSERT IGNORE INTO summary_participant (id, task_id, user_id, status)
VALUES (301, 102, 'owner', 5), (302, 103, 'member', 5);

INSERT IGNORE INTO summary_result
  (id, task_id, content, version, generated_at, content_revision, team_citations_json, citations_json)
VALUES
  (201, 101, 'group v1', 1, NOW(), 1, '[]', '[]'),
  (202, 101, 'group v2', 2, NOW(), 2, '[]', '[]'),
  (203, 101, 'group v3', 3, NOW(), 3, '[]', '[]'),
  (204, 101, 'group v4', 4, NOW(), 4, '[]', '[]'),
  (205, 101, 'group v5', 5, NOW(), 5, '[]', '[]'),
  (206, 101, 'group v6', 6, NOW(), 6, '[]', '[]'),
  (207, 101, 'group v7', 7, NOW(), 7, '[]', '[]'),
  (301, 103, 'team current [P1]', 2, NOW(), 2,
   '[{"index":1,"user_id":"member","user_name":"Member","personal_result_id":402,"task_id":103}]',
   '[{"index":1,"sender":"Member","content":"private fixture message","sent_at":"","source":"","channel_id":"private","channel_type":1,"message_seq":1}]'),
  (302, 103, 'team historical [P1]', 1, NOW(), 1,
   '[{"index":1,"user_id":"member","user_name":"Historical Member","personal_result_id":402,"task_id":103}]', '[]');

INSERT IGNORE INTO summary_personal_result
  (id, task_id, participant_ref_id, user_id, content, citations_json,
   current_version_id, content_revision, created_at, updated_at)
VALUES
  (401, 102, 301, 'owner', 'old personal report without history', '[]', NULL, 0, NOW(), NOW()),
  (402, 103, 302, 'member', 'private member report', '[]', 201, 1, NOW(), NOW());

-- Same integer ID as summary_result(201), but a different namespace.
INSERT IGNORE INTO summary_personal_result_version
  (id, task_id, participant_ref_id, user_id, content, citations_json, version, generated_at)
VALUES (201, 103, 302, 'member', 'private member report', '[]', 1, NOW());
