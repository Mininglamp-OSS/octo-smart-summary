# Citation 合并优化方案

## 问题

截图显示两个问题：

### 问题 1：重复 citation
- `[84]` 出现 3 次，`[89]` 出现 2 次
- 现有 `dedupCitations()` 按 `(sender, content)` 去重，但 LLM 生成时就引用了重复编号
- **根因**：prompt 已有"严格禁止重复引用"指令，但 LLM 不总是遵守；`dedupCitations()` 只合并**不同编号但内容相同**的 citation，不处理**同一编号在文本中出现多次**的情况

### 问题 2：单条 citation 无意义，需要合并+上下文
- 当前 citation 只存**单条消息**（1 个 sender + 1 条 content）
- 一堆 `[30][31][32][33][43][44][45]...` 散落在一个要点后面，点开每个只看到一条孤立消息
- **期望**：同一会话相邻的 citation 合并为一组，展示连续消息片段；非相邻的也应带上下文（前后各取 N 条）

---

## 方案

### 修改 1：后端 — citation 去重增强

**文件**：`internal/worker/citation.go`

在 `dedupCitations()` 之后新增 `collapseTextDuplicates(text)`：
- 扫描文本，把同一 `[n]` 在文本中出现多次的情况收敛为 1 次
- 已有的 `collapseConsecutiveMarkers()` 只处理**相邻**重复，需要扩展为**全局**同编号去重

```go
// collapseGlobalDuplicates removes all but the first occurrence of each [n] marker in text.
func collapseGlobalDuplicates(text string) string {
    seen := make(map[string]bool)
    return citationRe.ReplaceAllStringFunc(text, func(match string) string {
        if seen[match] {
            return ""
        }
        seen[match] = true
        return match
    })
}
```

调用点在 `personal_processor.go:248` 后追加：
```go
finalContent = collapseGlobalDuplicates(finalContent)
```

### 修改 2：后端 — citation 合并 + 上下文

**新增函数**：`internal/worker/citation.go` → `mergeCitationGroups()`

**逻辑**：
1. 对文本中每个要点（bullet point / 段落末尾）的 citation 列表，按 `channel_id` 分组
2. 同一 `channel_id` 内按 `message_seq` 排序
3. **相邻合并**：`message_seq` 连续（差值 ≤ 2）的合并为一个 citation group
4. **上下文补充**：每个 group 向前、向后各扩展 N 条消息（N=2），从原始 messages 中查找
5. 输出新的数据结构 `CitationGroup`

**新数据结构**：

```go
// model/model.go
type CitationGroup struct {
    Indexes    []int            `json:"indexes"`     // 被合并的原始 citation indexes
    Messages   []CitationMsg    `json:"messages"`    // 合并后的消息列表（含上下文）
    ChannelID  string           `json:"channel_id"`
    Source     string           `json:"source"`
}

type CitationMsg struct {
    Sender    string `json:"sender"`
    Content   string `json:"content"`
    SentAt    string `json:"sent_at"`
    IsCited   bool   `json:"is_cited"`   // true=被引用的消息, false=上下文消息
    Seq       int64  `json:"message_seq"`
}
```

**API 响应变更**：

在现有 `citations []Citation` 旁新增 `citation_groups []CitationGroup`：
- `citations` 保持不变（向后兼容）
- `citation_groups` 新增字段，前端优先使用

### 修改 3：前端 — CitationBadge 支持 group 展示

**文件**：`packages/dmworkbase/src/Pages/Summary/components/CitationBadge.tsx`

当检测到连续 citation（如 `[30][31][32]`）：
1. 合并为一个 badge 显示，如 `[30-32]` 或 `[30 等 3 条]`
2. 点击弹出 Popover 显示完整消息列表（含上下文灰色消息）
3. 上下文消息用浅灰色/斜体区分，被引用消息正常显示

**文件**：`packages/dmworkbase/src/Pages/Summary/components/CitationText.tsx`

解析逻辑增强：识别连续 `[n][n+1][n+2]` 模式，合并为单个 `CitationGroupBadge` 组件

---

## 文本中 citation 显示优化

**当前**：
```
多轮 tool call 测试：验证了每轮返回消息必须包含 text content 的要求 [77][78][81][84][81][84][89][97][89][101][102][121][123][124]
```

**优化后**：
```
多轮 tool call 测试：验证了每轮返回消息必须包含 text content 的要求 [77-78][81-89][97-102][121-124]
```

这个收敛在**后端 reduce 阶段**做：新增 `compactCitationRanges(text)` 函数，把连续编号收敛为范围格式。

```go
// compactCitationRanges replaces consecutive citation sequences like [30][31][32] with [30-32].
func compactCitationRanges(text string) string { ... }
```

---

## 实施步骤

| 步骤 | 内容 | 影响 |
|------|------|------|
| 1 | `collapseGlobalDuplicates()` 去重 | 后端，解决问题1 |
| 2 | `compactCitationRanges()` 范围收敛 | 后端，文本更整洁 |
| 3 | `CitationGroup` 数据结构 + `mergeCitationGroups()` | 后端，解决问题2 |
| 4 | API 响应加 `citation_groups` | 后端，向后兼容 |
| 5 | 前端 `CitationGroupBadge` 组件 | 前端，展示合并+上下文 |

步骤 1-2 可以独立部署，立即生效。步骤 3-5 需要前后端联调。

---

## 向后兼容

- `citations` 字段保留不动
- `citation_groups` 是新增字段，旧前端忽略即可
- 文本中 `[30-32]` 范围格式，前端需要支持解析（正则 `\[(\d+)-(\d+)\]`）
