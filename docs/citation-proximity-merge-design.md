# Citation Proximity Merge — 设计文档 v1.0

> Author: Jeff | Date: 2026-04-29 | Status: Draft

## 1. 问题

当前总结中，同一个群/私聊的多条 citation 分散显示：

```
多轮 tool call 测试持续进行，功能验证通过 [77-78] [81] [84] [89] [97] [101-102] [121] [123-124] [136]
```

如果 `[77] [78] [81] [84]` 都来自同一个 channel 且在原始消息流中距离很近，应该合并展示为 `[77-84]`，
点击后展示该范围内的**全部消息**，并区分「被引用」和「上下文补充」。

## 2. 核心发现

### 2.1 CitationIndex ≠ 消息距离

CitationIndex 是**全局跨 channel 递增**的编号。`[77]` 和 `[81]` 的 index 差 4，
但它们在同一 channel 内的实际距离可能是 1（中间只隔 1 条同 channel 消息），
也可能是 100（中间大量其他人的消息）。

**proximity 判断必须用 channel 内数组位置，不是 CitationIndex。**

### 2.2 已有 context 窗口部分覆盖

当前每条 citation 已有 `context_before[2]` + `context_after[2]`。
当两条同 channel citation 在 channelMsgMap 中距离 ≤ 4 时，它们的 context 窗口**已经重叠**，
gap 消息已存在于两边的 context 数据中（但有冗余，且前端不知道如何拼接）。

### 2.3 数据规模

| 指标 | 典型值 |
|------|--------|
| allMessages 总量 | 500-2000 条 |
| 单 channel 消息数 | 100-400 条 |
| 被引用 citation 数 | 80-110 条 |
| 单条 citation JSON 大小 | 1-2 KB |
| citations_json 总大小 | 80-200 KB |

O(n) 扫描在此规模下无性能问题。

## 3. 方案设计

### 3.1 新增数据结构

```go
// CitationGroup — 一组同 channel 的近距离 citations
type CitationGroup struct {
    GroupID     int          `json:"group_id"`       // 组编号，1-based
    ChannelID   string       `json:"channel_id"`
    ChannelType int          `json:"channel_type"`
    Source      string       `json:"source"`          // channel 显示名
    StartIndex  int          `json:"start_index"`     // 组内最小 CitationIndex
    EndIndex    int          `json:"end_index"`       // 组内最大 CitationIndex
    Messages    []GroupMsg   `json:"messages"`         // 范围内全部消息（有序）
}

type GroupMsg struct {
    MessageSeq int64  `json:"message_seq"`
    Sender     string `json:"sender"`
    Content    string `json:"content"`     // 截断 200 runes
    SentAt     string `json:"sent_at"`
    Cited      bool   `json:"cited"`       // true=LLM引用, false=上下文补充
    CitationIndex int `json:"citation_index,omitempty"` // cited=true 时有值
}
```

### 3.2 后端处理流程

在 `personal_processor.go` 的 `buildCitations` → `dedupCitations` 之后，新增一步：

```
buildCitations() → dedupCitations() → buildCitationGroups()  ← 新增
```

#### buildCitationGroups() 伪代码

```
输入: citations []Citation, allMessages []Message, channelMsgMap map[string][]Message
参数: GAP_THRESHOLD = 5, MAX_MERGE_RANGE = 20
输出: groups []CitationGroup

1. 按 channel_id 分组 citations
2. 对每个 channel 组:
   a. 查找每条 citation 在 channelMsgMap[channel_id] 中的数组位置 pos
      （通过 MessageSeq 匹配，O(n) 扫描）
   b. 按 pos 排序
   c. 贪心合并: 相邻 citation 的 pos 差 ≤ GAP_THRESHOLD → 同组
      约束: 单组 pos 跨度 ≤ MAX_MERGE_RANGE
   d. 对每个组:
      - 从 channelMsgMap 取 [min_pos, max_pos] 范围内全部消息
      - 标记 cited=true/false
      - 转换为 GroupMsg（截断 content）
3. 未被合并的单独 citation → 也包装成单条 CitationGroup（messages 只含自身）
4. 按首条 citation 的 CitationIndex 排序所有 groups
```

### 3.3 API 响应变更

在现有 `citations` 数组旁边，新增 `citation_groups` 字段：

```json
{
  "content": "多轮 tool call 测试...[77][78][81][84]...[89]...",
  "citations": [ ... ],         // 保持不变（向后兼容）
  "citation_groups": [          // 新增
    {
      "group_id": 1,
      "channel_id": "group-xyz",
      "channel_type": 2,
      "source": "Bot测试群",
      "start_index": 77,
      "end_index": 84,
      "messages": [
        {"message_seq": 501, "sender": "小指头", "content": "测试tool call", "sent_at": "10:31", "cited": true, "citation_index": 77},
        {"message_seq": 502, "sender": "小指头", "content": "再试一次", "sent_at": "10:32", "cited": true, "citation_index": 78},
        {"message_seq": 503, "sender": "Boris", "content": "收到，执行中", "sent_at": "10:33", "cited": false},
        {"message_seq": 504, "sender": "Boris", "content": "tool call 结果...", "sent_at": "10:34", "cited": false},
        {"message_seq": 510, "sender": "小指头", "content": "验证通过", "sent_at": "10:35", "cited": true, "citation_index": 81},
        ...
      ]
    },
    {
      "group_id": 2,
      "channel_id": "group-xyz",
      "source": "Bot测试群",
      "start_index": 89,
      "end_index": 89,
      "messages": [
        {"message_seq": 530, "sender": "小指头", "content": "...", "sent_at": "10:50", "cited": true, "citation_index": 89}
      ]
    }
  ]
}
```

### 3.4 前端渲染

#### Badge 合并

```
原始文本:  ...功能验证通过 [77][78][81][84]
渲染逻辑:  查 citation_groups → [77],[78],[81],[84] 属于 group_id=1
显示:      ...功能验证通过 [77-84]  （单个蓝色 badge）
```

#### 点击展开

| 消息 | sender | 样式 |
|------|--------|------|
| [77] 测试tool call | 小指头 | **高亮**（cited） |
| [78] 再试一次 | 小指头 | **高亮**（cited） |
| 收到，执行中 | Boris | 灰色/半透明（gap fill） |
| tool call 结果... | Boris | 灰色/半透明（gap fill） |
| [81] 验证通过 | 小指头 | **高亮**（cited） |
| ... | ... | ... |
| [84] 最终确认 | 小指头 | **高亮**（cited） |

### 3.5 现有 citations 数组的处理

**保持不变**。`citation_groups` 是增量字段：
- 旧前端不认识 `citation_groups` → 退化为当前行为（独立 badge + context_before/after）
- 新前端优先用 `citation_groups` 渲染
- `citations` 数组中的 `context_before/context_after` 可保留也可置空（group 已包含完整上下文）

## 4. 边界约束

| 约束 | 值 | 说明 |
|------|-----|------|
| GAP_THRESHOLD | 5 | channel 内数组位置差 ≤ 5 才合并 |
| MAX_MERGE_RANGE | 20 | 单组最大消息跨度，防止 [1-136] |
| MAX_GAP_FILL_TOTAL | 100 | 全部 groups 的 gap fill 消息总数上限 |
| Content 截断 | 200 runes | 与现有 Citation 一致 |

## 5. 与现有 context_before/after 的关系

| 场景 | context_before/after | citation_groups |
|------|---------------------|-----------------|
| 单独 citation | 提供 2+2 上下文 ✅ | 只含自身 1 条 |
| 2 条 citation 距离 ≤ 4 | 窗口重叠，有冗余 | 合并为 1 组，无冗余 |
| 2 条 citation 距离 > 5 | 各自独立窗口 ✅ | 不合并，各自成组 |

**建议**：当 `citation_groups` 启用后，`context_before/after` 可以考虑在未来版本移除以减少 JSON 体积。
过渡期两者共存。

## 6. 实现计划

| 阶段 | 内容 | 改动 |
|------|------|------|
| Step 1 | 后端 `buildCitationGroups()` | `citation.go` 新函数 + `model.go` 新 struct |
| Step 2 | API 响应补 `citation_groups` | `task.go` + `personal.go` 序列化 |
| Step 3 | 前端 badge 合并渲染 | `CitationText.tsx` 识别 groups |
| Step 4 | 前端 group popup 展示 | `CitationBadge.tsx` / 新组件 |

## 7. 测试用例

| Case | 输入 citations | 期望 groups |
|------|---------------|-------------|
| 连续同 channel | [5][6][7] from ch-A | 1 group: [5-7], 3 cited, 0 gap |
| 近距离同 channel | [10][14] from ch-A (gap=3) | 1 group: [10-14], 2 cited, 3 gap |
| 超过 threshold | [10][20] from ch-A (gap=8) | 2 groups: [10], [20] |
| 跨 channel | [10] ch-A, [11] ch-B | 2 groups（不合并） |
| 超过 max range | [1]...[25] ch-A 每隔 1 | 拆分为多组，每组 ≤ 20 |
| 链式合并 | [10][14][18][22] gap=3,3,3 | 1 group [10-22] (range=12 < 20) |
| 混合 channel | [10][11] ch-A, [12] ch-B, [13][14] ch-A | ch-A: group[10-11], group[13-14]; ch-B: group[12] |
