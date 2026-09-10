-- Synthetic fixtures only. Never run against another database.
USE summary_versioning_execution;
CREATE TABLE IF NOT EXISTS `group` (group_no VARCHAR(64), name VARCHAR(64), space_id VARCHAR(64), status INT, updated_at BIGINT, member_count INT DEFAULT 1);
CREATE TABLE IF NOT EXISTS group_member (group_no VARCHAR(64), uid VARCHAR(64), is_deleted INT);
CREATE TABLE IF NOT EXISTS space_member (space_id VARCHAR(64), uid VARCHAR(64), status INT);
CREATE TABLE IF NOT EXISTS conversation_extra (uid VARCHAR(64), channel_id VARCHAR(64), channel_type INT, updated_at BIGINT);
CREATE TABLE IF NOT EXISTS thread (id BIGINT, short_id VARCHAR(64), name VARCHAR(64), group_no VARCHAR(64), status INT, updated_at BIGINT);
CREATE TABLE IF NOT EXISTS thread_member (thread_id BIGINT, uid VARCHAR(64));
CREATE TABLE IF NOT EXISTS `user` (uid VARCHAR(64), name VARCHAR(64), robot INT DEFAULT 0);
CREATE TABLE IF NOT EXISTS robot (robot_id VARCHAR(64));
CREATE TABLE IF NOT EXISTS message (message_seq BIGINT, from_uid VARCHAR(64), channel_id VARCHAR(64), channel_type INT, timestamp BIGINT, payload BLOB, is_deleted INT);
INSERT INTO `group` VALUES ('group1','Project Alpha','execution-fixture',1,0,1);
INSERT INTO group_member VALUES ('group1','owner',0);
INSERT INTO space_member VALUES ('execution-fixture','owner',1);
INSERT INTO `user` (uid,name) VALUES ('owner','Fixture author');
INSERT INTO message VALUES (1,'owner','group1',2,UNIX_TIMESTAMP()-3600,'{"type":1,"content":"Alpha release shipped successfully with all checks."}',0);

INSERT INTO summary_task (id,task_no,title,space_id,creator_id,summary_mode,time_range_start,time_range_end,status)
VALUES (110,'execution-pilot','Unified summary execution fixture','execution-fixture','owner',2,DATE_SUB(NOW(),INTERVAL 7 DAY),NOW(),3);
INSERT INTO summary_participant (id,task_id,user_id,status) VALUES (110,110,'owner',5);
INSERT INTO summary_personal_result (id,task_id,participant_ref_id,user_id,content,citations_json,worker_status,created_at,updated_at)
VALUES (110,110,110,'owner','Previous summary remains available until generation succeeds.','[]',2,NOW(),NOW());
INSERT INTO summary_source (task_id,source_type,source_id,derived) VALUES (110,1,'group1',0);
