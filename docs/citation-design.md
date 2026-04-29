# 智能总结 — Citation 引用溯源功能技术设计文档

> 版本：v1.1 | 日期：2026-04-27 | 修订：修正文件路径 + 覆盖 BY_PERSON 模式 + Markdown 兼容方案

---

## 一、背景

智能总结生成的结论目前是"黑箱"——用户看到的是总结结果，但不知道这个结论来自哪条消息。类似企业微信的 Citation 功能，让每条总结结论都能关联到原始消息，增强可信度和可追溯性。

---

## 二、方案概述

**三层改动**：

```
后端 Pipeline                前端渲染
─────────────────────────────────────
消息编号 → Map Prompt      引用标记解析
       ↓                       ↓
   [n] 标注输出          remark 插件识别
       ↓                       ↓
  存 citations JSON       CitationBadge 气泡
```

---

## 三、后端实现

### 3.1 消息编号（Fetch 阶段）

在消息进入 pipeline 前，赋全局 1-based `CitationIndex`：

**文件**：`internal/pipeline/fetch.go`

```go
type Message struct {
    // 原有字段...
    CitationIndex int    // 新增：全局唯一编号，1-based
}

// 在 FetchMessages() 返回前统一编号
for i := range messages {
    messages[i].CitationIndex = i + 1
}
```

### 3.2 Map Prompt 改造

**文件**：`internal/service/llm.go`

输入消息格式变化：

```
原来：
[张三] 14:30: 今天完成了登录模块

改为：
[1][张三] 14:30: 今天完成了登录模块
[2][李四] 14:32: 测试通过，准备上线
```

Map Prompt 加指令：

```
你是信息提取助手。请从以下消息中提取与主题相关的要点。

**重要规则**：
1. 每个要点末尾必须标注来源编号，格式为 [n] 或 [n1][n2]
2. 多条消息支持同一要点时，列出所有编号：[1][3][5]
3. 只引用真实存在的编号，不要捏造
4. 如果某条消息与主题无关，忽略即可

示例输出：
- 登录模块已完成开发 [1]
- 测试通过，准备上线 [2]
- 性能优化讨论中 [3][4]
```

### 3.3 Reduce Prompt 改造

Reduce Prompt 加指令保留引用标记：

```
**重要规则**：
- 保留所有 [n] 引用标记，不要删除或修改
- 合并相同要点时，合并其引用：[1][3] + [2][3] → [1][2][3]
```

### 3.4 引用解析与存储

**文件**：`internal/model/model.go`，`internal/worker/processor.go`（BY_GROUP），`internal/worker/personal_processor.go`（BY_PERSON）

```go
type Citation struct {
    Index   int    `json:"index"`    // 消息编号（全局唯一）
    Sender  string `json:"sender"`   // 发送者名字
    Content string `json:"content"`  // 消息原文（截取前200字）
    SentAt  string `json:"sent_at"`  // 发送时间
    Source  string `json:"source"`   // 来源频道名
}

// 追加到 model.go 中的 SummaryResult 结构体
type SummaryResult struct {
    Content   string     `json:"content"`   // 带 [n] 标记的总结文本
    Citations []Citation `json:"citations"` // 引用映射表
    // 原有字段...
}
```

**解析逻辑**（Reduce 完成后）：

```go
// 从结果文本中提取所有 [n] 编号
// 使用负向前瞻/后顾，避免误匹配 Markdown 链接引用 [text][1]
func extractCitationIndexes(text string) []int {
    re := regexp.MustCompile(`(?:^|[^\]])\[(\d+)\](?:[^(]|$)`)
    // ...
}

// 构建引用映射表（只包含被实际引用的消息）
// 调用位置：processor.go（BY_GROUP）+ personal_processor.go（BY_PERSON，每人独立调用）
func buildCitations(text string, messages []Message) []Citation {
    indexes := extractCitationIndexes(text)
    indexSet := toSet(indexes)

    var citations []Citation
    for _, msg := range messages {
        if indexSet[msg.CitationIndex] {
            citations = append(citations, Citation{
                Index:   msg.CitationIndex,
                Sender:  msg.SenderName,
                Content: truncate(msg.Content, 200),
                SentAt:  msg.Timestamp.Format(time.RFC3339),
                Source:  msg.SourceName,
            })
        }
    }
    return citations
}
```

### 3.5 BY_PERSON 模式适配

**BY_PERSON 模式下的 Citation 策略**：每个参与者独立生成报告，需要隔离编号空间避免冲突。

```go
// personal_processor.go 中：为每个参与者独立分配编号，从 1 开始
func assignPersonalCitationIndexes(messages []Message) []Message {
    for i := range messages {
        messages[i].CitationIndex = i + 1  // 参与者内部编号，不跨人
    }
    return messages
}

// PersonalResult 中存独立的 citations 列表
type PersonalResult struct {
    Content   string     `json:"content"`
    Citations []Citation `json:"citations"` // 仅该参与者的引用
    // 原有字段...
}
```

前端展示时，每个参与者报告独立使用自己的 `citations`，**互不干扰**。

### 3.6 API 返回格式

`GET /api/v1/summaries/:id/result` 返回（BY_GROUP 模式）：

```json
{
  "content": "登录模块已完成 [1]，测试通过准备上线 [2]，性能优化进行中 [3][4]",
  "citations": [
    {
      "index": 1,
      "sender": "张三",
      "content": "今天完成了登录模块开发",
      "sent_at": "2026-04-27T14:30:00Z",
      "source": "开发群"
    },
    {
      "index": 2,
      "sender": "李四",
      "content": "测试通过，准备上线",
      "sent_at": "2026-04-27T14:32:00Z",
      "source": "开发群"
    }
  ]
}
```

`GET /api/v1/summaries/:id/members` 每个成员追加 `citations` 字段（BY_PERSON 模式）：

```json
{
  "members": [
    {
      "user_id": "xxx",
      "user_name": "张三",
      "content": "本周完成了登录模块 [1]",
      "citations": [
        { "index": 1, "sender": "张三", "content": "今天完成了登录模块", "sent_at": "...", "source": "开发群" }
      ]
    }
  ]
}
```

---

## 四、前端实现

### 4.1 CitationText 组件

**新文件**：`packages/dmworkbase/src/Pages/Summary/components/CitationText.tsx`

**不能用正则直接拆分**——会丢失 Markdown 渲染能力。改用 **remark 插件**方案：在 Markdown AST 层面识别 `[n]` 并替换为自定义节点，保留其他 Markdown 语法完整性。

```tsx
import ReactMarkdown from 'react-markdown';
import { visit } from 'unist-util-visit';

// remark 插件：将文本中的 [n] 标记转换为自定义 AST 节点
function remarkCitation() {
    return (tree: any) => {
        visit(tree, 'text', (node, index, parent) => {
            // 匹配 [数字]，且不在 Markdown 链接上下文内（负向前瞻/后顾）
            const regex = /(?<!\])\[(\d+)\](?!\()/g;
            const parts = [];
            let last = 0, match;
            while ((match = regex.exec(node.value)) !== null) {
                if (match.index > last) {
                    parts.push({ type: 'text', value: node.value.slice(last, match.index) });
                }
                parts.push({ type: 'citation', data: { index: parseInt(match[1]) } });
                last = match.index + match[0].length;
            }
            if (parts.length && parent) {
                if (last < node.value.length) parts.push({ type: 'text', value: node.value.slice(last) });
                parent.children.splice(index, 1, ...parts);
            }
        });
    };
}

function CitationText({ content, citations }: Props) {
    return (
        <ReactMarkdown
            remarkPlugins={[remarkCitation]}
            components={{
                citation: ({ node }: any) => (
                    <CitationBadge index={node.data.index} citations={citations} />
                ),
            }}
        >
            {content}
        </ReactMarkdown>
    );
}
```

### 4.2 CitationBadge 组件

**新文件**：`packages/dmworkbase/src/Pages/Summary/components/CitationBadge.tsx`

```tsx
// 渲染小角标气泡，点击展开 Popover 显示原始消息
function CitationBadge({ index, citations }: Props) {
    const citation = citations.find(c => c.index === index);

    return (
        <Popover
            trigger="click"
            content={
                <div className="citation-popover">
                    <div className="citation-sender">{citation?.sender}</div>
                    <div className="citation-time">{formatTime(citation?.sent_at)}</div>
                    <div className="citation-source">{citation?.source}</div>
                    <div className="citation-content">{citation?.content}</div>
                </div>
            }
        >
            <sup className="citation-badge">[{index}]</sup>
        </Popover>
    );
}
```

**样式**：
```css
.citation-badge {
    display: inline-flex;
    background: #1677ff22;
    color: #1677ff;
    border-radius: 4px;
    padding: 0 4px;
    font-size: 11px;
    cursor: pointer;
    margin-left: 2px;
}
.citation-badge:hover {
    background: #1677ff33;
}
```

### 4.3 BY_PERSON 模式接入

**文件**：`SummaryDetailPage.tsx` 的参与者报告区块

```tsx
// 每个参与者报告独立使用自己的 citations
{members.map(member => (
    <CollapsiblePanel key={member.user_id} title={member.user_name}>
        <CitationText
            content={member.content}
            citations={member.citations || []}  // 参与者独立 citations
        />
    </CollapsiblePanel>
))}
```

### 4.4 BY_GROUP 模式接入

**文件**：`SummaryDetailPage.tsx`

```tsx
// 原来：直接渲染 Markdown
<SummaryContent content={result.content} />

// 改为：用 CitationText 渲染（内部处理 [n] 标记 + 保留 Markdown）
<CitationText content={result.content} citations={result.citations || []} />
```

**降级处理**：`citations` 为空或全部无效时，直接渲染纯文本 Markdown，不显示任何气泡。

---

## 五、风险与降级

### 5.1 LLM 输出格式稳定性（最大风险）

| 场景 | 处理方式 |
|------|---------|
| LLM 遗漏 `[n]` | 直接渲染无引用的纯文本，不报错 |
| LLM 编造不存在的编号 | 前端查不到对应 citation，气泡显示"引用不存在" |
| LLM 把 `[n]` 改写为其他格式 | 正则无法匹配，降级为纯文本 |
| LLM 正常输出 | 完整 citation 体验 |

**缓解措施**：
- Prompt 中加示例（few-shot），稳定输出格式
- 后端解析时加容错：负向前瞻/后顾正则避免误匹配 Markdown 链接引用

### 5.2 Token 消耗

- 消息编号标注（`[n]`）每条消息增加约 4 个 token
- Map/Reduce 输出中的引用标记也增加约 4 个 token/条
- **实际增幅估算**：低消息量（< 100 条）< 5%；高消息量（500+ 条）可能达 **10-15%**
- 建议：高消息量场景可对编号标注做采样（每 N 条消息只标注一次），降低 token 消耗

---

## 六、工作量估算

| 模块 | 工作量 | 说明 |
|------|--------|------|
| 后端：消息编号 + Prompt 改造 | 0.5d | fetch.go + service/llm.go |
| 后端：引用解析 + 存储 | 0.5d | model.go + buildCitations()（BY_GROUP + BY_PERSON 分别适配） |
| 后端：API 返回 citations 字段 | 0.25d | task.go GetResult + personal handler |
| 前端：remark 插件 + CitationText | 1d | AST 级解析 + Markdown 保留（比正则方案复杂） |
| 前端：CitationBadge + Popover | 0.5d | UI + 样式 |
| 前端：接入 BY_GROUP 详情页 | 0.25d | 替换渲染组件 |
| 前端：接入 BY_PERSON 参与者报告 | 0.5d | 每人独立 citations |
| 联调 + 降级验证 | 0.5d | 覆盖 LLM 不稳定场景 |
| **合计（Phase 1，仅 BY_GROUP）** | **3d** | |
| **合计（完整，含 BY_PERSON）** | **4-4.5d** | |

---

## 七、改动文件清单

### 后端

```
internal/pipeline/fetch.go           ← 消息加 CitationIndex 编号
internal/service/llm.go              ← Map/Reduce prompt 加引用指令
internal/model/model.go              ← Citation 结构体 + citations 字段追加
internal/worker/processor.go         ← BY_GROUP：调用 buildCitations()
internal/worker/personal_processor.go ← BY_PERSON：独立编号 + buildCitations()
internal/api/handler/task.go         ← GetResult 返回 citations
```

### 前端

```
packages/dmworkbase/src/Pages/Summary/components/CitationText.tsx   ← 新增
packages/dmworkbase/src/Pages/Summary/components/CitationBadge.tsx  ← 新增
packages/dmworkbase/src/Pages/Summary/pages/SummaryDetailPage.tsx   ← 替换渲染（BY_GROUP + BY_PERSON）
packages/dmworkbase/src/Pages/Summary/api/summaryApi.ts             ← 类型扩展（citations 字段）
```

---

## 八、待确认

| # | 问题 | 影响 |
|---|------|------|
| 1 | 优先级确认：是否本阶段实现？ | 决定排期 |
| 2 | Phase 1 只做 BY_GROUP，BY_PERSON 放 Phase 2？ | 影响排期 |
| 3 | Popover 内是否需要"跳转到原始消息"功能？ | 影响前端复杂度 |
| 4 | 高消息量场景 token 消耗增加 10-15%，是否可接受？ | 影响是否做采样降耗 |
| 5 | 历史总结结果是否需要追加 citation？ | 影响是否需要 migration |
