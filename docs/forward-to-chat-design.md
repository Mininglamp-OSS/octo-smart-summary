# 智能总结 — "转发到聊天"功能技术设计文档

> 版本：v1.1 | 日期：2026-04-27 | 修订：修复技术栈、文件路径、路由 API 等问题

---

## 一、背景

智能总结生成的结果目前只能在详情页查看。需要支持用户将总结结果转发到任意聊天会话中，方便团队成员快速获取总结内容。

---

## 二、现状分析

### 后端（`/tmp/smart-summary-go/`）

- **Go 1.21 + Gin v1.10.0 + GORM v1.25.5 + MySQL**
- Redis 缓存、robfig/cron 定时任务、gorilla/websocket
- **零转发/分享逻辑**，需从零新建
- 总结结果存储在 `summary_result` 表（`internal/model/model.go`）：

```go
type SummaryResult struct {
    ID             uint      `gorm:"primarykey"`
    TaskID         uint      `gorm:"index"`
    Content        string    `gorm:"type:longtext"`        // Markdown 长文本
    TotalMsgCount  int
    TotalTokenUsed int
    ModelVersion   string
    Version        int                                      // 支持多版本
    GeneratedAt    time.Time
    CreatedAt      time.Time
    UpdatedAt      time.Time
}
```

- 详情接口：`GET /api/v1/summaries/:id` 返回完整任务 + sources + participants + latest result

### 前端（`/tmp/dmwork-web/`）

- **已有通用转发组件**，可直接复用：
  - `ForwardModal`（`packages/dmworkbase/src/Components/ForwardModal/ForwardModal.tsx`）— 双列布局：左侧会话列表 + 搜索，右侧已选目标
  - `useForwardModal`（同目录）— 加载最近会话 + 好友列表，300ms 防抖搜索
  - `ConversationSelect`（`packages/dmworkbase/src/Components/ConversationSelect/index.tsx`）— 封装组件，包装 ForwardModal + useForwardModal
  - 弹窗入口：`WKApp.shared.baseContext.showConversationSelect(callback, title)` — 定义在 `packages/dmworkbase/src/Components/WKBase/index.tsx:99-108`
- **已有消息发送链路**：
  - `ConversationVM.sendMessage(content, channel)` → `WKSDK.shared().chatManager.send()` → WebSocket
- 总结详情页：`packages/dmworkbase/src/Pages/Summary/pages/SummaryDetailPage.tsx`（~670 行）
- 总结内容渲染：`packages/dmworkbase/src/Pages/Summary/components/SummaryContent.tsx`（react-markdown）

### 已注册的 contentType 编号

来源：`packages/dmworkbase/src/module.tsx:171-245` + `Service/Const.ts:44-80`

| contentType | ID | 说明 |
|-------------|-----|------|
| image | 2 | 图片 |
| gif | 3 | GIF |
| voice | 4 | 语音 |
| smallVideo | 5 | 短视频 |
| location | 6 | 位置 |
| card | 7 | 名片 |
| file | 8 | 文件 |
| mergeForward | 11 | 合并转发 |
| lottieSticker | 12 | Lottie 贴纸 |
| lottieEmojiSticker | 13 | Lottie Emoji 贴纸 |
| joinOrganization | 16 | 加入组织 |
| screenshot | 20 | 截图 |
| historySplit | -3 | 历史分割线 |
| threadCreated | 1100 | 子区创建 |

**可用编号**：9、10、14、15、17-19 均未被占用。建议使用 **contentType = 14 或 15**，但需与 WuKongIM 服务端确认这些编号在协议层是否有预留含义。

---

## 三、方案设计

### 推荐方案：卡片消息 + 详情链接

发到聊天的不是全文，而是一张**摘要卡片**：

```
┌─────────────────────────────────┐
│ 📊 智能总结                      │
│                                 │
│ 标题：最近一周的工作进展            │
│ 来源：3个群聊 | 262条消息          │
│ 时间：4/20 - 4/27               │
│                                 │
│         [ 查看完整总结 ]           │
└─────────────────────────────────┘
```

**为什么不直接发全文？**

- 总结内容可能数千字，直接发会严重刷屏
- 普通文本消息不支持 Markdown 渲染，格式丢失
- 卡片形式更专业，点击跳转详情页体验更好

---

## 四、前端改动（主要工作量）

### 4.1 详情页添加"转发到聊天"按钮

**文件**：`packages/dmworkbase/src/Pages/Summary/pages/SummaryDetailPage.tsx`

- Header 区域新增"转发到聊天"按钮（仅 `status === COMPLETED` 时显示）
- 点击调用已有的 `WKApp.shared.baseContext.showConversationSelect(callback, "转发到聊天")` 打开 ForwardModal

### 4.2 新增 SummaryCardContent 消息类型

**新文件**：`packages/dmworkbase/src/Messages/SummaryCard/SummaryCardContent.ts`

```typescript
import { MessageContent } from "wukongimjssdk";

class SummaryCardContent extends MessageContent {
  contentType = 15; // ⚠️ 需与 WuKongIM 服务端确认此编号在协议层无预留

  taskId!: number;
  taskNo!: string;
  title!: string;
  sourceCount!: number;       // 信息来源数量
  totalMsgCount!: number;     // 消息总数
  timeRangeStart!: string;    // 起始时间
  timeRangeEnd!: string;      // 结束时间
  summaryMode!: number;       // 1=按群 2=按人
  spaceId!: string;           // 用于构建详情页 URL

  encodeJSON(): Record<string, any> {
    return {
      type: this.contentType,
      task_id: this.taskId,
      task_no: this.taskNo,
      title: this.title,
      source_count: this.sourceCount,
      total_msg_count: this.totalMsgCount,
      time_range_start: this.timeRangeStart,
      time_range_end: this.timeRangeEnd,
      summary_mode: this.summaryMode,
      space_id: this.spaceId,
    };
  }

  decodeJSON(json: Record<string, any>): void {
    this.taskId = json.task_id;
    this.taskNo = json.task_no;
    this.title = json.title;
    this.sourceCount = json.source_count;
    this.totalMsgCount = json.total_msg_count;
    this.timeRangeStart = json.time_range_start;
    this.timeRangeEnd = json.time_range_end;
    this.summaryMode = json.summary_mode;
    this.spaceId = json.space_id;
  }
}

export default SummaryCardContent;
```

**注册**（`packages/dmworkbase/src/module.tsx`，与其他 contentType 注册放在一起）：

```typescript
WKSDK.shared().register(15, () => new SummaryCardContent());
```

### 4.3 卡片渲染组件

**新文件**：`packages/dmworkbase/src/Messages/SummaryCard/index.tsx`

- 渲染卡片样式（标题、来源数、消息数、时间范围）
- "查看完整总结"按钮，点击通过 `WKApp.routeRight.push(<SummaryDetailPage taskId={taskId} spaceId={spaceId} />)` 跳转详情页
- 按群 / 按人模式显示不同图标

### 4.4 转发回调逻辑

```
用户点击"转发到聊天"按钮
  → WKApp.shared.baseContext.showConversationSelect(callback, "转发到聊天")
  → ForwardModal 弹出，用户选择目标会话
  → callback 返回 Channel[]
  → 从当前任务数据组装 SummaryCardContent
  → 遍历 Channel[]，逐个调用 WKSDK.shared().chatManager.send(cardContent, channel)
  → Toast 提示"已转发"
```

### 4.5 添加"进入聊天"按钮

**文件**：`SummaryDetailPage.tsx`

- 在来源列表中，每个来源项旁边加"进入聊天"按钮
- 点击后通过 `WKApp.endpoints.showConversation(channel)` 跳转到对应会话：

```typescript
import { Channel } from "wukongimjssdk";

// 群聊 (channel_type=2)
const channel = new Channel(sourceId, 2);
WKApp.endpoints.showConversation(channel);

// 私聊 (channel_type=1)
const channel = new Channel(sourceId, 1);
WKApp.endpoints.showConversation(channel);

// 子区 (channel_type=需确认)
// source_type=2 对应子区，channelType 值需确认
```

---

## 五、后端改动（可选，建议加）

### 5.1 转发记录接口

**文件**：`internal/api/handler/task.go`

```http
POST /api/v1/summaries/:id/share
Content-Type: application/json
X-User-ID: {uid}

{
  "target_channels": [
    {"channel_id": "group_xxx", "channel_type": 2},
    {"channel_id": "user_xxx", "channel_type": 1}
  ]
}

Response 200:
{
  "ok": true,
  "shared_at": "2026-04-27T21:00:00Z",
  "share_count": 2
}
```

**作用**：

- 校验当前用户是否有权转发该任务（必须是创建者或已接受的参与者）
- 记录转发行为（审计追溯）
- 未来可在详情页展示"已转发到 N 个会话"

### 5.2 数据模型扩展

**文件**：`internal/model/model.go`

```go
type SummaryShare struct {
    ID                uint      `gorm:"primarykey"`
    TaskID            uint      `gorm:"index;not null"`
    SharedBy          string    `gorm:"size:64;not null"`         // 转发人 UID
    TargetChannelID   string    `gorm:"size:128;not null"`
    TargetChannelType int       `gorm:"not null"`                 // 1=私聊 2=群聊
    SharedAt          time.Time `gorm:"autoCreateTime"`
}

func (SummaryShare) TableName() string {
    return "summary_share"
}
```

### 5.3 路由注册

**文件**：`internal/api/router/router.go`

```go
summaries.POST("/:id/share", handler.ShareSummary)
```

---

## 六、备选简化方案

如果暂不引入自定义消息类型，可用**纯文本截取方案**：

```typescript
import { MessageText } from "wukongimjssdk";

const preview = result.content.substring(0, 200) + "...";
const text = `📊 智能总结：${task.title}\n\n${preview}\n\n🔗 查看完整总结：${detailUrl}`;
const content = new MessageText();
content.text = text;
WKSDK.shared().chatManager.send(content, channel);
```

**优点**：零后端改动，零新消息类型，0.5d 完成
**缺点**：体验差，无卡片样式，链接可能不可点击

---

## 七、工作量估算

| 模块 | 工作量 | 说明 |
|------|--------|------|
| 详情页转发按钮 + ForwardModal 对接 | 0.5d | 复用现有 `showConversationSelect` |
| SummaryCardContent 消息类型 | 1d | encode/decode + SDK 注册 + module.tsx |
| 卡片渲染组件 | 0.5d | UI 样式 + 点击跳转 |
| "进入聊天"按钮 | 0.25d | `WKApp.endpoints.showConversation()` |
| 后端 share 接口 + 数据模型（可选） | 0.5d | handler + model + router |
| **合计（完整方案）** | **2.5 - 2.75d** | |
| **合计（简化方案）** | **0.5d** | 纯文本截取 |

---

## 八、待确认项

| # | 问题 | 影响 |
|---|------|------|
| 1 | 走卡片方案还是简化文本方案？ | 决定工作量和体验 |
| 2 | contentType 编号确认：当前 9/10/14/15/17-19 未占用，但需与 WuKongIM 服务端确认协议层预留 | 避免编号冲突 |
| 3 | 是否需要后端审计接口？ | 决定是否记录转发行为 |
| 4 | 转发权限范围：仅创建者可转发，还是所有参与者都可以？ | 权限校验逻辑 |
| 5 | 卡片中的"查看完整总结"链接，未被邀请的人点击后如何处理？ | 需要鉴权兜底 |
| 6 | 子区（source_type=2）对应的 WuKongIM channelType 值？ | "进入聊天"跳转逻辑 |

---

## 九、关键文件索引

### 前端（需改动）

```
packages/dmworkbase/src/Pages/Summary/pages/SummaryDetailPage.tsx  ← 加转发按钮 + 进入聊天
packages/dmworkbase/src/Pages/Summary/api/summaryApi.ts            ← 加 share API 调用（如需后端接口）
packages/dmworkbase/src/Messages/SummaryCard/                      ← 新增：消息类型 + 渲染组件
packages/dmworkbase/src/module.tsx                                 ← 注册 contentType
```

### 前端（可复用，无需改动）

```
packages/dmworkbase/src/Components/ForwardModal/ForwardModal.tsx   ← 转发 Modal UI
packages/dmworkbase/src/Components/ForwardModal/useForwardModal.ts ← 转发逻辑 Hook
packages/dmworkbase/src/Components/ConversationSelect/index.tsx    ← 封装组件
packages/dmworkbase/src/Components/WKBase/index.tsx:99-108         ← showConversationSelect() 定义
```

### 后端（可选改动）

```
internal/api/handler/task.go    ← 新增 ShareSummary handler
internal/model/model.go         ← 新增 SummaryShare 结构体
internal/api/router/router.go   ← 新增 share 路由
```
