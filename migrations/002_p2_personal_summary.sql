-- P2: Personal summary support for by-person mode

-- New table: summary_personal_result
CREATE TABLE summary_personal_result (
    id                  BIGINT AUTO_INCREMENT PRIMARY KEY,
    task_id             BIGINT NOT NULL,
    participant_ref_id  BIGINT NOT NULL COMMENT '关联 summary_participant.id',
    user_id             VARCHAR(64) NOT NULL COMMENT '冗余 DMWork uid，方便按用户查询',
    content             MEDIUMTEXT NOT NULL,
    msg_count           INT NOT NULL DEFAULT 0,
    total_token_used    INT NOT NULL DEFAULT 0,
    model_version       VARCHAR(50) NOT NULL DEFAULT '',
    worker_status       TINYINT NOT NULL DEFAULT 0
                        COMMENT '0=PENDING 1=PROCESSING 2=COMPLETED 3=FAILED',
    error_message       VARCHAR(500),
    submitted_at        DATETIME(6) COMMENT 'NULL=草稿/未生成完 NOT NULL=已提交对外可见',
    generated_at        DATETIME(6),
    created_at          DATETIME(6) NOT NULL,
    updated_at          DATETIME(6) NOT NULL,
    UNIQUE KEY uk_task_participant (task_id, participant_ref_id),
    KEY idx_task_id (task_id),
    KEY idx_user_id (task_id, user_id),
    KEY idx_submitted (task_id, submitted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

-- Extend summary_participant table
ALTER TABLE summary_participant
    MODIFY COLUMN status TINYINT NOT NULL DEFAULT 0
        COMMENT '0=pending(待响应) 1=accepted(已同意) 2=declined(已拒绝) 3=processing(生成中) 4=completed(已生成) 5=submitted(已提交对外可见)',
    ADD COLUMN personal_result_id BIGINT AFTER confirmed_at,
    ADD COLUMN worker_started_at  DATETIME(6) AFTER personal_result_id;
