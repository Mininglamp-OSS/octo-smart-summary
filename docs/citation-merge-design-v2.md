# Citation 合并优化方案 v2

## 问题

1. **重复 citation** — 同一 `[n]` 在文本中出现多次（如 `[84]` 出现 3 次）
2. **单条 citation 无意义** — 一堆孤立的 `[30][31][32]...` 点开只看到一条消息，缺乏上下文

---

## 设计原则

- **后端不改文本格式** — 文本中保持 `[n]` 原始格式，不引入 `[n-m]` 范围语法
- **前端负责合并展示** — 连续 citation 的合并显示完全在前端渲染层处理
- **向后兼容** — 旧前端忽略新字段即可，不破坏现有功能
- **不引入新 DB 字段** — 扩展现有 Citation 结构，不新增表或列

---

## 后端改动

### 改动 1：全局去重 — `collapseGlobalDuplicates()`

**文件**：`internal/worker/citation.go`

**问题**：LLM 生成的文本中同一 `[n]` 可能出现多次。现有 `collapseConsecutiveMarkers()` 只处理相邻重复，不处理全局重复。

**方案**：新增函数，扫描全文，同一 `[n]` 只保留首次出现，后续出现删除（连带前导空格清理）。

```go
// 包级别预编译正则
var multiSpaceRe = regexp.MustCompile(`[ \t]{2,}`)
var emptyLineRe = regexp.MustCompile(`(?m)^\s*$\n`)

func collapseGlobalDuplicates(text string) string {
    seen := make(map[string]bool)
    result := citationRe.ReplaceAllStringFunc(text, func(match string) string {
        if seen[match] {
            return ""
        }
        seen[match] = true
        return match
    })
    // 清理删除 marker 后残留的多余空格和空行
    result = multiSpaceRe.ReplaceAllString(result, " ")
    result = emptyLineRe.ReplaceAllString(result, "")
    return strings.TrimSpace(result)
}
```

**注意**：正则必须提取为包级别 `var`，避免每次调用重复编译。空行清理防止删除 marker 后产生空行。

**调用点**：`internal/worker/personal_processor.go`，在 `dedupCitations()` 之后追加：
```go
finalContent, citations = dedupCitations(finalContent, citations)
finalContent = collapseGlobalDuplicates(finalContent)  // 新增
```

### 改动 2：Citation 结构增加上下文字段

**文件**：`internal/model/model.go`

扩展现有 `Citation` 结构，新增 `context_before` 和 `context_after`：

```go
type Citation struct {
    Index      int           `json:"index"`
    Sender     string        `json:"sender"`
    Content    string        `json:"content"`
    SentAt     string        `json:"sent_at"`
    Source     string        `json:"source"`
    ChannelID  string        `json:"channel_id"`
    ChannelType int          `json:"channel_type"`     // 新增：1=DM, 2=Group
    MessageSeq int64         `json:"message_seq"`
    ContextBefore []ContextMsg `json:"context_before,omitempty"` // 新增
    ContextAfter  []ContextMsg `json:"context_after,omitempty"`  // 新增
}

type ContextMsg struct {
    Sender  string `json:"sender"`
    Content string `json:"content"`
    SentAt  string `json:"sent_at"`
}
```

**不需要 DB migration**：citations 已经是 JSON 序列化存储在 `citations_json` TEXT 列中，新增字段自动包含在 JSON 里。旧数据反序列化时 `context_before/after` 为 nil，前端正常处理。

### 改动 3：`buildCitations()` 填充上下文

**文件**：`internal/worker/citation.go`

修改 `buildCitations()` 函数签名，增加 `allMessages []pipeline.Message` 参数（原始未过滤消息列表），用于查找上下文：

```go
func buildCitations(text string, messages []pipeline.Message, allMessages []pipeline.Message, nameMap map[string]string) []model.Citation
```

对每个被引用的消息，在 `allMessages` 中定位其 `message_seq`，向前取 2 条、向后取 2 条（同一 `channel_id` 内），填入 `ContextBefore` / `ContextAfter`。

**上下文查找逻辑**：
```go
func findContext(target pipeline.Message, allMessages []pipeline.Message, nameMap map[string]string, n int) (before []model.ContextMsg, after []model.ContextMsg) {
    // 1. 在 allMessages 中找到 target 的位置（按 channel_id + message_seq 匹配）
    // 2. 向前扫描同 channel_id 的消息，取 n 条
    // 3. 向后扫描同 channel_id 的消息，取 n 条
    // 4. content 截断到 200 字符
    // 5. sender 通过 nameMap 转换
}
```

**调用点变更**：`internal/worker/personal_processor.go`

需要把原始 `allMessages`（filter 之前的完整消息列表）传递到 `buildCitations`。当前 `executePersonalPipeline` 中 `userMessages` 是过滤后的，需要保留过滤前的 `fetchedMessages` 变量并传入。

**注意**：`batchResolveUserNames` 的输入也要改为全量 `messages`（而非过滤后的 `userMessages`），否则上下文消息的 sender 会显示为裸 UID。

### 改动 4：Citation 增加 `channel_type` 字段

**文件**：`internal/worker/citation.go` → `buildCitations()`

从 `pipeline.Message` 中取 channel_type 填入 Citation，修复前端跳转时硬编码 `ChannelTypeGroup` 的 bug。

**确认：`pipeline.Message` 当前没有 `ChannelType` 字段**。需要以下改动：

1. `internal/pipeline/fetch.go` — `Message` struct 新增 `ChannelType int` 字段
2. `internal/pipeline/fetch.go` — `ResolveAndFetchMessagesForPersonal()` 中，在 `msgs[i].SourceName = ch.ChannelName` 后追加 `msgs[i].ChannelType = ch.ChannelType`
3. `internal/worker/citation.go` — `buildCitations()` 中从 `msg.ChannelType` 填入 `Citation.ChannelType`

---

## 前端改动

### 改动 P0：修复 CitationText 正则 bug（阻塞项）

**文件**：`packages/dmworkbase/src/Pages/Summary/components/CitationText.tsx`

**现有 bug**：正则 `/(?<!\])\[(\d+)\](?!\()/g` 中的负向后行断言 `(?<!\])` 导致连续 citation 只有第一个能被匹配。例如 `[1][2][3]` 中，`[2]` 和 `[3]` 因前面紧邻 `]` 而不匹配，无法渲染为 badge。

**修复**：去掉 `(?<!\])`，改为：
```ts
const regex = /\[(\d+)\](?!\()/g;
```

**安全性**：Markdown link `[text][1]` 中的 `[1]` 不会误匹配，因为 remark AST 解析阶段已将其处理为 `link` 节点，不会出现在 `text` 节点中。

**此修复是 P3 的前置依赖**，也是独立的现有 bug fix，建议立即部署。

---

### 改动 5：CitationText 连续 citation 合并显示

**文件**：`packages/dmworkbase/src/Pages/Summary/components/CitationText.tsx`

解析逻辑增强：检测连续的 `[n][n+1][n+2]...` 序列（基于 CitationIndex 数字连续），渲染为单个合并 badge：

- 2-3 个连续：显示 `[30-32]`
- 4 个以上连续：显示 `[30-32 等 N 条]`
- 非连续的正常显示各自 badge

**注意**：这是纯展示层逻辑，后端文本不变。前端正则仍然匹配 `\[(\d+)\]`，只是渲染时检测相邻 match 的 index 连续性。

### 改动 6：CitationBadge 支持 group 模式

**文件**：`packages/dmworkbase/src/Pages/Summary/components/CitationBadge.tsx`

新增 `CitationGroupBadge` 组件：

- 接收一组 citations（合并后的）
- Popover 内展示消息列表：
  - `context_before` 消息：浅灰色/斜体，标注"上下文"
  - 被引用消息：正常颜色，左侧蓝色竖线标记
  - `context_after` 消息：浅灰色/斜体
- 底部"跳转到原文"使用第一条被引用消息的 `channel_id` + `channel_type` + `message_seq`

### 改动 7：修复 ChannelType 硬编码

**文件**：`packages/dmworkbase/src/Pages/Summary/components/CitationBadge.tsx`

```tsx
// 旧代码
const channel = new Channel(citation.channel_id!, ChannelTypeGroup);

// 新代码
const channelType = citation.channel_type === 1 ? ChannelTypePerson : ChannelTypeGroup;
const channel = new Channel(citation.channel_id!, channelType);
```

---

## 实施步骤

| 阶段 | 步骤 | 内容 | 可独立部署 |
|------|------|------|-----------|
| P0 | 前端正则修复 | 修复 `(?<!\])` 导致连续 citation 只渲染第一个的 bug | ✅ 前端独立，现有 bug fix |
| P1 | 1 | `collapseGlobalDuplicates()` 去重 | ✅ 后端独立，立即生效 |
| P2 | 2-4 | Citation 加上下文 + channel_type + nameMap 全量解析 + Message.ChannelType 字段 | ✅ 后端独立，旧前端忽略新字段 |
| P3 | 5-7 | 前端合并展示 + group badge + channel_type 修复 | 需 P0 + P2 先上线 |

P0 是现有 bug，建议立即修复。P1 可以今晚直接部署。P2 后端改动约 100 行（含 Message struct + nameMap 修改），可以独立上线。P3 前端改动等 Ploy 排期。

---

## 不做的事

- **不改文本中的 citation 格式** — 不引入 `[n-m]` 范围语法
- **不新增 DB 表或列** — 复用现有 `citations_json` TEXT 字段
- **不新增 `CitationGroup` 数据结构** — 扩展现有 `Citation` 结构即可
- **不改 LLM prompt** — 现有 prompt 已有防重复指令，后端兜底去重
