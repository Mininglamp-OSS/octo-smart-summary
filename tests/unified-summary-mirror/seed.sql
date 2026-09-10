-- Synthetic fixtures only. Never run against another database.
USE summary_versioning_unified;
CREATE TABLE IF NOT EXISTS `group` (group_no VARCHAR(64), name VARCHAR(64), space_id VARCHAR(64), status INT, updated_at BIGINT, member_count INT DEFAULT 1);
-- `status` is not optional padding: the agent workspace's own membership guard
-- (SOURCE_LOOKUP on /api/v1/agent/chat) filters group_member on it, so a table
-- without the column makes every agent turn fail with a 500 rather than a
-- permission answer.
CREATE TABLE IF NOT EXISTS group_member (group_no VARCHAR(64), uid VARCHAR(64), status INT DEFAULT 1, is_deleted INT);
CREATE TABLE IF NOT EXISTS space (space_id VARCHAR(64), name VARCHAR(64), status INT);
CREATE TABLE IF NOT EXISTS space_member (space_id VARCHAR(64), uid VARCHAR(64), status INT);
CREATE TABLE IF NOT EXISTS conversation_extra (uid VARCHAR(64), channel_id VARCHAR(64), channel_type INT, updated_at BIGINT);
CREATE TABLE IF NOT EXISTS thread (id BIGINT, short_id VARCHAR(64), name VARCHAR(64), group_no VARCHAR(64), status INT, updated_at BIGINT);
CREATE TABLE IF NOT EXISTS thread_member (thread_id BIGINT, uid VARCHAR(64));
CREATE TABLE IF NOT EXISTS `user` (uid VARCHAR(64), name VARCHAR(64), robot INT DEFAULT 0);
CREATE TABLE IF NOT EXISTS robot (robot_id VARCHAR(64));
CREATE TABLE IF NOT EXISTS message (message_seq BIGINT, from_uid VARCHAR(64), channel_id VARCHAR(64), channel_type INT, timestamp BIGINT, payload BLOB, is_deleted INT);

INSERT INTO `group` VALUES ('group1','Project Alpha 发布群','unified-fixture',1,0,3);
INSERT INTO group_member VALUES ('group1','owner',1,0),('group1','dev1',1,0),('group1','pm1',1,0);
INSERT INTO space VALUES ('unified-fixture','Fixture space',1);
INSERT INTO space_member VALUES ('unified-fixture','owner',1);
INSERT INTO `user` (uid,name) VALUES ('owner','周明'),('dev1','陈工'),('pm1','林 PM');

-- A week of real-looking discussion, spread so that BOTH generation windows have
-- material: the manual regenerate window is [now-7d, now], while the scheduled run
-- (whose due slot the smoke forces two days back) covers [now-9d, now-2d]. Each
-- message carries one of the topic keywords provider.mjs recognises, so a window's
-- span visibly changes the report it produces instead of every version reading alike.
INSERT INTO message VALUES
 (1,'dev1','group1',2,UNIX_TIMESTAMP()-8*86400,'{"type":1,"content":"Alpha 灰度批次一放量到 5%,错误率 0.2%,暂时没有告警。"}',0),
 (2,'dev1','group1',2,UNIX_TIMESTAMP()-7*86400,'{"type":1,"content":"支付回调接口联调完成,和结算侧约定用幂等键去重,重复回调不会再产生双笔。"}',0),
 (3,'owner','group1',2,UNIX_TIMESTAMP()-6*86400,'{"type":1,"content":"压测跑到 1200 QPS,P99 延迟 380ms,瓶颈在数据库连接池,已经把 max_open 提到 64。"}',0),
 (4,'dev1','group1',2,UNIX_TIMESTAMP()-5*86400,'{"type":1,"content":"昨晚有一次回滚,原因是配置中心热更新没生效,已经补了启动校验,复现不了了。"}',0),
 (5,'pm1','group1',2,UNIX_TIMESTAMP()-4*86400,'{"type":1,"content":"埋点补齐了下单漏斗的三个节点,数据看板明天能出第一版。"}',0),
 (6,'pm1','group1',2,UNIX_TIMESTAMP()-3*86400,'{"type":1,"content":"对外文档补了错误码表和限流说明,等下周一评审。"}',0),
 (7,'owner','group1',2,UNIX_TIMESTAMP()-3*86400+7200,'{"type":1,"content":"对账还差历史数据迁移,这是本周最大的阻塞,需要 DBA 排期,不然发布要延。"}',0),
 (8,'pm1','group1',2,UNIX_TIMESTAMP()-2*86400-7200,'{"type":1,"content":"确认一下:Alpha 正式发布延到下周二,先把对账迁移做完,大家没异议就这么定。"}',0),
 (9,'dev1','group1',2,UNIX_TIMESTAMP()-2*86400+3600,'{"type":1,"content":"监控告警阈值调整完毕,误报从每天 30 条降到 3 条。"}',0),
 (10,'owner','group1',2,UNIX_TIMESTAMP()-1*86400,'{"type":1,"content":"两个试点客户反馈导出很慢,定位到导出走了全表扫,今天加索引。"}',0),
 (11,'dev1','group1',2,UNIX_TIMESTAMP()-43200,'{"type":1,"content":"灰度放量到 20%,各项指标平稳,没有新的告警。"}',0),
 (12,'owner','group1',2,UNIX_TIMESTAMP()-3600,'{"type":1,"content":"Alpha 发布完成,所有检查项通过,导出慢的问题也一起上了。"}',0);

-- Two agent chat drafts, one per save. space_id/result_type/turn_id stay at their
-- legacy defaults so the save takes the pre-workspace path (the workspace path
-- needs a preview row this fixture does not fabricate). A successful save deletes
-- the session's messages, so each save needs its own session.
--
-- These two read as a from-scratch deliverable and its continue-optimize successor:
-- the derived one keeps the progress section and adds the risks / next steps the new
-- instruction asked for, so the difference between the two summaries is visible in
-- the UI rather than inferred from ids.
INSERT INTO agent_message (space_id,session_id,user_id,turn_id,role,content,tool_calls,created_at)
VALUES ('','sess-scratch','owner',0,'assistant','## Alpha 项目周报\n\n### 本周进展\n\n- 支付回调接口与结算侧联调完成,双方约定以幂等键去重,重复回调不再产生双笔。\n- 压测达到 1200 QPS,P99 延迟 380ms;瓶颈定位在数据库连接池,已将 max_open 调整到 64。\n- 灰度按批次推进:批次一 5% 错误率 0.2%,随后放量到 20%,各项指标平稳。\n- 下单漏斗三个节点的埋点补齐,数据看板第一版即将产出。\n\n### 质量与稳定性\n\n- 出现一次回滚,根因是配置中心热更新未生效,已补启动校验,问题不再复现。\n- 监控告警阈值重新校准,误报由每天 30 条降至 3 条。\n\n### 客户反馈\n\n- 两家试点客户反馈导出偏慢,定位为导出走了全表扫描,已安排加索引。',NULL,NOW());
INSERT INTO agent_message (space_id,session_id,user_id,turn_id,role,content,tool_calls,created_at)
VALUES ('','sess-derived','owner',0,'assistant','## Alpha 项目周报(补充风险与下一步)\n\n### 本周进展\n\n- 支付回调接口与结算侧联调完成,以幂等键去重,重复回调不再产生双笔。\n- 压测达到 1200 QPS,P99 延迟 380ms,连接池 max_open 调整到 64 后瓶颈缓解。\n- 灰度自 5% 放量至 20%,指标平稳,无新增告警。\n- 下单漏斗埋点补齐,数据看板第一版即将产出;对外文档补充错误码表与限流说明,待下周一评审。\n\n### 风险与阻塞\n\n1. **对账历史数据迁移未完成** —— 本周最大阻塞,依赖 DBA 排期;不解决将直接影响发布节奏。\n2. **一次配置回滚暴露的发布前校验缺口** —— 已补启动校验,但同类配置项仍需要一次全量盘查。\n3. **导出性能** —— 试点客户已反馈,索引方案上线后需要复核 P99,避免只是把全表扫换成了大范围扫。\n\n### 决议\n\n- Alpha 正式发布延至下周二,先完成对账迁移。\n\n### 下一步\n\n- 推动 DBA 排期并给出对账迁移的完成时间点。\n- 索引上线后复核导出耗时,并把结果同步到试点客户。\n- 文档评审通过后同步给接入方。',NULL,NOW());
