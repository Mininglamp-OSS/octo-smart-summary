# Citation 合并优化方案 v3

> v3 基于 v2 方案 + code review 修订。变更点标注 `[v3-FIX]`。

## 问题

1. **重复 citation** -- 同一 `[n]` 在文本中出现多次（如 `[84]` 出现 3 次）
2. **单条 citation 无意义** -- 一堆孤立的 `[30][31][32]...` 点开只看到一条消息，缺乏上下文

---

## 设计原则

- **后端不改文本格式** -- 文本中保持 `[n]` 原始格式，不引入 `[n-m]` 范围语法
- **前端负责合并展示** -- 连续 citation 的合并显示完全在前端渲染层处理
- **向后兼容** -- 旧前端忽略新字段即可，不破坏现有功能
- **不引入新 DB 字段** -- 扩展现有 Citation 结构，不新增表或列

---

## 后端改动

### 改动 1：全局去重 -- 合并进 `dedupCitations()`

**文件**：`internal/worker/citation.go`

**问题**：LLM 生成的文本中同一 `[n]` 可能出现多次。现有 `collapseConsecutiveMarkers()` 只处理相邻重复，不处理全局重复。

**`[v3-FIX]` SUGGESTION-1**：不再新增独立的 `collapseGlobalDuplicates()` 函数，而是将全局去重逻辑合并进现有 `dedupCitations()` 的末尾，减少调用方需要维护的调用链。

**`[v3-FIX]` ISSUE-1**：空行清理正则从 `(?m)^\s*$\n` 改为 `(?m)^[ \t]*$\n`。原正则中 `\s` 匹配 `\n` 会跨行贪婪删除有意义的段落分隔。改为 `[ \t]` 只匹配水平空白字符。

```go
// 包级别预编译正则（新增）
var multiSpaceRe = regexp.MustCompile(`[ \t]{2,}`)
var emptyLineRe  = regexp.MustCompile(`(?m)^[ \t]*$\n`)  // [v3-FIX] 只匹配水平空白，不跨行
```

**`[v3-FIX]` 自审 ISSUE-A**：`dedupCitations()` 在 L105-107 有 early return：当 `len(remap) == 0` 时直接返回。但全局去重的核心场景正是 remap 为空（同一 `[n]` 重复出现，但 citation 内容不重复）。如果全局去重放在 early return 之后，永远不执行 —— 变成死代码。

**解决方案**：将全局去重放在 early return **之前**（remap 计算之后、early return 判断之前），或重构为 early return 只跳过 remap-application 路径。

**修改函数**：`dedupCitations()`：

```go
func dedupCitations(text string, citations []model.Citation) (string, []model.Citation) {
    // ---- 现有逻辑：计算 remap（同 sender+content 的重复 citation 合并编号）----
    // ...（省略现有 remap 计算逻辑，保持不变）...

    // ---- [v3-FIX] 全局去重必须在 early return 之前执行 ----
    // 同一 [n] 只保留首次出现，后续删除（解决 LLM 重复引用同编号的问题）
    seen := make(map[string]bool)
    newText := citationRe.ReplaceAllStringFunc(text, func(match string) string {
        if seen[match] {
            return ""
        }
        seen[match] = true
        return match
    })
    // 清理删除 marker 后残留的多余空格和空行
    newText = multiSpaceRe.ReplaceAllString(newText, " ")
    newText = emptyLineRe.ReplaceAllString(newText, "")
    newText = strings.TrimSpace(newText)

    // ---- early return：如果没有 remap（无重复 citation），到此结束 ----
    if len(remap) == 0 {
        return newText, citations
    }

    // ---- 有 remap 时：替换文本中的编号 + collapseConsecutiveMarkers ----
    // ...（省略现有 remap 应用逻辑，保持不变）...

    // Collapse consecutive identical markers: [1][1][1] -> [1]
    newText = collapseConsecutiveMarkers(newText)

    // ---- 构建去重后的 citation 列表 ----
    // ...（省略现有 citation 列表构建，保持不变）...

    return newText, result
}
```

**调用点**：`internal/worker/personal_processor.go` L252。**无需修改**（不再需要追加 `collapseGlobalDuplicates` 调用，去重已内置在 `dedupCitations` 中）。

```go
// personal_processor.go L251-252 保持不变
citations := buildCitations(finalContent, userMessages, messages, nameMap)
finalContent, citations = dedupCitations(finalContent, citations)
```

**`[v3-FIX]` 自审 ISSUE-B**：现有测试 `TestDedupCitations_MixedDuplicatesAndUnique`（L179-201）也会受全局去重影响。remap 将 `[3]→[2]` 后文本中有两个 `[2]`，全局去重会删除第二个。此测试断言需同步更新。归入 P1 测试修改清单。

---

### 改动 2：Citation 结构增加上下文字段 + ChannelType 常量

**文件**：`internal/model/model.go`

**`[v3-FIX]` ISSUE-7**：新增 `ChannelType` 后端常量定义，与 IM 系统对齐。

在现有 `SourceGroup/SourceThread/SourceDirect` 常量块之后追加：

```go
// Channel type constants (aligned with WuKongIM).
const (
    ChannelTypeDM    = 1 // 私聊
    ChannelTypeGroup = 2 // 群聊
)
```

扩展现有 `Citation` 结构（当前定义在 `model.go` L158-166），新增 `channel_type`、`context_before`、`context_after`：

```go
type Citation struct {
    Index         int          `json:"index"`
    Sender        string       `json:"sender"`
    Content       string       `json:"content"`
    SentAt        string       `json:"sent_at"`
    Source        string       `json:"source"`
    ChannelID     string       `json:"channel_id"`
    ChannelType   int          `json:"channel_type"`                   // 新增：1=DM, 2=Group
    MessageSeq    int64        `json:"message_seq"`
    ContextBefore []ContextMsg `json:"context_before,omitempty"`       // 新增
    ContextAfter  []ContextMsg `json:"context_after,omitempty"`        // 新增
}

type ContextMsg struct {
    Sender  string `json:"sender"`
    Content string `json:"content"`
    SentAt  string `json:"sent_at"`
}
```

**不需要 DB migration**：citations 已经是 JSON 序列化存储在 `citations_json` TEXT 列中，新增字段自动包含在 JSON 里。旧数据反序列化时 `context_before/after` 为 nil，前端正常处理。

---

### 改动 3：`buildCitations()` 填充上下文 + 签名变更

**文件**：`internal/worker/citation.go`

**`[v3-FIX]` ISSUE-2**：`buildCitations()` 从 3 参数改为 4 参数（新增 `allMessages`），会破坏 `citation_test.go` 中 4 个现有测试。P2 阶段测试修改见实施步骤表。

修改 `buildCitations()` 函数签名（当前 `citation.go` L35）：

```go
// 旧签名
func buildCitations(text string, messages []pipeline.Message, nameMap map[string]string) []model.Citation

// 新签名
func buildCitations(text string, messages []pipeline.Message, allMessages []pipeline.Message, nameMap map[string]string) []model.Citation
```

在函数体内，对每个被引用的消息调用 `findContext()` 填充上下文，并从 `msg.ChannelType` 填充 `Citation.ChannelType`：

```go
func buildCitations(text string, messages []pipeline.Message, allMessages []pipeline.Message, nameMap map[string]string) []model.Citation {
    indexes := extractCitationIndexes(text)
    if len(indexes) == 0 {
        return []model.Citation{}
    }

    indexSet := make(map[int]bool, len(indexes))
    for _, idx := range indexes {
        indexSet[idx] = true
    }

    // [v3-FIX] ISSUE-3: 预先按 channel_id 建立消息索引
    channelMsgMap := buildChannelMessageMap(allMessages)

    var citations []model.Citation
    for _, msg := range messages {
        if indexSet[msg.CitationIndex] {
            // [v3-FIX] SUGGESTION-2: 用 []rune 截断，避免 UTF-8 中文截断
            content := truncateRunes(msg.Content, 200)

            sender := msg.SenderUID
            if nameMap != nil {
                if name, ok := nameMap[msg.SenderUID]; ok && name != "" {
                    sender = name
                }
            }

            before, after := findContext(msg, channelMsgMap, nameMap, 2)

            citations = append(citations, model.Citation{
                Index:         msg.CitationIndex,
                Sender:        sender,
                Content:       content,
                SentAt:        msg.SendTime,
                Source:        msg.SourceName,
                ChannelID:     msg.ChannelID,
                ChannelType:   msg.ChannelType,     // 新增
                MessageSeq:    msg.MessageSeq,
                ContextBefore: before,               // 新增
                ContextAfter:  after,                 // 新增
            })
        }
    }
    if citations == nil {
        return []model.Citation{}
    }
    return citations
}
```

**`[v3-FIX]` SUGGESTION-2**：新增 rune 安全截断函数：

```go
func truncateRunes(s string, maxRunes int) string {
    runes := []rune(s)
    if len(runes) <= maxRunes {
        return s
    }
    return string(runes[:maxRunes]) + "..."
}
```

---

### 改动 4：`findContext()` 按 channel 分组查找

**文件**：`internal/worker/citation.go`

**`[v3-FIX]` ISSUE-3**：`allMessages` 中不同 channel 的消息是交错的（`fetch.go` L519-529 循环追加各 channel 消息）。简单 `index±1` 会取错 channel。必须先按 `channel_id` 建 map，再在组内查找。

新增辅助函数 `buildChannelMessageMap()` 和修订后的 `findContext()`：

```go
// buildChannelMessageMap groups allMessages by channel_id, preserving order.
func buildChannelMessageMap(allMessages []pipeline.Message) map[string][]pipeline.Message {
    m := make(map[string][]pipeline.Message)
    for _, msg := range allMessages {
        m[msg.ChannelID] = append(m[msg.ChannelID], msg)
    }
    return m
}

// findContext locates n messages before and after target within the same channel.
func findContext(target pipeline.Message, channelMsgMap map[string][]pipeline.Message, nameMap map[string]string, n int) ([]model.ContextMsg, []model.ContextMsg) {
    channelMsgs, ok := channelMsgMap[target.ChannelID]
    if !ok {
        return nil, nil
    }

    // 在同 channel 消息中找到 target 的位置（按 message_seq 匹配）
    targetIdx := -1
    for i, msg := range channelMsgs {
        if msg.MessageSeq == target.MessageSeq {
            targetIdx = i
            break
        }
    }
    if targetIdx < 0 {
        return nil, nil
    }

    // 向前取 n 条
    var before []model.ContextMsg
    start := targetIdx - n
    if start < 0 {
        start = 0
    }
    for i := start; i < targetIdx; i++ {
        before = append(before, toContextMsg(channelMsgs[i], nameMap))
    }

    // 向后取 n 条
    var after []model.ContextMsg
    end := targetIdx + n + 1
    if end > len(channelMsgs) {
        end = len(channelMsgs)
    }
    for i := targetIdx + 1; i < end; i++ {
        after = append(after, toContextMsg(channelMsgs[i], nameMap))
    }

    return before, after
}

func toContextMsg(msg pipeline.Message, nameMap map[string]string) model.ContextMsg {
    sender := msg.SenderUID
    if nameMap != nil {
        if name, ok := nameMap[msg.SenderUID]; ok && name != "" {
            sender = name
        }
    }
    return model.ContextMsg{
        Sender:  sender,
        Content: truncateRunes(msg.Content, 200),
        SentAt:  msg.SendTime,
    }
}
```

**关键点**：`buildChannelMessageMap` 在 `buildCitations()` 入口调用一次，避免对每个 citation 重复遍历 allMessages。

---

### 改动 5：`pipeline.Message` 增加 `ChannelType` 字段

**文件**：`internal/pipeline/fetch.go`

`Message` struct（当前 `fetch.go` L24-35）新增 `ChannelType` 字段：

```go
type Message struct {
    MessageSeq    int64  `json:"message_seq"`
    SenderUID     string `json:"sender_uid"`
    SenderName    string `json:"sender_name"`
    ChannelID     string `json:"channel_id"`
    ChannelType   int    `json:"channel_type"`   // 新增
    Timestamp     int64  `json:"timestamp"`
    SendTime      string `json:"send_time"`
    Content       string `json:"content"`
    SourceName    string `json:"source_name"`
    CitationIndex int    `json:"citation_index"`
    IsTargetUser  bool   `json:"is_target_user"`
}
```

**赋值点**：`ResolveAndFetchMessagesForPersonal()`（`fetch.go` L525-527），在 `msgs[i].SourceName = ch.ChannelName` 后追加：

```go
for i := range msgs {
    msgs[i].SourceName   = ch.ChannelName
    msgs[i].ChannelType  = ch.ChannelType   // 新增
}
```

---

### 改动 6：调用点变更 -- `personal_processor.go`

**文件**：`internal/worker/personal_processor.go`

#### 6a. `batchResolveUserNames` 输入改为全量消息

当前（L177）：

```go
nameMap := p.batchResolveUserNames(userMessages)
```

改为：

```go
nameMap := p.batchResolveUserNames(messages)
```

**原因**：上下文消息的 sender 可能不在 `userMessages` 中，需要全量解析才能正确显示名称。

#### 6b. `buildCitations` 调用增加 `messages` 参数

当前（L251）：

```go
citations := buildCitations(finalContent, userMessages, nameMap)
```

改为：

```go
citations := buildCitations(finalContent, userMessages, messages, nameMap)
```

#### 6c. 不再需要追加 `collapseGlobalDuplicates` 调用

`[v3-FIX]` SUGGESTION-1：全局去重已合并进 `dedupCitations()`，调用点无需额外调用。

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

### 改动 7：CitationText 连续 citation 合并显示

**文件**：`packages/dmworkbase/src/Pages/Summary/components/CitationText.tsx`

解析逻辑增强：检测连续的 `[n][n+1][n+2]...` 序列（基于 CitationIndex 数字连续），渲染为单个合并 badge：

- 2-3 个连续：显示 `[30-32]`
- 4 个以上连续：显示 `[30-32 等 N 条]`
- 非连续的正常显示各自 badge

**注意**：这是纯展示层逻辑，后端文本不变。前端正则仍然匹配 `\[(\d+)\]`，只是渲染时检测相邻 match 的 index 连续性。

### 改动 8：CitationBadge 支持 group 模式

**文件**：`packages/dmworkbase/src/Pages/Summary/components/CitationBadge.tsx`

新增 `CitationGroupBadge` 组件：

- 接收一组 citations（合并后的）
- Popover 内展示消息列表：
  - `context_before` 消息：浅灰色/斜体，标注"上下文"
  - 被引用消息：正常颜色，左侧蓝色竖线标记
  - `context_after` 消息：浅灰色/斜体
- 底部"跳转到原文"使用第一条被引用消息的 `channel_id` + `channel_type` + `message_seq`

### 改动 9：修复 ChannelType 硬编码

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

| 阶段 | 改动 | 内容 | 涉及文件 | 涉及测试修改 | 可独立部署 |
|------|------|------|----------|-------------|-----------|
| P0 | 前端正则修复 | 修复 `(?<!\])` 导致连续 citation 只渲染第一个的 bug | `CitationText.tsx` | 前端组件测试（如有） | ✅ 前端独立 |
| P1 | 改动 1 | 全局去重逻辑合并进 `dedupCitations()`（**注意 early return 位置**） | `citation.go` | `citation_test.go`：① 新增 `TestDedupCitations_GlobalDuplicate*` 系列测试；② **修改** `TestDedupCitations_MixedDuplicatesAndUnique`（L179）断言 | ✅ 后端独立 |
| P2 | 改动 2-6 | Citation 加上下文 + ChannelType 常量 + Message.ChannelType 字段 + findContext 按 channel 分组 + nameMap 全量解析 + rune 截断 | `model.go`, `citation.go`, `fetch.go`, `personal_processor.go` | `citation_test.go`：**必须修改 4 个现有测试**（见下方 ISSUE-2 测试清单） | ✅ 后端独立 |
| P3 | 改动 7-9 | 前端合并展示 + group badge + channel_type 修复 | `CitationText.tsx`, `CitationBadge.tsx` | 前端组件测试 | 需 P0 + P2 先上线 |

P0 是现有 bug，建议立即修复。P1 可以今晚直接部署。P2 后端改动约 150 行（含 Message struct + nameMap 修改 + findContext + 测试修改），可以独立上线。P3 前端改动等 Ploy 排期。

---

## `[v3-FIX]` P1 测试修改清单

### P1 受影响的现有测试

全局去重合并进 `dedupCitations()` 后，以下现有测试断言需更新：

| 测试函数 | 行号 | 影响 | 修改说明 |
|----------|------|------|----------|
| `TestDedupCitations_MixedDuplicatesAndUnique` | L179 | remap `[3]→[2]` 后文本有两个 `[2]`，全局去重删除第二个 | 更新 expectedText 断言：第二个 `[2]` 被移除 |

---

## `[v3-FIX]` ISSUE-2：P2 测试修改清单

`buildCitations()` 签名从 3 参数改为 4 参数后，`citation_test.go` 中以下 4 个测试必须同步修改：

| 测试函数 | 当前行号 | 当前调用 | 修改后调用 |
|----------|---------|---------|-----------|
| `TestBuildCitations_WithNameMap` | L55 | `buildCitations(text, messages, nameMap)` | `buildCitations(text, messages, messages, nameMap)` |
| `TestBuildCitations_NilNameMap` | L77 | `buildCitations(text, messages, nil)` | `buildCitations(text, messages, messages, nil)` |
| `TestBuildCitations_NoCitations` | L92 | `buildCitations("no citations here", messages, nameMap)` | `buildCitations("no citations here", messages, messages, nameMap)` |
| `TestBuildCitations_ContentTruncation` | L106 | `buildCitations("[1] long message", messages, nil)` | `buildCitations("[1] long message", messages, messages, nil)` |

**注意**：在这 4 个已有测试中，`allMessages` 直接传 `messages` 即可（测试数据中消息本身就是全量的）。上下文填充的正确性由 P2 阶段新增的 `TestFindContext_*` 系列测试覆盖。

**额外修改 -- ContentTruncation 测试**（`[v3-FIX]` SUGGESTION-2）：

`TestBuildCitations_ContentTruncation`（L98-113）当前用 300 个 ASCII `x` 测试截断，断言 `len(citations[0].Content) == 203`。改为 `[]rune` 截断后，该断言改为基于 rune 长度：

```go
func TestBuildCitations_ContentTruncation(t *testing.T) {
    // 用中文字符测试 rune 截断
    longContent := strings.Repeat("测", 300) // 300 个中文字符，UTF-8 = 900 bytes
    messages := []pipeline.Message{
        {CitationIndex: 1, SenderUID: "uid_a", Content: longContent, SendTime: "2025-01-01T10:00:00Z", ChannelID: "ch1"},
    }
    citations := buildCitations("[1] long message", messages, messages, nil)
    if len(citations) != 1 {
        t.Fatalf("expected 1 citation, got %d", len(citations))
    }
    runes := []rune(citations[0].Content)
    // 200 runes + "..." (3 runes) = 203 runes
    if len(runes) != 203 {
        t.Errorf("expected 203 runes, got %d", len(runes))
    }
}
```

---

## `[v3-FIX]` SUGGESTION-3：新增单元测试清单

### P1 阶段：全局去重测试（在 `citation_test.go` 中新增）

由于全局去重已合并进 `dedupCitations()`，以下测试通过 `dedupCitations()` 入口验证：

```go
// 1. 同 marker 不同段落位置 -- 保留首次，删除后续
func TestDedupCitations_GlobalDuplicate_SameMarkerDifferentParagraphs(t *testing.T) {
    text := "第一段提到 [1] 很重要。\n\n第二段又引用了 [1] 作为佐证。"
    citations := []model.Citation{
        {Index: 1, Sender: "Alice", Content: "Hello"},
    }
    newText, newCitations := dedupCitations(text, citations)

    // [1] 应只出现一次
    count := strings.Count(newText, "[1]")
    if count != 1 {
        t.Errorf("expected [1] to appear once, got %d times in: %q", count, newText)
    }
    if len(newCitations) != 1 {
        t.Errorf("expected 1 citation, got %d", len(newCitations))
    }
}

// 2. 多 marker 混合 -- 各 marker 分别只保留首次
func TestDedupCitations_GlobalDuplicate_MultipleMarkers(t *testing.T) {
    text := "讨论 [1] 和 [2] 的内容。后面再次提到 [1] 和 [2]。"
    citations := []model.Citation{
        {Index: 1, Sender: "Alice", Content: "msg1"},
        {Index: 2, Sender: "Bob", Content: "msg2"},
    }
    newText, _ := dedupCitations(text, citations)

    if strings.Count(newText, "[1]") != 1 {
        t.Errorf("[1] count != 1 in: %q", newText)
    }
    if strings.Count(newText, "[2]") != 1 {
        t.Errorf("[2] count != 1 in: %q", newText)
    }
}

// 3. 删除 marker 后的空行清理
func TestDedupCitations_GlobalDuplicate_EmptyLineCleanup(t *testing.T) {
    // 删除 [1] 后第二行变空行，应被清理
    text := "第一段 [1] 很重要。\n[1]\n第三段继续。"
    citations := []model.Citation{
        {Index: 1, Sender: "Alice", Content: "Hello"},
    }
    newText, _ := dedupCitations(text, citations)

    if strings.Contains(newText, "\n\n") {
        t.Errorf("empty line not cleaned: %q", newText)
    }
    if strings.Count(newText, "[1]") != 1 {
        t.Errorf("[1] should appear once in: %q", newText)
    }
}

// 4. 无重复时文本不变
func TestDedupCitations_GlobalDuplicate_NoDuplicates(t *testing.T) {
    text := "引用 [1] 和 [2] 各出现一次"
    citations := []model.Citation{
        {Index: 1, Sender: "Alice", Content: "msg1"},
        {Index: 2, Sender: "Bob", Content: "msg2"},
    }
    newText, _ := dedupCitations(text, citations)
    if newText != text {
        t.Errorf("text should not change, got: %q", newText)
    }
}
```

### P2 阶段：上下文查找测试（在 `citation_test.go` 中新增）

```go
// 5. findContext 在同 channel 内正确取前后消息
func TestFindContext_SameChannel(t *testing.T) {
    allMessages := []pipeline.Message{
        {MessageSeq: 1, ChannelID: "ch1", SenderUID: "u1", Content: "msg1", SendTime: "10:00"},
        {MessageSeq: 2, ChannelID: "ch1", SenderUID: "u2", Content: "msg2", SendTime: "10:01"},
        {MessageSeq: 3, ChannelID: "ch1", SenderUID: "u1", Content: "msg3", SendTime: "10:02"}, // target
        {MessageSeq: 4, ChannelID: "ch1", SenderUID: "u3", Content: "msg4", SendTime: "10:03"},
        {MessageSeq: 5, ChannelID: "ch1", SenderUID: "u2", Content: "msg5", SendTime: "10:04"},
    }
    target := allMessages[2] // msg3
    channelMap := buildChannelMessageMap(allMessages)
    before, after := findContext(target, channelMap, nil, 2)

    if len(before) != 2 {
        t.Fatalf("expected 2 before, got %d", len(before))
    }
    if before[0].Content != "msg1" || before[1].Content != "msg2" {
        t.Errorf("unexpected before: %+v", before)
    }
    if len(after) != 2 {
        t.Fatalf("expected 2 after, got %d", len(after))
    }
    if after[0].Content != "msg4" || after[1].Content != "msg5" {
        t.Errorf("unexpected after: %+v", after)
    }
}

// 6. findContext 不跨 channel 取消息（ISSUE-3 核心验证）
func TestFindContext_CrossChannelIsolation(t *testing.T) {
    allMessages := []pipeline.Message{
        {MessageSeq: 1, ChannelID: "ch1", SenderUID: "u1", Content: "ch1-msg1", SendTime: "10:00"},
        {MessageSeq: 2, ChannelID: "ch2", SenderUID: "u2", Content: "ch2-msg1", SendTime: "10:01"},
        {MessageSeq: 3, ChannelID: "ch1", SenderUID: "u1", Content: "ch1-msg2", SendTime: "10:02"}, // target
        {MessageSeq: 4, ChannelID: "ch2", SenderUID: "u3", Content: "ch2-msg2", SendTime: "10:03"},
        {MessageSeq: 5, ChannelID: "ch1", SenderUID: "u2", Content: "ch1-msg3", SendTime: "10:04"},
    }
    target := allMessages[2] // ch1-msg2
    channelMap := buildChannelMessageMap(allMessages)
    before, after := findContext(target, channelMap, nil, 2)

    // 只应包含 ch1 的消息
    if len(before) != 1 { // ch1 中 target 前只有 ch1-msg1
        t.Fatalf("expected 1 before (same channel), got %d", len(before))
    }
    if before[0].Content != "ch1-msg1" {
        t.Errorf("before[0] should be ch1-msg1, got %q", before[0].Content)
    }
    if len(after) != 1 { // ch1 中 target 后只有 ch1-msg3
        t.Fatalf("expected 1 after (same channel), got %d", len(after))
    }
    if after[0].Content != "ch1-msg3" {
        t.Errorf("after[0] should be ch1-msg3, got %q", after[0].Content)
    }
}

// 7. findContext target 在列表首尾时不越界
func TestFindContext_BoundaryPositions(t *testing.T) {
    allMessages := []pipeline.Message{
        {MessageSeq: 1, ChannelID: "ch1", SenderUID: "u1", Content: "first", SendTime: "10:00"},
        {MessageSeq: 2, ChannelID: "ch1", SenderUID: "u2", Content: "second", SendTime: "10:01"},
    }
    channelMap := buildChannelMessageMap(allMessages)

    // target 是第一条消息
    before, after := findContext(allMessages[0], channelMap, nil, 2)
    if len(before) != 0 {
        t.Errorf("expected 0 before for first msg, got %d", len(before))
    }
    if len(after) != 1 {
        t.Errorf("expected 1 after for first msg, got %d", len(after))
    }

    // target 是最后一条消息
    before, after = findContext(allMessages[1], channelMap, nil, 2)
    if len(before) != 1 {
        t.Errorf("expected 1 before for last msg, got %d", len(before))
    }
    if len(after) != 0 {
        t.Errorf("expected 0 after for last msg, got %d", len(after))
    }
}

// 8. truncateRunes 中文截断正确性
func TestTruncateRunes(t *testing.T) {
    cn := strings.Repeat("中", 250)
    result := truncateRunes(cn, 200)
    runes := []rune(result)
    if len(runes) != 203 { // 200 + "..."
        t.Errorf("expected 203 runes, got %d", len(runes))
    }

    short := "短文本"
    if truncateRunes(short, 200) != short {
        t.Errorf("short text should not be truncated")
    }
}
```

---

## 不做的事

- **不改文本中的 citation 格式** -- 不引入 `[n-m]` 范围语法
- **不新增 DB 表或列** -- 复用现有 `citations_json` TEXT 字段
- **不新增 `CitationGroup` 数据结构** -- 扩展现有 `Citation` 结构即可
- **不改 LLM prompt** -- 现有 prompt 已有防重复指令，后端兜底去重

---

## Review Checklist

| ID | 类型 | 描述 | 修正状态 | 体现位置 |
|----|------|------|---------|---------|
| ISSUE-1 | Bug | `emptyLineRe` 正则 `\s` 跨行贪婪匹配 | ✅ 已修正 | 改动 1：正则改为 `^[ \t]*$\n` |
| ISSUE-2 | Breaking Change | `buildCitations()` 4 参数破坏 4 个测试 | ✅ 已修正 | 改动 3 + 实施步骤表 P2 列出 4 个测试修改 |
| ISSUE-3 | Bug | `findContext()` 跨 channel 取消息 | ✅ 已修正 | 改动 4：`buildChannelMessageMap` + 组内查找 |
| ISSUE-4 | 事实 | 跨仓库验证 | -- 不影响方案 | N/A |
| ISSUE-5 | 命名 | 变量命名规范 | -- 实施时统一 | N/A |
| ISSUE-6 | 性能 | nameMap 性能 | -- 消息量级不大 | N/A |
| ISSUE-7 | 缺失 | ChannelType 枚举缺常量 | ✅ 已修正 | 改动 2：`model.go` 新增 `ChannelTypeDM/ChannelTypeGroup` |
| SUGGESTION-1 | 重构 | `collapseGlobalDuplicates` 合并进 `dedupCitations` | ✅ 已采纳 | 改动 1：合并进 `dedupCitations()` 末尾 |
| SUGGESTION-2 | Bug | content 字节截断破坏 UTF-8 | ✅ 已采纳 | 改动 3：`truncateRunes()` 函数 |
| SUGGESTION-3 | 测试 | 全局去重缺单元测试 | ✅ 已采纳 | 新增 8 个测试用例（4 个去重 + 3 个上下文 + 1 个截断） |
| 自审-A | 死代码 | `dedupCitations()` early return 导致全局去重不执行 | ✅ 已修正 | 改动 1：全局去重移到 early return 之前执行 |
| 自审-B | 测试破坏 | `TestDedupCitations_MixedDuplicatesAndUnique` P1 破坏 | ✅ 已修正 | P1 测试修改清单已列入 |
