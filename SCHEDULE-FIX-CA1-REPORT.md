# SCHEDULE-FIX-CA1-REPORT — 详情页改定时只动自己（方案 C + A1 实现报告）

执行人：全栈工程师（subagent）
执行时间：2026-06-05 UTC
依据：`SCHEDULE-SHARED-BUG-REPORT.md` 第 153–186 行 方案 C + A1
状态：**代码改完、本地编译/vet/单测/前端测试/tsc 全过，未 commit、未重建镜像、未动数据库**，等 Ares 核验上线。

---

## 0. 结论速览

| 项 | 结果 |
|---|---|
| 方案 C（协议补 `schedule_id`） | ✅ 已实现，在 task 详情接口 `GetSummary` 响应补 `schedule_id` |
| 方案 A1（详情页改定时=只改本条；共享则 clone） | ✅ 已实现，`UpdateSchedule` 加 `scope=task` 语义 + 事务内 COUNT 判断 + clone+回填 |
| 前端 scope 标志 | ✅ `SummaryDetailPage.handleScheduleSave` 带 `scope:'task', task_id`，依赖后端返回 `schedule_id` 回显 |
| 不破坏 ScheduleListPage | ✅ 列表页/useSchedule 不带 scope，仍走原"直接改模板"in-place update |
| 后端 `go build ./... && go vet ./internal/...` | ✅ BUILD_OK / VET_OK |
| 后端已有单测 | ✅ handler + service 全过 |
| A1 新增单测（共享clone / 独占update / 列表页in-place） | ✅ 3 个全过 |
| 前端 `npx vitest run` | ✅ 51 passed (6 files) |
| 前端 `npx tsc --noEmit` 不新增错误 | ✅ git stash 对比：改前/改后均 4378（delta=0，详见 §5） |

---

## 1. 改了哪些文件

### 后端 `/root/projects/octo-smart-summary`
- `internal/api/handler/task.go`（GetSummary，约 472–486 行附近）：在 `resp` map 之后补 `schedule_id` 字段。【方案 C】
- `internal/api/handler/schedule.go`：
  - 顶部 import 增加 `gorm.io/gorm/clause`。
  - `updateScheduleReq`（约 41–62 行）：新增 `Scope string` 和 `TaskID *int64`。【A1 入参】
  - `UpdateSchedule`（约 232 行起）：原先末尾的 `h.db.Model(&sched).Updates(updates)` 直写，改为 **事务内 scope 判断 + 共享则 clone + 回填 task.schedule_id**；返回的 `schedule_id` 改为"生效后的 schedule id（clone 时是新 id）"。
  - 新增辅助函数 `applyScheduleUpdates(s *model.SummarySchedule, updates map)`：把 update 的字段差量套到 clone 结构体上（含 `next_run_at`）。
- `internal/api/handler/schedule_clone_test.go`（**新增**）：A1 三个单测。

### 前端 `/root/projects/octo-web/packages/dmworksummary`
- `src/types/summary.ts`（`UpdateScheduleParams`，约 229–243 行）：新增可选 `scope?: 'task'` 与 `task_id?: number`。
- `src/pages/SummaryDetailPage.tsx`（`handleScheduleSave`，约 404–435 行）：update 分支带 `scope:'task', task_id: detail.task_id`，并用返回的 `updated.schedule_id` 作为回填依据 `loadSchedule(effectiveScheduleId)`。

> `SummaryDetail` 类型早已有 `task_id` 和 `schedule_id?`（types/summary.ts 135/147 行），前端无需额外加字段；`updateSchedule` 已返回 `ScheduleItem`（含 `schedule_id`）。

---

## 2. 方案 C 怎么实现的（协议补字段）

真 bug：详情接口（实际函数名是 `GetSummary`，路由 `GET /api/v1/summaries/:id`，**不是** GetSummaryDetail）原本响应里没有 `schedule_id`，前端 `detail.schedule_id` 恒空，导致详情页 update/create 分支判断永远错。

修复（task.go GetSummary）：
```go
// Plan C: expose the task's associated schedule_id
if task.ScheduleID != nil {
    resp["schedule_id"] = *task.ScheduleID
} else {
    resp["schedule_id"] = nil
}
```
- `task.ScheduleID` 是 `*int64`，可空时返回 `null`。
- 前端 `SummaryDetailPage` 已用 `detail.schedule_id`（loadSchedule、hasSchedule 判断），补字段后即可正确回显，无需改前端回显逻辑。

---

## 3. 方案 A1 怎么实现的（详情页改定时=只改本条；共享则 clone）

核心放在后端 `UpdateSchedule`，用前端带的 `scope` 标志区分调用来源（采用任务书指定的"前端带 scope 标志"清晰方案）：

语义：
- **详情页**：前端带 `scope:'task'` + `task_id`。后端在**一个事务**里：
  1. 校验该 task 属于本 space 且当前 `schedule_id == 本 schedule`；
  2. `SELECT COUNT(*) FROM summary_task WHERE schedule_id = ? AND deleted_at IS NULL`（带 `FOR UPDATE` 行锁，防并发串）；
  3. 若 **COUNT > 1（共享）**：**clone** 一份新 schedule（结构体复制原行 → 清空 id/created_at/updated_at/deleted_at/last_run_at → `applyScheduleUpdates` 套上本次 run_time/interval/day_of_week/day_of_month/next_run_at 等）→ `Create` → 把**这条 task** 的 `schedule_id` 回填为 clone.id。其它共享 task 与原 schedule **完全不动**。
  4. 若 **COUNT == 1（独占）**：直接 in-place `Updates(updates)`，不 clone。
- **列表页 / useSchedule**：不带 scope → 走 `!cloned` 分支 → in-place update 原 schedule（管理周期模板，符合预期，行为不变）。

关键点对照任务书要求：
- ✅ "是否共享"判断与 clone+回填在**同一事务**（`h.db.Transaction`），COUNT 查询带 `clause.Locking{Strength:"UPDATE"}` 行锁防并发。
- ✅ clone 带正确 `space_id`（结构体复制原 sched.SpaceID）、`is_active`（复制原值）、`next_run_at`（**复用现有 `service.NextRunInitial`** 算出，写在 `updates["next_run_at"]`，再由 applyScheduleUpdates 套到 clone；未自己重算时区/对齐，沿用现有时区与对齐逻辑）。
- ✅ `space_id` / 权限校验保持原逻辑：函数开头仍校验 `sched.SpaceID==spaceID`、`sched.CreatorID==userID`；clone 路径里又校验 task 属本 space + 当前确实指向该 schedule。
- ✅ 返回体 `schedule_id` 改为"生效 id"：clone 时返回新 id，让前端回填到 clone；独占/列表页返回原 id。

返回结构（与原契约一致字段名）：
```json
{ "code":0, "data": { "schedule_id": <生效id>, "next_run_at": "RFC3339|null" } }
```

---

## 4. 前端 scope 标志怎么传

`SummaryDetailPage.handleScheduleSave`（详情页保存定时）update 分支：
```ts
const updated = await api.updateSchedule(scheduleItem.schedule_id, {
    cron_expr, interval_days, interval_months, day_of_week, day_of_month, run_time,
    scope: 'task',
    task_id: detail.task_id,
});
const effectiveScheduleId = updated?.schedule_id ?? scheduleItem.schedule_id;
this.loadSchedule(effectiveScheduleId);   // clone 时自动切到新 schedule 回显
```
- create 分支（详情页第一次设定时、本就独立）保持原样（CreateSchedule 本来就建新行，天然只属于这条，无共享问题）。
- 列表页 `ScheduleListPage.tsx:105` 与 `hooks/useSchedule.ts:54` 的 `updateSchedule` **不带 scope** → 后端走原 in-place 更新，列表页"直接改模板"行为不变。

---

## 5. 编译 / vet / 单测 / 前端测试 / tsc 结果

### 后端（docker golang:1.25，离线用 gocache 卷做 mod 缓存）
- `go build ./...` → **BUILD_OK**
- `go vet ./internal/...` → **VET_OK**
- `go test ./internal/api/handler/... ./internal/service/...` → 全 **ok**（handler 0.089s / service 7.029s）
- A1 新增单测 `-run TestUpdateSchedule -v`：
  - `TestUpdateSchedule_SharedClonesForTaskScope` PASS —— 共享(2 task)时 scope=task：生成新 schedule、原 schedule run_time 不变(17:00)、clone=09:30、taskA 指向 clone、**taskB 仍指向原 schedule**、clone 带 space/active/next_run_at。
  - `TestUpdateSchedule_SoleOwnerUpdatesInPlace` PASS —— 独占(1 task)时 scope=task：in-place 更新、不产生 clone（schedule 总数仍=1）。
  - `TestUpdateSchedule_ListPageSharedUpdatesInPlace` PASS —— 不带 scope（列表页）即使共享也 in-place 改原行、不 clone（验证未破坏列表页行为）。

> 注：构建环境无外网，使用宿主机已存在的 `gocache` docker 卷（含 go module download 缓存）挂到 `/go`，`GOPROXY=off GOFLAGS=-mod=mod CGO_ENABLED=1`（sqlite 测试需 CGO）。未改 go.mod/go.sum。

### 前端（/root/projects/octo-web/packages/dmworksummary）
- `npx vitest run` → **51 passed (6 files)**。
- `npx tsc --noEmit`：
  - 改后总 error TS 数 = **4378**；`git stash` 掉本次两处前端改动后 = **4378**。**delta = 0，未新增任何错误**。
  - 说明：在该 package 单独跑 tsc 不会走 monorepo 的 project references，React 等类型解析失败导致基线本身偏高（远超报告里提到的 ~335，那是 monorepo 整体 typecheck 的基线）。这里按任务书要求用 **git stash 对比法**确认"不新增错误"，对比结果完全一致（0 新增），结论可靠。

---

## 6. SQL 验证思路（模拟共享后调详情页保存，确认只 clone 给当前 task）

> 只读演示，不在本次执行。线上核验可用以下思路（与单测 TestUpdateSchedule_SharedClonesForTaskScope 等价）。

前置（构造共享，仅演示，**勿在生产执行写操作**）：
```sql
-- 假设 schedule id=S 被 taskA、taskB 共享
SELECT id, schedule_id FROM summary_task WHERE schedule_id = S;   -- 期望返回 taskA, taskB
SELECT id, run_time, space_id, is_active, next_run_at FROM summary_schedule WHERE id = S;  -- 记下原值 R0
```

操作（详情页对 taskA 改定时）：
```
PUT /api/v1/summary-schedules/S
Headers: Token=<creator>, X-Space-Id=<space>
Body: { "scope":"task", "task_id": <taskA>, "run_time":"09:30", "interval_days":1 }
-- 期望响应 data.schedule_id = S' (新 clone id, ≠ S)
```

校验（保存后）：
```sql
-- 1) 原 schedule 未变（只有 taskB 还指它）
SELECT id, run_time FROM summary_schedule WHERE id = S;            -- run_time 仍为 R0 的旧值
SELECT id, schedule_id FROM summary_task WHERE id = <taskB>;       -- schedule_id 仍 = S（未被波及）

-- 2) 生成了新 clone，且只有 taskA 指它
SELECT id, run_time, space_id, is_active, next_run_at
  FROM summary_schedule WHERE id = S';                             -- run_time=09:30, space/is_active 同原, next_run_at 已按 09:30 算
SELECT id, schedule_id FROM summary_task WHERE id = <taskA>;       -- schedule_id = S'

-- 3) 共享计数：S 现在只剩 1 个引用，S' 也只 1 个
SELECT schedule_id, COUNT(*) FROM summary_task
  WHERE schedule_id IN (S, S') AND deleted_at IS NULL GROUP BY schedule_id; -- S:1, S':1
```
结论判定：原 schedule run_time 不变 + taskB.schedule_id 不变 + 新 clone 仅 taskA 指向 ⇒ "改一条只动自己、不波及别的" 成立。

对照"独占"场景：若 S 只被 taskA 引用，则上述 PUT 后 `data.schedule_id == S`，`summary_schedule` 不新增行，S.run_time 直接变 09:30（in-place）。

---

## 7. 遗留点 / 注意事项

1. **存量数据未拆分**（与报告 A1 一致，非必须）：现有已共享的 schedule（如报告里 1:(3,4,5,6,7,19)、12:(18,20)）不会自动拆。本方案是"下次有人在详情页改某条共享总结时，才把那条 clone 出去"——即**惰性拆分**，符合 A1"不需 schema 迁移、可不动历史"。如老板要一次性彻底拆分，需另写一次性数据脚本（二期，可选）。
2. **回填范围 = 仅当前 task 一条**：A1 回填的是 `task_id` 指定的这一条 task。`summary_task` 是"每次执行实例"，未来该总结的新实例由 scheduler 按 schedule 生成——clone 后这条 task 指向新 schedule，其后续实例自然继承新 schedule（沿用现有 scheduler 逻辑，本次未改 scheduler）。如果产品语义是"某逻辑总结的所有历史实例都要跟着改"，当前只改了被点的那条 task；但按报告语义"改这条总结今后的周期"，惰性单条回填即可。**这点请 Ares/老板确认是否符合预期**。
3. **create 分支**未加 scope（详情页首次建定时本就建独立新行，无共享问题），无需改。
4. **tsc 基线偏高**纯属 package 单独跑的环境问题（React 类型解析），delta=0 已证明无新增错误；如需"~335 基线"口径，应在 monorepo 根用统一 typecheck 命令跑（本次按任务书 git-stash 对比法执行）。
5. **未 commit、未重建镜像、未重启容器、未改库数据、未发任何聊天消息**——全部满足硬约束。改动等 Ares 核验后统一上线。

---

## 8. 关键代码片段（备查）

`UpdateSchedule` 事务核心：
```go
resultScheduleID := sched.ID
var resultNextRunAt *time.Time
txErr := h.db.Transaction(func(tx *gorm.DB) error {
    cloned := false
    if req.Scope == "task" && req.TaskID != nil {
        var task model.SummaryTask
        if err := tx.Where("id = ? AND space_id = ? AND deleted_at IS NULL", *req.TaskID, spaceID).First(&task).Error; err == nil &&
            task.ScheduleID != nil && *task.ScheduleID == sched.ID {
            var shareCount int64
            if err := tx.Model(&model.SummaryTask{}).
                Clauses(clause.Locking{Strength: "UPDATE"}).
                Where("schedule_id = ? AND deleted_at IS NULL", sched.ID).
                Count(&shareCount).Error; err != nil { return err }
            if shareCount > 1 {
                clone := sched
                clone.ID = 0; clone.CreatedAt = time.Time{}; clone.UpdatedAt = time.Time{}
                clone.DeletedAt = nil; clone.LastRunAt = nil
                applyScheduleUpdates(&clone, updates)
                if err := tx.Create(&clone).Error; err != nil { return err }
                if err := tx.Model(&model.SummaryTask{}).Where("id = ?", task.ID).
                    Update("schedule_id", clone.ID).Error; err != nil { return err }
                resultScheduleID = clone.ID; resultNextRunAt = clone.NextRunAt; cloned = true
            }
        }
    }
    if !cloned {
        if err := tx.Model(&model.SummarySchedule{}).Where("id = ?", sched.ID).
            Updates(updates).Error; err != nil { return err }
        if nr, ok := updates["next_run_at"].(time.Time); ok { resultNextRunAt = &nr } else { resultNextRunAt = sched.NextRunAt }
    }
    return nil
})
```
