# 智能总结「改一条定时，多条全变」根因排查报告

日期: 2026-06-05 (UTC)
排查人: 后端工程师 (subagent, 受 Ares 指派)
项目: 后端 `/root/projects/octo-smart-summary`，前端 `/root/projects/octo-web` (包 `packages/dmworksummary`)
数据库: 容器 `octo-deploy-mysql-1`，库 `summary`，表 `summary_task`、`summary_schedule`

**本次仅排查 + 出方案。未改任何代码、未改数据库、未重启/重建容器、未 commit。**
**结论先行：这不是按钮 bug，也不是「不同总结被错误关联到同一 schedule」。这是数据模型语义 (1 schedule 周期模板 : N 执行实例 task) + 详情页 UX 把"周期模板"当成"这条总结的定时"来展示和编辑导致的反直觉行为。下面用代码行号 + 库数据逐条证明。**

---

## 〇、TL;DR（给老板/Ares 拍板）

1. `summary_schedule` = **周期模板**（"每天 9:30 跑一次"这种规则）。`summary_task` = **每次执行产生的一条总结实例**。一个模板被它历史上跑出来的多条 task 引用，**这是设计如此，schema 里 `schedule_id` 本就是非唯一索引 (1:N)**。
2. 老板在某条总结详情页/或定时列表页改的，**改的是那个共享的周期模板行**。模板一改，所有"挂"在这个模板上的总结看起来定时都变了 → 这就是"改一条全变"的来源。**逻辑上数据没被串错，是语义没对齐。**
3. **额外发现一个真 bug（前端→后端协议缺字段）**：后端 `GetSummaryDetail` 根本不返回 `schedule_id`，而前端详情页用 `detail.schedule_id` 判断"是改还是建"。导致**详情页那个"修改定时"按钮其实永远走不到 update 分支，永远是新建**。也就是说：老板真正"改一条全变"的操作，更可能发生在**定时列表页 `ScheduleListPage`**（那里 update 是真生效的），或者是后续有人补了这个字段。两条线都在下面写清。
4. **数据安全**：当前库里**只有 schedule id=12 这一行**有被人手工改过的痕迹（`updated_at` 比上次自动跑晚 ~26 分钟）。其余 schedule(1/2/4) 的 `updated_at` 和最后一次定时执行时间**完全相等**，说明只被 scheduler 动过、没被人工改。**没有发现正在持续恶化的数据问题，无需紧急止血。** 是否需要"恢复 schedule 12 的旧值"取决于老板本意，详见第四节（旧值线索有限，建议确认而非盲目回滚）。

---

## 一、数据模型关系：到底是 1:1 还是 1:N

**结论：设计上就是 `1 summary_schedule : N summary_task`（模板 : 执行实例），不是 1:1。**

证据：

- 模型 `internal/model/model.go:105`：
  ```go
  ScheduleID *int64 `gorm:"column:schedule_id" json:"schedule_id"`
  ```
  task 侧持有指向 schedule 的可空外键，schedule 侧没有反向唯一约束。
- 建表 `migrations/sql/20260101-00-baseline.sql:137,147`：
  ```sql
  `schedule_id` bigint DEFAULT NULL,
  ...
  KEY `idx_schedule_id` (`schedule_id`),   -- 普通索引，非 UNIQUE
  ```
  `schedule_id` 是**非唯一**普通索引，schema 层面明确允许多 task 共享一个 schedule。
- 生成逻辑 `internal/worker/scheduler.go:32 scanPendingSchedules()`：scheduler 每 60s 扫到期模板，**每次到期都新建一条 task 并写入 `ScheduleID: &sched.ID`**（`scheduler.go:88`）。同一个模板跑 N 次 = N 条 task 全部指向同一个 `schedule_id`。这正是 Ares 查到的现象。

---

## 二、为什么会出现"多 task 共享一个 schedule_id" —— 用数据 + 代码区分两种可能

Ares 提出两种可能：(A) scheduler 周期执行的正常实例；(B) 详情页/创建页把不同总结错关联到同一 schedule。**逐条核对后：是 (A) 正常实例，不是 (B) 错关联。**

### 2.1 库数据（已用 utf8mb4 读出真实标题做核对）

`summary_task`（节选关键列；tt=trigger_type，sid=schedule_id）：

| id | 标题(前缀) | tt | sid | created_at |
|----|-----------|----|----|-----------|
| 1  | 语音输入测试用例… | 1(手动) | NULL | 06-02 |
| 2  | 语音输入的测试…   | 1(手动) | NULL | 06-02 |
| 3  | 语音输入的测试…   | 2(定时) | 1 | 06-03 11:00 |
| 4  | 语音输入的测试…   | 2(定时) | 1 | 06-03 17:15 |
| 5  | 语音输入的测试…   | 2(定时) | 1 | 06-03 17:30 |
| 6  | 语音输入的测试…   | 2(定时) | 1 | 06-04 01:30 |
| 7  | 语音输入的测试…   | 2(定时) | 1 | 06-04 10:52 |
| 19 | 语音输入的测试…   | 2(定时) | 1 | 06-04 17:30 |
| 18 | 查看群里没做的事… | 2(定时) | 12 | 06-04 16:28 |
| 20 | 查看群里没做的事… | 2(定时) | 12 | 06-04 18:21 |
| 21 | 群聊内容分析…     | 2(定时) | 2 | 06-04 20:00 |
| 22 | 群聊内容分析…     | 2(定时) | 4 | 06-04 22:30 |
| 8/10/17 | (各种)      | 1(手动) | NULL | — |

`summary_schedule`：

| id | 标题(前缀) | cron/间隔 | last_run_at | updated_at |
|----|-----------|----------|-------------|-----------|
| 1  | 语音输入的测试…   | `30 9 * * *` (legacy cron) | 06-04 17:30:34 | 06-04 17:30:34 |
| 2  | 群聊内容分析…     | `0 12 * * *` | 06-04 20:00:22 | 06-04 20:00:22 |
| 4  | 群聊内容分析…     | `30 14 * * *` | 06-04 22:30:22 | 06-04 22:30:22 |
| 12 | 查看群里没做的事… | interval 1天 @17:00 | 06-04 18:21:34 | **06-04 18:47:50** |
| 13 | 群聊内容分析…     | interval 1天 @11:00 | NULL(没跑过) | 06-05 10:40:02 |

### 2.2 判定为"正常实例"的硬证据

1. **共享同一 sid 的 task 标题，和它们的 schedule 标题完全一致**：
   - sid=1 的 task(3,4,5,6,7,19) 标题全是"语音输入的测试…"，schedule 1 标题也是"语音输入的测试…"。
   - sid=12 的 task(18,20) 标题全是"查看群里没做的事…"，schedule 12 标题同。
   - **没有出现"两个不同总结的 task 指向同一个 schedule"的情况** → 排除可能性 (B) 错关联。
2. **时间对得上**：schedule 1 的 `last_run_at = 06-04 17:30:34`，正好等于 task 19 的 `created_at = 06-04 17:30` ；schedule 12 的 `last_run_at = 06-04 18:21:34` 正好等于 task 20 的 created_at。完全是 scheduler 周期产出的痕迹（`scheduler.go:140-145` 在建完 task 后更新 `last_run_at`/`next_run_at`）。
3. **手动 task 全部 sid=NULL**：手动创建路径 `internal/api/handler/task.go:204-214 CreateSummary()` 构造 `SummaryTask` 时**根本没有设置 `ScheduleID`**（只设 `TriggerType: model.TriggerManual`）。库里 task(1,2,8,10,17) 全是 tt=1 且 sid=NULL，与代码一致。

**小结**：多 task 共享一个 sid = "周期模板跑了很多次"，是**正常设计**，不是脏数据、不是错关联。Ares "多 task 指向同一行 schedule" 的观察正确，但根因不是"创建逻辑把不同总结关到一起"。

---

## 三、真正的根因：详情页 UX 语义 + 一个前后端协议 bug

### 3.1 用户视角的反直觉行为（设计语义层）

详情页打开的是**某一条 task（某一次执行实例）**，但它上面那个"定时更新"编辑的是**整条周期模板 `summary_schedule`**。模板被多条 task 共享，所以：
- 在 task A 的详情页改了模板时间 → task B 详情页再看，定时也变了 → 老板感知为"改一条全变"。

这在数据上是对的（它们本就同一个模板），但 UI 没有告诉用户"你改的是周期规则，会影响这一系列总结"。这是**核心要修的点**。

相关代码：
- 详情页保存 `packages/dmworksummary/src/pages/SummaryDetailPage.tsx:404 handleScheduleSave()`：
  ```ts
  if (scheduleItem) {
      await api.updateSchedule(scheduleItem.schedule_id, {...});   // ← 改的是共享模板行
  } else {
      await api.createSchedule({...});
  }
  ```
- 后端 `UpdateSchedule` (`internal/api/handler/schedule.go:228`) 按 `id = ? AND space_id = ?` 严格更新这一行 schedule，单行隔离没问题——**但它改的就是被 N 个 task 共享的那一行**，所以效果是"全变"。Ares 说"UpdateSchedule 单独看没问题"是对的，bug 不在它。

### 3.2 附带发现的真 bug：后端详情不返回 `schedule_id`，详情页 update 分支实际走不到（高优先级）

- 前端类型 `packages/dmworksummary/src/types/summary.ts:147` 声明了 `schedule_id?: number`。
- 详情页 `SummaryDetailPage.tsx:139` 用它来决定是否加载 schedule：
  ```ts
  if (detail.schedule_id && detail.schedule_id > 0) { this.loadSchedule(detail.schedule_id); }
  ```
- **但后端 `GetSummaryDetail` (`internal/api/handler/task.go:473-518` 的 `resp` map) 根本没有 `schedule_id` 这个字段**（手工核对整段 resp 输出，确认无 schedule_id；列表 `task.go:395` 同样没有）。
- 后果：`detail.schedule_id` 永远 `undefined` → 详情页 `scheduleItem` 永远 null → `openScheduleModal` 永远进"新建"态(`SummaryDetailPage.tsx:391` 的 else) → `handleScheduleSave` 永远走 `createSchedule` 分支。

**这意味着：在当前线上代码下，详情页那个"修改定时"按钮实际是"每次都新建一条 schedule"，并不会去改共享模板。** 那老板"改一条全变"的真实操作入口，**更可能是定时列表页 `ScheduleListPage`**：
- `ScheduleListPage.tsx:89 handleUpdate()` → `:105 api.updateSchedule(editingSchedule.schedule_id, ...)`，这里 update 是真生效的，改的就是共享模板。

> 注意这两件事方向相反、都要处理：
> - 详情页：协议缺字段导致**该改的没改成（反而在乱建 schedule）**；
> - 列表页：改模板**确实会影响所有实例**（语义层问题）。
> 修复时必须一起想清楚，否则只补上 `schedule_id` 字段、让详情页 update 分支生效，会把"改一条全变"从列表页**扩散到详情页**，体验更糟。

---

## 四、是否有数据被老板这次操作"误改"，要不要恢复

**判定方法**：对比每条 schedule 的 `updated_at` 与 `last_run_at`。scheduler 每次跑完会把两者一起写，所以"只被自动跑过"的行，二者相等(差 0 秒)；"被人工改过"的行，`updated_at` 会晚于 `last_run_at`。

查询结果（`TIMESTAMPDIFF(SECOND, last_run_at, updated_at)`）：

| schedule id | 差值(秒) | 判定 |
|-------------|---------|------|
| 1 | 0 | 仅自动跑，**未被人工改** |
| 2 | 0 | 仅自动跑，**未被人工改** |
| 4 | 0 | 仅自动跑，**未被人工改** |
| 12 | **1576 (~26min)** | **有人工编辑痕迹**（`last_run 18:21:34` → `updated 18:47:50`） |
| 13 | NULL(没跑过) | 06-05 10:40 新建，尚未运行 |

**结论：**
- 真正被人手工动过的只有 **schedule id=12**（interval 1天 / run_time 17:00 / next_run 06-05 17:00）。它被 task(18,20) 共享 —— 这与老板"在某条总结详情页/列表里改了 17:00 这条，发现关联的另一条也变了"高度吻合。
- **没有发现 schedule 1/2/4 被这次误操作改动**，它们的定时是历史 cron 配置（legacy `30 9 * * *` 等），不是被这次操作连带改的。所以"所有总结都变了"在数据上其实是**视觉/认知上的"全变"**（多个 task 共享模板，看着像全变），并非真的把每一行 schedule 都改了。
- **是否需要恢复 schedule 12**：库里**没有保存旧值的历史表/审计日志**（无 schedule history 表），无法自动回滚到改之前的精确值。建议**不要盲目回滚**，而是**直接问老板他改 schedule 12 之前想要的是什么时间**，再决定。当前 schedule 12 = 每天 17:00，若这本就是老板想设的，则无需动。
- **没有正在持续恶化的数据问题**（之前 `scheduler.go:50-57` 已有"脏数据禁用"保护，避免坏 schedule 每 60s 反复刷 task 烧钱），无需紧急止血。

---

## 五、推荐修复方案（含优先级 / 改动面 / 是否迁移 / 前后端）

### 方案 A（推荐，P0）：详情页"改定时"= 改这一条总结自己的周期，不再复用/影响别的

把"周期模板"做成**每条总结配置一份独立的 schedule**，详情页改定时只动自己那份。

- **产品语义**：用户在某条总结详情页改定时 = 改这条总结今后的周期，不波及其它总结。最符合老板直觉。
- **后端改动**：
  1. `GetSummaryDetail`(`task.go` resp) **补返回 `schedule_id`**（修第 3.2 的协议 bug，P0 必做）。
  2. 让"一条总结"对应"一份独立 schedule"。两种落地：
     - (A1) 详情页保存定时时，**若该 task 当前的 schedule 被多个 task 共享，则不 update 共享行，而是 clone 一份新 schedule 给这条总结用**（写新 schedule + 回填本 task/后续实例的 schedule_id）。改动相对集中在 `handleScheduleSave` 对应的后端语义。
     - (A2) 更彻底：引入"总结配置(summary config)"概念，task 实例都挂在 config 上，schedule 与 config 1:1。改动面大，建议二期。
- **前端改动**：`SummaryDetailPage.tsx` 依赖后端返回的 `schedule_id` 正确回显；`handleScheduleSave` 的 update 语义改为"改本总结的周期"。
- **是否迁移**：A1 基本不需要 schema 迁移（schedule 仍 1:N，但每条逻辑总结自己一份）；需要一次性数据梳理（可选）把现有共享 schedule 拆分，但**非必须**、可不动历史。
- **优先级**：P0（其中"补 schedule_id 字段"必须先做，否则详情页按钮行为是错的）。

### 方案 B（P1，最小改动）：明确告诉用户"这是周期任务统一设置"

保留 schedule = 全局周期模板的现状，但在 UI 讲清楚。

- **前端**：详情页 + 列表页编辑定时的弹窗加文案："此设置为该周期总结的统一规则，修改会影响该系列后续所有自动总结"。详情页那个按钮文案/入口也对齐成"周期设置"。
- **后端**：仍需补 `GetSummaryDetail` 的 `schedule_id`（否则详情页连"它属于哪个周期"都判断不了）。
- **是否迁移**：不需要。
- **优先级**：P1。改动小、上线快，但没解决"想单独改一条"的诉求，属于"先把认知对齐、别让人误会"的止血型方案。

### 方案 C（必做的纯 bug 修复，P0，独立于 A/B）：修详情页 update 永远走不到的协议缺口

不管选 A 还是 B，**都要先修第 3.2**：

- 后端 `GetSummaryDetail`（以及列表 `ListSummaries` 如有需要）在响应里加入 `"schedule_id": task.ScheduleID`。
- 同时**复核详情页"改一条会不会扩散"**：补上字段后，详情页 update 分支会真正生效；若此时仍直接 `updateSchedule(共享行)`，就会把"改一条全变"从列表页扩散到详情页。**所以方案 C 必须和方案 A(独立 schedule) 或方案 B(明确文案) 配套**，不能单独只补字段就上线。
- **改动面**：后端 1~2 处 map 加字段；前端无需改即可生效回显。**无迁移。**

> 建议组合：**C + A1**（P0 修协议 + 详情页改定时不复用共享模板）。若工期紧，先上 **C + B**（补字段 + 明确文案）止血，二期再做 A1。

---

## 六、附：本次执行的关键查询（可复现，只读）

```sql
-- 共享情况
SELECT schedule_id, COUNT(*), GROUP_CONCAT(id ORDER BY id)
FROM summary_task WHERE schedule_id IS NOT NULL GROUP BY schedule_id;
-- => 1:(3,4,5,6,7,19)  2:(21)  4:(22)  12:(18,20)

-- 人工编辑痕迹判定
SELECT id, cron_expr, interval_days, run_time,
       TIMESTAMPDIFF(SECOND, last_run_at, updated_at) AS upd_minus_lastrun
FROM summary_schedule ORDER BY id;
-- => 仅 id=12 差 1576s(被人改过)，1/2/4 差 0(仅自动跑)，13 为新建
```

读库命令（root 容器内，已加 `--default-character-set=utf8mb4` 才能正确显示中文标题）：
```
docker exec octo-deploy-mysql-1 mysql -uroot -ptsdd123456 --default-character-set=utf8mb4 summary -e "<SQL>"
```

---

## 七、给 Ares/老板的拍板清单

1. 确认产品语义：**"详情页改定时"是想"只改这一条总结"(选方案 A)还是"改整个周期规则"(选方案 B)**？
2. P0 必做：后端 `GetSummaryDetail` 补 `schedule_id` 字段（方案 C），且必须与 A 或 B 配套，避免把"改一条全变"扩散到详情页。
3. schedule id=12 是否需要恢复旧定时？库里无审计旧值，**建议直接确认老板本意**，而非盲回滚。
4. 现有共享 schedule 的历史 task 是否需要拆分/清理？建议**不动历史**，新逻辑只对今后生效（除非老板要求）。
