# BY_PERSON 模式三个问题修复设计文档

> 版本：v1.0 | 日期：2026-04-27

---

## 一、问题概述

| # | 问题 | 根因 |
|---|------|------|
| 问题1 | 同意邀请后结果显示"该时段内无你的文本消息" | `personal_processor` 只统计 SenderUID == userID 的消息，未考虑用户可能是被提及对象而非发送者 |
| 问题2 | 任意一人点击"提交给所有人"后任务立刻变成"已完成" | `meta_processor` 没有检查是否所有参与者都已提交 |
| 问题3 | 全部提交后只能看到"别人已提交"，看不到别人的报告内容 | 前端只渲染当前用户自己的 PersonalResult，未渲染其他参与者的报告 |

---

## 二、问题1：同意后显示"该时段内无你的文本消息"

### 根因分析

`internal/worker/personal_processor.go`：

```go
// 当前逻辑：只筛选"由该用户发送的消息"
var userMessages []pipeline.Message
for _, msg := range allMessages {
    if msg.SenderUID == userID {
        userMessages = append(userMessages, msg)
    }
}
if len(userMessages) == 0 {
    return "该时段内无你的文本消息", 0, 0, nil
}
```

**核心语义问题**：BY_PERSON 的用意是"总结该用户在这些频道中的参与情况"。
大棍子没有发过消息，但可能：
- 被别人 @
- 被别人提名讨论
- 是私聊的接收方

这些都是"与该用户相关的内容"，应该纳入总结。

### 修复方案

扩大消息收集范围，不只收集"发送者是该用户"的消息，还收集"内容涉及该用户"的消息：

```go
// 收集规则（任一满足即保留）
func collectRelevantMessages(allMessages []Message, userID, userName string) []Message {
    var relevant []Message
    for _, msg := range allMessages {
        // 规则1: 该用户发送的消息
        if msg.SenderUID == userID {
            relevant = append(relevant, msg)
            continue
        }
        // 规则2: 消息中 @该用户（UID 或名字）
        if strings.Contains(msg.Content, "@"+userName) ||
            strings.Contains(msg.Content, userID) {
            relevant = append(relevant, msg)
            continue
        }
        // 规则3: 私聊消息（该用户是接收方）
        if msg.ChannelType == 1 { // DM
            relevant = append(relevant, msg)
            continue
        }
    }
    return relevant
}
```

**Prompt 同步修改**：

当前 personal Map prompt 强调"你的发言"，需改为"与你相关的内容"：

```
原：请提取该用户在这段时间内的主要发言和工作内容
改：请提取该用户在这段时间内的参与情况，包括：
   - 该用户的发言
   - 别人@该用户的内容
   - 涉及该用户名字的讨论
   - 与该用户的私聊内容
   如果该用户完全没有参与，返回空字符串即可
```

**兜底处理**：

如果收集完后仍为空（用户确实没有任何相关消息），不返回硬编码错误字符串，而是返回空结果，由前端展示友好提示。

---

## 三、问题2：一人提交后任务立刻完成

### 根因分析

`internal/handler/personal.go`（Submit 接口）：

```go
// 任意一人提交，立刻触发 meta_summary
go h.triggerWorker(model.WorkerTriggerRequest{
    Type:   "meta_summary",
    TaskID: taskID,
})
```

`internal/worker/meta_processor.go`：

```go
// meta 完成后无条件把任务标记为 Completed
db.Model(&model.SummaryTask{}).Where("id = ?", taskID).
    Update("status", model.StatusCompleted)
```

**缺失的检查**：提交前应确认所有 `accepted` 的参与者都已提交。

### 修复方案

**方案 A（推荐）：Submit 接口中判断**

```go
// personal.go — Submit handler
func (h *PersonalHandler) Submit(c *gin.Context) {
    // ... 保存当前用户的 personal result ...

    // 检查是否所有 accepted 的参与者都已提交
    var acceptedCount, submittedCount int64
    db.Model(&model.SummaryParticipant{}).
        Where("task_id = ? AND status = ?", taskID, model.ParticipantAccepted).
        Count(&acceptedCount)

    db.Model(&model.SummaryParticipant{}).
        Where("task_id = ? AND submitted_at IS NOT NULL", taskID).
        Count(&submittedCount)

    if submittedCount >= acceptedCount {
        // 所有人都提交了，触发 meta
        go h.triggerWorker(model.WorkerTriggerRequest{
            Type:   "meta_summary",
            TaskID: taskID,
        })
    }
    // 否则静默等待
}
```

**`summary_participants` 表加 `submitted_at` 字段**（migration）：

```sql
ALTER TABLE summary_participants ADD COLUMN submitted_at DATETIME NULL;
```

提交时更新：

```go
db.Model(&model.SummaryParticipant{}).
    Where("task_id = ? AND participant_uid = ?", taskID, userID).
    Update("submitted_at", time.Now())
```

**任务状态增加中间状态**：

```
pending → processing → all_submitted → completed
```

所有人提交后先切到 `all_submitted`，meta 完成后切到 `completed`。前端列表中 `all_submitted` 显示"汇总中..."。

---

## 四、问题3：看不到别人的报告

### 根因分析

**后端**：`GET /api/v1/summaries/:id` 返回的数据结构：

```go
type TaskDetailResp struct {
    Task         SummaryTask        `json:"task"`
    Sources      []SummarySource    `json:"sources"`
    PersonalResults []PersonalResult `json:"personal_results"` // 所有参与者的
    MetaResult   *MetaResult        `json:"meta_result"`
}
```

后端可能只返回了当前用户的 PersonalResult（按 `participant_uid = currentUser` 过滤了）。

**前端**：`SummaryDetailPage.tsx` 可能只渲染了 `myPersonalResult`，没有展示所有参与者的报告列表。

### 修复方案

#### 后端修复

`GET /api/v1/summaries/:id` 改为返回所有参与者的 PersonalResult（已提交的）：

```go
// task.go — GetTaskDetail
var personalResults []model.PersonalResult
db.Where("task_id = ?", taskID).
    // 不加 participant_uid 过滤，返回全部
    Find(&personalResults)

// 附带参与者信息（名字、头像等）
// 通过 participant_uid → 查 user 表 join
```

返回格式：

```json
{
  "personal_results": [
    {
      "participant_uid": "5904fca8...",
      "participant_name": "大棍子",
      "participant_avatar": "...",
      "content": "...",
      "submitted_at": "2026-04-27T21:30:00Z",
      "version": 1
    },
    {
      "participant_uid": "2c56cb06...",
      "participant_name": "大背头",
      "participant_avatar": "...",
      "content": "...",
      "submitted_at": "2026-04-27T21:35:00Z",
      "version": 1
    }
  ],
  "meta_result": { ... }
}
```

**权限控制**：所有参与者都可以看到彼此已提交的报告（`submitted_at != null`）。未提交的不展示内容，只展示"等待提交"状态。

#### 前端修复

`SummaryDetailPage.tsx` 详情页底部新增"参与者报告"区块：

```
┌─────────────────────────────────────┐
│ 📋 汇总报告（元总结）                │
│ ... meta result 内容 ...             │
└─────────────────────────────────────┘

┌─────────────────────────────────────┐
│ 👤 参与者报告                        │
│                                     │
│ 大棍子 · 已提交 21:30               │
│ ► 展开查看                           │
│                                     │
│ 大背头 · 已提交 21:35               │
│ ► 展开查看                           │
│                                     │
│ 测试用户2 · 等待提交...              │
└─────────────────────────────────────┘
```

每个参与者的报告默认折叠，点击展开 Markdown 渲染。

---

## 五、改动文件清单

### 后端

| 文件 | 改动 |
|------|------|
| `internal/worker/personal_processor.go` | 扩大消息收集范围（@提及 + DM + 发送者） |
| `internal/llm/prompts.go` | 修改 personal Map prompt |
| `internal/handler/personal.go` | Submit 检查所有人是否提交再触发 meta |
| `internal/model/participant.go` | 加 `SubmittedAt` 字段 |
| `internal/handler/task.go` | GetTaskDetail 返回所有参与者的 PersonalResult |
| `migrations/` | 新增 `submitted_at` 字段 migration |

### 前端

| 文件 | 改动 |
|------|------|
| `SummaryDetailPage.tsx` | 新增"参与者报告"区块，折叠展示 |
| `summaryApi.ts` | 接口类型扩展，PersonalResult 加 participant info |
| `summary.ts` | 类型定义扩展 |

---

## 六、工作量估算

| 模块 | 估算 |
|------|------|
| 问题1：消息收集范围扩大 + prompt 修改 | 0.5d |
| 问题2：提交门控逻辑 + migration | 0.5d |
| 问题3：后端返回全部报告 + 前端渲染 | 1d |
| 联调验证 | 0.5d |
| **合计** | **2.5d** |

---

## 七、待确认

1. 问题1：用户没有相关消息时，是返回空（由前端友好展示）还是保留当前错误文案？
2. 问题3：参与者报告默认折叠还是展开？
3. 参与者报告是否有编辑能力（提交后还能修改）？
