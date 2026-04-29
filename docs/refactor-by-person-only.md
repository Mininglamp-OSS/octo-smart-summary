# 重构方案：移除 BY_GROUP，仅保留 BY_PERSON

## 一、现状分析

### 1.1 模式常量定义

**文件**: `internal/model/model.go:74-77`

```go
const (
    ModeByGroup  = 1
    ModeByPerson = 2
)
```

当前系统支持两种模式，但 API 层 (`handler/task.go:147`) 已经硬编码为 `ModeByPerson`：

```go
summaryMode := model.ModeByPerson
```

`ModeByGroup` 仅在以下位置仍被引用或隐式依赖。

### 1.2 BY_GROUP 代码路径（待删除）

| 文件 | 行号 | 描述 |
|------|------|------|
| `internal/model/model.go` | 75 | `ModeByGroup = 1` 常量定义 |
| `internal/worker/processor.go` | 209-222 | `processTask()` 中 `else` 分支：BY_GROUP 完成后直接设为 `StatusCompleted` |
| `internal/worker/processor.go` | 270-276 | `executePipeline()` 中 `else` 分支：调用 `pipeline.ResolveAndFetchMessages()` (非 Personal 版本) |
| `internal/worker/processor.go` | 296-409 | `executePipeline()` 中 BY_GROUP 的 Map/Reduce 全流程（Map 分片、Reduce 合并、buildCitations、保存 SummaryResult） |
| `internal/pipeline/fetch.go` | 558-601 | `ResolveAndFetchMessages()` 函数——仅供 BY_GROUP 使用的消息获取管线 |
| `internal/api/handler/schedule.go` | 67 | 定时任务默认 `summaryMode := 1`（即 ModeByGroup） |
| `internal/api/handler/schedule.go` | 210-212 | 更新定时任务时允许设置任意 `SummaryMode` |
| `internal/service/llm.go` | 210-230 | `CallReduce()` 函数——BY_GROUP 模式的 Reduce 阶段 LLM 调用 |

### 1.3 BY_PERSON 代码路径（保留）

| 文件 | 行号 | 描述 |
|------|------|------|
| `internal/model/model.go` | 76 | `ModeByPerson = 2` 常量定义 |
| `internal/model/personal_result.go` | 全文 | PersonalResult 模型、状态常量 |
| `internal/worker/processor.go` | 168-207 | `processTask()` 中 BY_PERSON 分支（单人/多人判断） |
| `internal/worker/processor.go` | 251-301 | `executePipeline()` 中 BY_PERSON 的消息获取与 Map/Reduce 跳过 |
| `internal/worker/personal_processor.go` | 全文 | 个人总结处理器（获取消息、过滤、Map/Reduce、保存 PersonalResult） |
| `internal/worker/meta_processor.go` | 全文 | 团队总结处理器（合并多人结果、生成 TeamCitation） |
| `internal/api/handler/task.go` | 147,192-288 | 创建任务时的 BY_PERSON 逻辑 |
| `internal/api/handler/task.go` | 483-539 | `GetSummary()` 中 BY_PERSON 特有的 personal_result/members 数据 |
| `internal/api/handler/personal.go` | 全文 | Accept/Decline/Submit/GetPersonal/GetMembers 端点 |
| `internal/pipeline/fetch.go` | 488-556 | `ResolveAndFetchMessagesForPersonal()` 函数 |
| `internal/pipeline/fetch.go` | 294-329 | `IntersectParticipantChannels()` (Layer 1.5) |
| `internal/pipeline/fetch.go` | 333-382 | `FilterByMutualActivity()` (Layer 4.5) |
| `internal/service/llm.go` | 232-254 | `CallReduceByPerson()` — 多人 Reduce |

---

## 二、修改清单

### 2.1 model 层

#### `internal/model/model.go`

| 行号 | 改动 | 说明 |
|------|------|------|
| 74-77 | 删除 `ModeByGroup = 1`，将 `ModeByPerson` 值改为 `1` | 统一为唯一模式值；或保留 `ModeByPerson = 2` 不变（数据库兼容），删除 `ModeByGroup` 常量即可 |

**建议**: 保留 `ModeByPerson = 2` 不改值，避免已有数据不兼容。仅删除 `ModeByGroup` 常量。

### 2.2 worker 层

#### `internal/worker/processor.go`

| 行号 | 改动 | 说明 |
|------|------|------|
| 167-222 | **重写 `processTask()` 成功分支** | 移除 `if task.SummaryMode == model.ModeByPerson` 判断和 `else` (BY_GROUP) 分支，仅保留 BY_PERSON 逻辑 |
| 168 | 删除 `if task.SummaryMode == model.ModeByPerson` 条件 | 现在所有任务都是 BY_PERSON |
| 209-222 | **删除整个 `else` 块** | BY_GROUP 的完成逻辑不再需要 |
| 225-410 | **重写 `executePipeline()`** | 仅保留 BY_PERSON 的消息获取路径 (251-301)；删除 BY_GROUP 的 Map/Reduce 流程 (304-409) |
| 251 | 删除 `if task.SummaryMode == model.ModeByPerson` 条件 | 直接调用 `ResolveAndFetchMessagesForPersonal` |
| 270-276 | **删除 `else` 块** | 不再需要 `pipeline.ResolveAndFetchMessages()` |
| 296-409 | **删除 BY_GROUP Map/Reduce 全部代码** | 包括 Map 分片循环 (335-378)、Reduce 合并 (380-409)、buildCitations 等 |

**重写后的 `processTask()` 成功分支逻辑**:
```
// 成功后 — 所有任务都是 BY_PERSON
participantCount = 查询参与者数量
if participantCount <= 1:
    单人模式: 直接开始处理, 触发 processPersonalSummary (无 WaitingConfirm)
else:
    多人模式: 其他参与者保持 WaitingConfirm (参与者级别状态), 只触发 Creator 的 processPersonalSummary
    // Task 状态不设为 WaitingConfirm，跟着 Creator 走: Pending → Processing → Completed
```

**重写后的 `executePipeline()` 逻辑**:
```
1. 加载 sources
2. 加载 participants
3. 调用 ResolveAndFetchMessagesForPersonal() 获取消息
4. 如果无消息，创建空结果
5. 记录消息数量到 task，返回 nil (个人总结由 personal_processor 处理)
```

#### `internal/worker/personal_processor.go`

**单人模式改动**:

| 行号 | 改动 | 说明 |
|------|------|------|
| 87-88 | **条件化 TriggerMetaSummary** | 单人模式下不再触发 meta_summary，个人总结完成即为最终结果 |
| 60-75 | **单人模式直接完成任务** | 个人总结完成后，如果只有1个参与者，直接设 task.Status = Completed，将 PersonalResult 内容复制到 SummaryResult |

具体改动：

```
// 在 processPersonalSummary() 的 "Mark completed" 之后 (line 76 后)，新增逻辑:

// 查询参与者总数
var participantCount int64
p.db.Model(&model.SummaryParticipant{}).Where("task_id = ?", taskID).Count(&participantCount)

if participantCount <= 1 {
    // 单人模式：直接完成，不走 meta_summary
    // 1. 将 PersonalResult 内容直接创建为 SummaryResult
    // 2. 设置 task.Status = StatusCompleted
    // 3. 发送 TASK_COMPLETED 回调
} else {
    // 多人模式：触发 meta_summary 检查
    p.meta.TriggerMetaSummary(taskID)
}
```

#### `internal/worker/meta_processor.go`

| 行号 | 改动 | 说明 |
|------|------|------|
| 无需删除 | 保留但仅用于多人模式 | meta_processor 仍负责合并多人结果为团队总结 |

**注意**: 单人模式不再经过 meta_processor，meta_processor 只为多人模式服务。

#### `internal/worker/scheduler.go`

| 行号 | 改动 | 说明 |
|------|------|------|
| 61-72 | 创建的任务强制 `SummaryMode = ModeByPerson` | 忽略 schedule 表中的旧 mode 值 |
| 66 | 改为 `SummaryMode: model.ModeByPerson,` | 不再使用 `sched.SummaryMode` |
| 112-120 | **重写 `scanConfirmTimeouts()`** | 不再扫描 `task.status = StatusWaitingConfirm`（Task 不再进入该状态），改为扫描仍有参与者处于 WaitingConfirm 且超过 `confirm_deadline` 的任务，自动取消未确认的参与者 |

### 2.3 API 层

#### `internal/api/handler/task.go`

| 行号 | 改动 | 说明 |
|------|------|------|
| 95-104 | 删除 `createSummaryReq.SummaryMode` 字段 | 不再接受前端传入 mode 参数 |
| 147 | 保留 `summaryMode := model.ModeByPerson` | 已经是硬编码，无需改 |
| 190-196 | 简化：直接设置初始状态 | 移除 `if summaryMode == model.ModeByPerson` 判断，因为永远为 true |
| 229 | 同上，移除 `if summaryMode == model.ModeByPerson` | 直接执行 participant 创建逻辑 |
| 282 | 同上，移除 `if summaryMode == model.ModeByPerson` | 直接触发 worker |
| 326-329 | **ListSummaries: 删除 `summary_mode` 过滤参数** | 不再需要按模式筛选 |
| 392 | 列表返回中可移除 `summary_mode` 字段或固定为 2 | 前端不再需要区分模式 |
| 469 | GetSummary 响应中 `summary_mode` 固定为 2 | 或直接移除 |
| 483 | 移除 `if task.SummaryMode == model.ModeByPerson` 判断 | 所有任务都需要返回 personal_result 和 members |

#### `internal/api/handler/schedule.go`

| 行号 | 改动 | 说明 |
|------|------|------|
| 29 | 删除 `createScheduleReq.SummaryMode` 字段 | 不再接受前端传入 |
| 38 | 删除 `updateScheduleReq.SummaryMode` 字段 | 不再接受前端传入 |
| 67-69 | 强制 `summaryMode := model.ModeByPerson` | 删除从请求读取的逻辑 |
| 90 | `SummaryMode: model.ModeByPerson,` | 固定值 |
| 124 | 列表返回可保留 `summary_mode` 但值固定 | 向后兼容 |
| 210-212 | **删除 `SummaryMode` 更新逻辑** | 不允许更新 mode |

#### `internal/api/handler/personal.go`

无需改动。所有端点（Accept/Decline/Submit/GetPersonal/GetMembers）已经是纯 BY_PERSON 逻辑。

### 2.4 pipeline 层

#### `internal/pipeline/fetch.go`

| 行号 | 改动 | 说明 |
|------|------|------|
| 558-601 | **删除 `ResolveAndFetchMessages()` 函数** | 仅供 BY_GROUP 使用，不再需要 |
| 384-486 | 可选：保留 `FilterMessagesByRelevance()`，它同时被两种模式使用 | 如果 BY_GROUP 的调用方已删除，检查是否还有其他调用方 |

**注意**: `ResolveAndFetchMessages()` 只在 `processor.go:271` 的 BY_GROUP 路径中被调用。删除该函数后应无编译错误。

### 2.5 service 层

#### `internal/service/llm.go`

| 行号 | 改动 | 说明 |
|------|------|------|
| 148-175 | `reduceSystemPrompt` — 检查是否仍被使用 | `CallReduce()` 同时被 personal_processor 调用 (personal_processor.go:203)，**不能删除** |
| 210-230 | `CallReduce()` — **保留** | personal_processor 的 Map/Reduce 仍需要此函数 |
| 232-254 | `CallReduceByPerson()` — **保留** | meta_processor 的多人合并仍需要此函数 |

`CallReduce()` 被 personal_processor (line 203) 和 processor.go BY_GROUP 路径 (line 381) 同时使用。删除 BY_GROUP 路径后，`CallReduce()` 仍被 personal_processor 使用，**必须保留**。

---

## 三、单人模式完整流程

> 单人模式 = BY_PERSON + 只有创建者一人参与
>
> **核心原则**: 单人就一个人，不需要等谁确认，创建后直接开始处理。

### 3.1 创建阶段

1. **前端** `POST /api/v1/summaries`
   - 不传 `participants`，或仅传自己
   - Handler 自动补全: `req.Participants = [{UserID: effectiveUID}]` (`task.go:177-179`)

2. **Handler 事务** (`task.go:213-274`):
   - 创建 `SummaryTask` (status=`Pending`)
   - 创建 `SummarySource` 记录
   - 创建 `SummaryParticipant` (status=`Processing`，单人无需确认，直接开始)
   - 创建 `PersonalResult` (worker_status=`PersonalStatusPending`)
   - 关联 `participant.personal_result_id = pr.ID`

3. **触发 Worker** (`task.go:282-288`):
   - `go triggerWorker({Type: "personal_summary", TaskID, ParticipantRefID})`
   - 单人模式下任务初始状态为 `Pending`，直接进入处理

### 3.2 处理阶段（重构后）

通过 trigger channel (`personal_summary`) 直接触发 `processPersonalSummary()`。

**`processPersonalSummary()` 执行** (`personal_processor.go:13-91`):
1. 加载 participant 和 PersonalResult
2. 设置 worker_status=`Processing`, participant.status=`Processing`
3. 设置 task.Status = `Processing`
4. `executePersonalPipeline()`:
   - 获取消息 (ResolveAndFetchMessagesForPersonal)
   - 过滤上下文窗口 (FilterWithContext)
   - 解析发送者姓名
   - Map 阶段（分片+LLM摘要）
   - Reduce 阶段（合并分片）
   - 构建 citations
5. 保存 PersonalResult (worker_status=`Completed`)
6. 设置 participant.status=`Completed`
7. 发送 WS 通知 `PERSONAL_SUMMARY_STATUS`

**单人直接完成**:

8. **检测到只有1个参与者**
9. **将 PersonalResult 内容复制到 SummaryResult**:
    ```
    SummaryResult{
        TaskID:         taskID,
        Content:        pr.Content,
        CitationsJSON:  pr.CitationsJSON,
        TotalMsgCount:  pr.MsgCount,
        TotalTokenUsed: totalTokens,
        ModelVersion:   modelVer,
        Version:        1,
        GeneratedAt:    now,
    }
    ```
10. **设置 task.Status = `StatusCompleted`**
11. **发送 `TASK_COMPLETED` 回调** (Progress=100)

### 3.3 单人模式状态流转

```
Task:           Pending → Processing → Completed
Participant:    Processing → Completed
PersonalResult: Pending → Processing → Completed
SummaryResult:  (auto created from PersonalResult)
```

**关键点**:
- **没有 WaitingConfirm** — 单人无需等任何人确认
- **没有 Accepted** — 不存在"接受"动作，创建即开始
- **没有 Submitted** — 没有 Submit 动作，个人总结完成后直接创建 SummaryResult，任务结束
- 不经过 meta_processor，个人总结即为最终总结

---

## 四、多人模式完整流程

> 多人模式 = BY_PERSON + 创建者 + 其他参与者
>
> **核心原则**: Creator 直接开始处理，不需要确认。WaitingConfirm 是参与者级别的状态，不是任务级别。Task 状态跟着 Creator 走，其他参与者不确认只影响自己，不影响 Creator 和任务整体进度。

### 4.1 创建阶段

1. **前端** `POST /api/v1/summaries`
   - `participants: [{user_id: "A", user_name: "Alice"}, {user_id: "B", user_name: "Bob"}]`

2. **Handler 事务** (`task.go:213-274`):
   - 创建 `SummaryTask` (status=`Pending`, confirm_deadline=now+24h)
     - **Task 状态跟着 Creator 走**，不受其他参与者影响
   - 创建 `SummarySource` 记录
   - **Creator** → `SummaryParticipant` (status=`Pending`，即将开始处理，无需确认)
   - **Creator** → `PersonalResult` (worker_status=`PersonalStatusPending`)
   - **其他参与者** → `SummaryParticipant` (status=`WaitingConfirm`，等待确认)

3. **触发 Creator 的 Worker** (`task.go:282-288`):
   - `go triggerWorker({Type: "personal_summary", TaskID, ParticipantRefID: creatorP.ID})`
   - Creator 无需等待，立即开始个人总结

### 4.2 Creator 处理（直接开始，无需确认）

4. `processPersonalSummary()` 对 Creator 执行（同单人模式步骤 1-7）
   - Creator 从 `Pending` 进入 `Processing`，task.Status 同步变为 `Processing`
   - Creator 不经过 WaitingConfirm，直接开始处理
5. Creator 的 PersonalResult 完成 (worker_status=`Completed`)
6. **触发 `TriggerMetaSummary()`** — 但此时其他参与者尚未提交
7. MetaProcessor 检查: `submitted < totalAccepted` → 不执行，等待

### 4.3 其他参与者确认（只有非 Creator 需要确认）

8. **参与者 B** 调用 `POST /api/v1/summaries/:id/accept`
   - `personal.go:42-113`
   - 状态从 `WaitingConfirm` → `Pending`
   - 创建 PersonalResult
   - 触发 `personal_summary` worker

9. **参与者 B** 的 `processPersonalSummary()` 执行
   - 同上，生成个人总结
   - 触发 `TriggerMetaSummary()`

10. **参与者 C** 可以调用 `POST /api/v1/summaries/:id/decline` 拒绝
    - 状态从 `WaitingConfirm` → `Declined`
    - 不创建 PersonalResult
    - 不影响 Creator 和任务整体进度，只影响参与者自己

### 4.4 用户提交

11. **创建者** 查看个人总结 `GET /api/v1/summaries/:id/personal`
12. **创建者** 提交 `POST /api/v1/summaries/:id/submit`
    - `personal.go:202-253`
    - 设置 `submitted_at`, participant.status=`Submitted`
    - 广播 `MEMBER_SUBMITTED` WebSocket 事件
    - 触发 `meta_summary` worker

13. **参与者 B** 同样提交个人总结

### 4.5 团队总结生成

14. `MetaProcessor.processMetaSummary()` (`meta_processor.go:69-213`):
    - 检查: submitted count >= totalAccepted → 条件满足
    - **如果只有2人提交**: 调用 `CallReduceByPerson()` 合并
    - **如果只有1人提交** (其他人拒绝): 直接复制内容
    - 保存 `SummaryResult` (含 TeamCitations)
    - 设置 task.Status=`Completed`
    - 广播 `META_SUMMARY_UPDATED` WebSocket 事件

### 4.6 多人模式状态流转

```
Task（跟着 Creator 走，不受其他参与者影响）:
  Pending ─(Creator开始处理)→ Processing ─(meta_summary完成)→ Completed

Creator Participant:
  Pending → Processing → Completed → (手动submit) Submitted

Other Participant:
  WaitingConfirm ─(accept)→ Pending → Processing → Completed → (手动submit) Submitted
  WaitingConfirm ─(decline)→ Declined

Creator PersonalResult:
  Pending → Processing → Completed

Other PersonalResult (确认后才创建):
  Pending → Processing → Completed

SummaryResult:
  (MetaProcessor 创建, 合并所有 PersonalResult)
```

### 4.7 超时机制

- **确认超时**: `scheduler.go:112-119` — `scanConfirmTimeouts()` 每60秒扫描，超过 `confirm_deadline` 且仍有参与者处于 `WaitingConfirm` 状态的任务，自动取消未确认的参与者
- **处理超时**: `scheduler.go:124-152` — `scanStuckTasks()` 重置超时任务，超过最大重试次数则失败
- **个人总结超时**: `scheduler.go:157-205` — `scanStuckPersonalTasks()` 重置卡在 Processing 的个人任务 (>10min)，重新触发 worker

---

## 五、数据库变更

### 5.1 不需要 DDL 变更

现有表结构无需修改。`summary_task.summary_mode` 列保留，新创建的记录固定写入 `2`。

### 5.2 数据迁移（可选）

**处理历史数据中的 BY_GROUP 任务**:

```sql
-- 将历史 BY_GROUP 任务标记为已归档（可选）
UPDATE summary_task SET summary_mode = 2 WHERE summary_mode = 1;
```

**定时任务表**:

```sql
-- 将定时任务配置中的 mode 统一为 BY_PERSON
UPDATE summary_schedule SET summary_mode = 2 WHERE summary_mode = 1;
```

**注意**: 由于旧的 BY_GROUP 任务不含 `SummaryParticipant` 和 `PersonalResult` 记录，这些历史任务在详情页面将显示为空参与者列表。建议在前端做兼容处理（参与者列表为空时不显示成员区域）。

### 5.3 `summary_schedule` 表

`summary_mode` 字段保留，但代码层面强制为 `ModeByPerson`。新创建的定时任务配置需要包含 `participant_config` 字段来定义参与者。

---

## 六、API 变更

### 6.1 创建总结 `POST /api/v1/summaries`

| 字段 | 变更 | 说明 |
|------|------|------|
| `summary_mode` | **废弃/忽略** | 不再接受，后端固定为 BY_PERSON |

### 6.2 列表查询 `GET /api/v1/summaries`

| 参数 | 变更 | 说明 |
|------|------|------|
| `summary_mode` 过滤 | **移除** | 不再需要按模式筛选 |

### 6.3 详情查询 `GET /api/v1/summaries/:id`

| 字段 | 变更 | 说明 |
|------|------|------|
| `summary_mode` | 固定返回 `2` | 向后兼容 |
| `personal_result` | **所有任务都返回** | 不再有条件判断 |
| `members` | **所有任务都返回** | 不再有条件判断 |
| `result.submitted_count` | **所有任务都返回** | 不再有条件判断 |

### 6.4 创建定时任务 `POST /api/v1/summary-schedules`

| 字段 | 变更 | 说明 |
|------|------|------|
| `summary_mode` | **废弃/忽略** | 后端强制 BY_PERSON |
| `participants` | **必填** | 定时任务必须指定参与者 |

### 6.5 更新定时任务 `PUT /api/v1/summary-schedules/:id`

| 字段 | 变更 | 说明 |
|------|------|------|
| `summary_mode` | **忽略** | 不允许修改 mode |

### 6.6 无变更的 API

以下 API 无需改动：
- `POST /api/v1/summaries/:id/accept`
- `POST /api/v1/summaries/:id/decline`
- `POST /api/v1/summaries/:id/respond`
- `POST /api/v1/summaries/:id/submit`
- `GET /api/v1/summaries/:id/personal`
- `GET /api/v1/summaries/:id/members`
- `GET /api/v1/summaries/:id/result`
- `POST /api/v1/summaries/:id/regenerate`
- `DELETE /api/v1/summaries/:id`
- `POST /api/v1/summaries/:id/cancel`
- WebSocket `/ws/summaries`

---

## 七、前端影响

### 7.1 必须改动

| 改动项 | 说明 |
|--------|------|
| **移除模式选择 UI** | 创建总结时不再显示 BY_GROUP / BY_PERSON 切换 |
| **移除 `summary_mode` 参数** | 创建总结和定时任务请求中删除此字段 |
| **移除模式筛选** | 列表页面删除按模式筛选的下拉框/tab |
| **单人模式: 隐藏 Submit 按钮** | 当参与者只有自己时，个人总结完成后自动完成，不需要手动提交 |
| **单人模式: 隐藏成员面板** | 只有自己时不需要显示成员列表 |

### 7.2 需要兼容

| 兼容项 | 说明 |
|--------|------|
| **历史 BY_GROUP 任务** | `summary_mode=1` 的旧任务没有 participants/personal_result，前端需判断 `participants` 为空时不渲染成员区域 |
| **`summary_mode` 响应字段** | 后端继续返回 `summary_mode: 2`，前端可继续读取但不做分支逻辑 |

### 7.3 建议改动

| 建议项 | 说明 |
|--------|------|
| **单人模式流程简化** | 创建 → 处理中 → 自动完成。没有"等待确认"状态 |
| **多人模式进度展示** | 显示 "已提交 X/Y 人" 进度条 |
| **WebSocket 事件处理** | 确保处理 `PERSONAL_SUMMARY_STATUS` (单人完成通知) 和 `META_SUMMARY_UPDATED` (多人汇总完成通知) |

---

## 八、实施步骤建议

### Phase 1: 后端 — 删除 BY_GROUP 代码路径

1. 删除 `ModeByGroup` 常量
2. 重写 `processor.go` 的 `processTask()` 和 `executePipeline()`
3. 删除 `pipeline/fetch.go` 中的 `ResolveAndFetchMessages()` 函数
4. 修改 `scheduler.go` 强制 `SummaryMode = ModeByPerson`
5. 修改 `schedule.go` handler 忽略 `summary_mode` 参数
6. 编译验证，修复所有引用错误

### Phase 2: 后端 — 实现单人模式直接完成

1. 修改 `personal_processor.go` — 单人模式下跳过 meta_summary，直接创建 SummaryResult 并完成任务
2. 修改 `task.go` 创建逻辑 — 移除模式条件判断
3. 修改 `task.go` GetSummary — 移除模式条件判断，所有任务都返回 personal/members 数据

### Phase 3: 前端配合

1. 移除模式选择 UI
2. 简化单人模式流程展示
3. 处理历史 BY_GROUP 任务兼容

### Phase 4: 数据清理（可选）

1. 更新历史数据 `summary_mode`
2. 更新定时任务配置

---

## 九、风险与注意事项

1. **定时任务兼容**: 现有定时任务如果 `summary_mode=1` 且没有 `participant_config`，在 `scanPendingSchedules` 触发时将创建没有参与者的 BY_PERSON 任务。需要在 scheduler 中增加校验：如果没有 participant_config，自动将创建者加为唯一参与者。

2. **Regenerate 操作**: `task.go:573-612` Regenerate 将任务重置为 `StatusPending`，processor poll 会捡起来执行。需确保重构后的 `executePipeline()` 正确处理重新生成场景。

3. **`CallReduce` 不能删**: 它同时被 `personal_processor.go:203` 使用。`reduceSystemPrompt` 也不能删。

4. **`CallMap` 的 prompt**: `mapSystemPrompt` 中包含 "★标记的是目标用户的消息" 相关说明——这是 BY_PERSON 特有的，删除 BY_GROUP 后无影响，保留即可。

5. **`FilterMessagesByRelevance()`**: 目前此函数未被 processor 直接调用（grep 确认），但作为 pipeline 的公共工具可保留。
