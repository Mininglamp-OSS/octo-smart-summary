# 智能总结：文档与聊天混合来源开发计划

日期：2026-09-22  
状态：一期范围已确认；本文为开发计划，功能尚未实施。  
交付位置：`/home/mlamp/octo-smart-summary/docs/mixed-document-chat-summary-development-plan.md`

## 1. 目标与范围

### 1.1 交付目标

用户在现有智能总结入口，同时选择富文本文档和聊天会话，提交一项总结要求，得到一份综合总结。结果必须区分文档依据和聊天依据，并能通过引用核对来源。

典型场景：选择《产品方案》以及项目群最近七天的聊天，生成“方案目标、执行进展、已确认变更、待确认分歧和风险”的综合总结。

### 1.2 已确认的一期范围

| 项目 | 一期规则 |
|---|---|
| 创建人 | 当前登录用户本人 |
| 执行方式 | 手动发起，个人总结 |
| 来源类型 | 已支持的富文本文档，以及现有群聊、私聊、Thread 会话类型 |
| 数量 | 去重后文档不超过 10 篇，文档与会话合计不超过 30 个；入口如有更严格的既有限制，不擅自放宽 |
| 时间 | 聊天使用明确的时间范围；文档使用提交时抓取的版本与正文快照 |
| 结果 | 进入现有列表、详情、编辑和版本流程；分享沿用现有授权，不扩大原文访问权限 |
| 兼容 | 原有纯聊天和纯文档行为保持可用 |

来源集合按 `(source_type, source_id)` 去重；不能仅按 ID 去重。请求原始数组仍要设置有限长度，不能借去重允许无限请求体。

### 1.3 明确不做

1. 混合来源定时总结、周期性自动更新。
2. 混合来源多人协作、代他人创建或自动向来源会话发送结果。
3. 文档历史版本选择，以及 Sheet、Board、HTML、PPT 等额外文档类型。
4. 自动追踪聊天链接并扩大文档来源，或新增面向文档的自由问答与 Agent 自动选源能力。
5. 全量聊天正文快照、按用户设置来源权重，以及已有总结作为第三种新增混合来源。

最后一项不删除原有“引用总结”能力：旧流程保持原样；混合来源入口一期不叠加新的引用总结组合，遇到该组合应明确提示，而不是静默忽略。

## 2. 基线与已核实的限制

### 2.1 本地基线

下列是分析时实际读取的本地分支，不代表远程 PR 最新状态，也不代表已上线版本。

| 仓库 | 本地参考工作树 | 提交 |
|---|---|---|
| Summary 后端 | `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability` | `ef7f74f` |
| Web | `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig` | `f18a3c73` |
| 计划文档所在仓库 | `/home/mlamp/octo-smart-summary` | 当前主目录不作为文档功能实现基线 |

正式开工时，先确认文档接入最终落在哪个集成分支，再从包含这些能力的提交创建独立 `codex/` 工作树。不得直接在现有参考工作树或 Web 主目录覆盖用户改动。

### 2.2 不是只改选择器

| 层级 | 已核实的现状 | 混选风险 |
|---|---|---|
| Web 选择 | 选择会话会清空文档；选择文档会清空会话、参与者和时间 | UI 无法保留混选 |
| Web 历史恢复 | `adapter.ts` 在存在文档时清空恢复出的会话和时间 | 首次创建正常，刷新后丢来源 |
| 普通创建接口 | 检测到文档后，用准备好的文档数组替换全部来源 | 仅删除校验会导致聊天来源丢失 |
| Workbench 后端 | 契约拒绝混选；文档流程替换来源并清空时间 | 新旧入口行为不一致 |
| Workbench 权限 | 存在文档时直接将 `sourcesValid` 置为 true，不再执行会话校验 | 放开混选后可能跳过聊天权限检查 |
| Workbench 路由 | 含文档分支直接选择个人 Workflow，没有消费会话 `sourcesValid` 校验结果 | 仅恢复会话校验调用仍不足以阻止非法任务 |
| Worker | 纯文档和聊天二选一，混选显式报错；多处按整体 `documentMode` 分支 | 删除报错仍无法正确处理格式、过滤、提示词和引用 |

### 2.3 可以复用的基础

现有 `SummarySource` 已含来源类型、文档版本和哈希，`SummarySourceSnapshot` 已能保存文档正文，citation 已有 `document_id`、`document_version`、`document_chunk`。基础混选暂不计划新增表或迁移。

文档正文继续调用已有 Docs 接口；不保存用户 Token，不让异步 Worker 重新以用户 Token 拉取文档。若后续要求完整聊天证据冻结或新增持久化质量状态，须另行评估迁移，不隐含纳入本计划。

## 3. 用户行为与边界规则

### 3.1 主流程

1. 在现有总结入口添加文档和会话，两类选择互不清空。
2. 设置“聊天时间范围”，选择模板或输入总结要求。
3. 提交后完成权限检查及文档快照准备，创建一个个人总结任务。
4. 页面展示处理进度，完成后呈现一份综合结果和两类来源清单。
5. 点击引用查看对应证据；重新打开历史总结仍能识别其文档版本和聊天时间范围。

### 3.2 选择状态

| 操作 | 期望行为 |
|---|---|
| 已选聊天后添加文档 | 保留聊天与时间；若存在协作参与者，先提示并由用户确认移除，不自动丢弃 |
| 已选文档后添加聊天 | 保留文档；恢复或生成明确的聊天时间范围 |
| 在某类选择器清空选择 | 只清空该类来源；取消弹窗不修改任何来源 |
| 删除最后一个聊天 | 进入纯文档状态；请求不再携带聊天时间，UI 可保留未提交的时间偏好 |
| 删除最后一篇文档 | 进入纯聊天状态，恢复原纯聊天能力；不自动恢复已移除的参与者 |
| 修改来源、时间或模板 | 更新 scope 版本，使旧确认信息和旧预览失效 |
| 刷新、返回、恢复会话 | 文档、聊天、时间同时恢复，不依赖前端“修正”为单一来源 |
| 切换空间或退出登录 | 清除当前作用域缓存和进行中的选择请求，不能把上一个空间来源带入提交 |
| 混选能力关闭 | 保留已有选择并提示不可创建；允许用户主动调整为纯来源，不静默删除数据 |

### 3.3 时间与版本

文档版本是在本次提交读取该文档时捕获的版本；多篇文档并不构成跨服务原子快照。每篇来源分别记录自己的版本和捕获依据。

聊天沿用既有时间解析和边界语义，最终保存明确的起止时间；无显式选择时采用该入口现有默认值，并在 UI 中展示。不能仅因存在文档就清空时间，也不能让文档内容被聊天日期过滤。

同一任务重试或重新生成复用原文档快照，不自动抓取最新版。用户需要新版文档时重新创建任务。聊天仍按固定时间范围重新读取，因此消息删除、编辑和权限变化可能影响结果；一期不承诺聊天证据逐字可重复。

### 3.4 失败、空内容与安全

| 情况 | 一期处理 |
|---|---|
| 文档无权限、删除、空正文 | 创建失败，说明来源不可用；不创建少一篇文档的任务 |
| 会话无权限、空间不一致 | 拒绝请求或执行失败；不得退回自动选取其他会话 |
| 来源服务超时、限流 | 明确可重试错误；保留用户选择，重试遵守幂等 |
| 合法会话在时间范围内无消息 | 与权限失败区分；其他来源有内容时允许完成，正文明确说明该会话无可用消息 |
| 文档已截断、聊天达检索上限 | 显式展示覆盖范围限制，不能声称已分析全部原文 |
| 某个 Map 分块最终失败 | 混合任务一期按失败处理，不用其余分块冒充完整结果 |
| 全部来源均无可用内容 | 返回“没有可总结内容”，不调用模型虚构结论 |
| 分享用户不能访问原文 | 不新增原文或快照读取授权；沿用已授权摘要内容，原文入口仍受权限限制 |

空消息和覆盖限制优先通过现有结果正文及进度承载；如需要新增响应字段，应为可选、向后兼容字段，并先检查是否需要落库。不得把异常堆栈、Token、文档正文或私聊内容写入诊断日志。

## 4. 接口与执行设计

### 4.1 普通创建接口

复用 `POST /api/v1/summaries` 和现有 `sources` 数组。`SourceGroup=1`、`SourceThread=2`、`SourceDirect=3`、`SourceDocument=4`；不新增“混合”来源枚举。

示意请求，ID 为测试占位值：

```json
{
  "title": "产品方案与项目讨论总结",
  "topic": "总结方案目标、近期进展、已确认变更与待确认风险",
  "sources": [
    { "source_type": 4, "source_id": "doc_demo_001" },
    { "source_type": 1, "source_id": "group_demo_001" }
  ],
  "time_range": {
    "start": "2026-09-15T00:00:00+08:00",
    "end": "2026-09-22T00:00:00+08:00"
  }
}
```

登录身份、空间和幂等键继续通过现有请求机制传递。文档正文、哈希及版本由服务端读取与计算，不能信任客户端提交的快照信息。

### 4.2 Workbench 协议

继续使用已有 `summary_context.selected_channels`、`documents` 和 `time_range` 字段，允许三者同时存在。保留 `participants` 等既有字段，但在含文档任务中校验其不含额外参与者。

本期只接入用户明确选择来源后的个人 Workflow；不扩展自由 Agent 的文档检索、问答或混合预览工具链。已有预览状态在切换到混选后必须失效；历史纯聊天预览保持原行为。

协议以可选能力字段 `mixed_sources` 扩展（实现落地字段名；早期草案记为 `mixed_document_chat_sources`），字段缺失视为 false。现有 `document_sources` 不等价于支持混选。该字段跟踪 `SUMMARY_MIXED_SOURCES_ENABLED` 门禁（默认关），与 worker 执行器 PR 同步开启；前端据此 feature-detect 而非依赖 `contract_version`（契约版本不因该可选增量字段而升级）。若严格解码器验证表明新增可选字段即可兼容，则维持当前契约版本；否则同步协商版本，不能单侧升级。

### 4.3 创建与持久化

1. 规范化请求、按类型去重，检查能力开关、数量、个人范围和幂等信息。
2. 校验指定会话的空间与权限；只为文档调用 Docs，获取版本、正文及哈希。
3. 用已准备文档替换同键的文档来源，保留全部聊天来源；普通创建和 Workbench 复用同一合并逻辑。
4. 在既有事务内保存任务、完整来源、文档快照和幂等绑定；任一写入失败全部回滚。
5. 事务成功后触发个人 Worker，输出与请求一致的来源及聊天时间范围。

权限结果必须真正参与路由和创建决策：修改 `validateWorkspaceScope` 后，还需修改 `deriveWorkspaceRoute` 的含文档分支，并在 Workflow 创建边界保留最终防线。不能出现“权限检查返回 false，但仍创建任务”的情况。

网络抓取应在数据库写事务外进行。已有任务的幂等重放不能因为文档后来变化而覆盖原快照；同一幂等键和同一规范化用户请求返回原任务，变更来源或时间后复用键则返回冲突。需要核对并区分“用户请求指纹”与“抓取后的文档版本/正文指纹”，避免重试重新抓文档造成假冲突。

### 4.4 Worker 处理流程

```text
持久化的显式来源
    ├─ 文档来源 → 读取快照、校验哈希 → 按 token 预算分块 → 文档 Map
    └─ 聊天来源 → 按权限与时间取消息 → 聊天清洗和分块 → 聊天 Map
                                          ↓
                    统一证据编号贯穿两路 Map
                                          ↓
                  混合 Reduce：交叉归纳、指出分歧
                                          ↓
                  引用校验 → 保存结果 → 完成通知
```

建议新增内部来源分类和混合编排辅助文件，沿用现有 `pipeline.Message` 与 citation 表达，不进行与需求无关的大规模管线重写。

| 环节 | 必须满足 |
|---|---|
| 来源分流 | 文档 ID 不能进入聊天检索；聊天来源不能进入文档快照加载；`Derived` 来源不变成用户显式选择 |
| 聊天清洗 | 发送人、机器人、时间和主题裁剪仅作用于聊天，不误删文档片段 |
| 证据编号 | 两类证据在 Map 前分配唯一编号；单次执行期间稳定，不能各自从 1 开始 |
| 分块 | 沿用实际输入 token 预算，并计入提示词与引用标记开销；两类来源分别分块 |
| 汇总预算 | 超过 Reduce 输入预算时进行分层归纳，保留两类来源和引用；不能只保留先进入预算的一类 |
| 模型输出 | 同时核验共同点、进展及冲突；不能把讨论提议直接表述为已确认决策 |
| 流式输出 | 中间 Map 结果不当作最终全文；最终校验失败时标记失败，不持久化为完成结果 |
| 错误与取消 | 延续 Worker 重试、取消、租约与完成状态保护；混合路径不可绕开 |

模型提示词须说明：来源正文是待分析数据而非指令；有证据才输出事实；冲突同时给出两侧引用；捕获时间不等于文档内容的实际生效时间；无法判断新旧或权威时明确待确认。

### 4.5 引用与结果展示

文档引用保留 `document_id + document_version + document_chunk`；聊天引用保留会话类型、会话 ID 和消息坐标。内部证据键包含类型，避免相同 ID 或序号碰撞。

统一编号后，继续使用现有引用筛选和校验，补足混合结果的越界、重复、缺失及伪造编号检查。按正文文本去重时不能把“同样文字、不同来源”的证据合并丢失。

文档引用优先展示生成时的已授权证据摘录和版本信息；如果现有 Docs 跳转只能打开当前版本，应明确提示“当前文档可能已更新”，不能宣称可精准定位历史版本。不为此新增一个无权限保护的快照下载接口。

来源统计区分“聊天消息数”和“文档数/片段数”，不能把文档片段计成聊天消息误导用户。

## 5. 文件地图

下表使用分析基线中的绝对路径。正式实施时替换为新工作树的同名文件；标注“拟新增”的文件当前尚不存在。

### 5.1 后端：契约、创建和权限

| 文件 | 改动责任 |
|---|---|
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/api/handler/task.go` | 普通创建入口保留两类来源，接入统一检查；核对重试、重新生成和 origin 相关行为 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/api/handler/document_summary_create.go` | 只提取文档准备快照，取消错误的纯文档假设，保留正文限制及限流 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/api/handler/summary_workspace_contract.go` | 允许混合上下文、保留聊天时间、校验总数量与不支持的组合 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/api/handler/agent_summary_workspace.go` | 修正来源替换、历史/上下文时间清空、默认时间补全、会话权限跳过、路由未消费校验结果和能力响应 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/service/summary_workflow.go` | 统一领域校验、数量、合并后的来源持久化和幂等语义 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/service/snapshot_validator.go` | 审查文档与会话并存时是否完整执行权限和时间约束 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/config/config.go` | 拟新增默认关闭的混选准入配置；配置名在 PR1 冻结 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/model/model.go` | 核对现有来源、快照和引用字段；本期默认不改表 |

`origin_channel` 不是信息来源数组。一期含文档任务不新增 origin 自动推导或回发会话能力，继续保持个人行为；提交来源中有聊天不代表可以向该聊天自动发送总结。

### 5.2 后端：Worker 和模型提示词

| 文件 | 改动责任 |
|---|---|
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/worker/personal_processor.go` | 接入混合执行，拆开整体 documentMode 对清洗、格式化、Map/Reduce 和统计的影响 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/worker/document_sources.go` | 复用快照校验、文档分块和格式化，仅接收文档来源 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/worker/specified_sources.go` | 保证发给聊天检索的显式来源不含文档及 Derived 行 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/worker/processor.go` | 审计旧执行入口；无法证明不可达时转入公共混合编排，不能留下可达的混选拒绝路径 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/worker/citation.go` | 复用并补足跨类型证据去重、编号和坐标输出 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/service/llm.go` | 复用现有两类 Map；混合 Reduce 沿用限流、超时、模型回退、token 统计 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/worker/mixed_sources.go`（拟新增） | 来源分流、公共证据编排及混合辅助逻辑 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/service/mixed_summary_prompt.go`（拟新增） | 混合归纳提示词与调用封装，避免继续扩大既有大文件 |

### 5.3 前端：来源状态、协议和视图

| 文件 | 改动责任 |
|---|---|
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/features/summaryWorkbench/scope.ts` | 两类选择互不清空，时间和参与者按混合规则处理 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/features/summaryWorkbench/SummaryWorkbenchFeature.tsx` | 添加/删除来源、提示、时间入口及提交状态 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/bridge/summaryWorkbench/protocol.ts` | 能力字段与已有混合上下文类型契约 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/bridge/summaryWorkbench/adapter.ts` | 请求序列化、服务端响应解码、历史恢复不丢聊天和时间 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/bridge/summaryWorkbench/model.ts` | 两类 context item、文案状态和预览失效 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/bridge/summaryWorkbench/useSummaryWorkbench.ts` | scope 版本、进行中请求及旧响应处理 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/Service/SummaryWorkbenchService.ts` | 新能力传递与完整上下文提交，保留 Service 边界 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/features/summaryWorkbench/availability.ts` | 混选能力缺失/关闭时安全降级，按空间隔离缓存 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/features/summaryWorkbench/sessionStorage.ts` | 回归草稿、空间切换和版本兼容，必要时修改 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/pages/SummaryCreatePage.tsx` | 对仍启用的旧入口做最小兼容，不另建新业务入口 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/ui/SummaryWorkbench/index.tsx` | 来源展示与聊天时间说明；只渲染 props |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/components/CitationBadge.tsx` | 检查两类引用路由，必要时补版本及无权限提示 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/components/SummaryReferenceSidePanel.tsx` | 混合引用展示回归；不得新增原文授权 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/i18n/zh-CN.json` | 中文来源、时间、异常与覆盖提示 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary/src/i18n/en-US.json` | 对应英文文案 |

复用文档和聊天选择器，不重写 Docs 搜索。新增 UI 状态先写 Story，再接入业务。国际化实施前阅读 Web 仓库现行 i18n 指南；交互不应混入通用 `Components/` 中新增完整流程。

## 6. 工作包与 PR 拆分

总工作量初估 9–14 人日。估算以前述文档接入已可运行、无需数据库迁移为前提；包含下列开发与联调，不等于承诺日历交付日期。

### 6.1 PR1：后端契约、权限和创建

建议分支名：`codex/summary-mixed-source-contract`  
主责：后端；估算 2–3 人日。能力开关保持关闭。

1. 冻结混选能力、来源计数、时间、origin、错误与幂等契约，并补失败测试。
2. 改普通创建与 Workbench 契约，移除纯文档假设但保留个人/手动边界。
3. 抽取共同来源合并逻辑，同时校验文档和聊天权限；处理上下文默认时间与恢复。
4. 完成事务与幂等重放测试，包括 Docs 失败、并发提交及文档后续变化。
5. 输出接口示例和测试结果；证明开关关闭时新混选请求被后端拒绝。

完成标准：两类入口均能在测试中持久化完整来源及文档快照；不会跳过会话权限；重复提交不产生第二个任务。

### 6.2 PR2：混合 Worker 与引用

建议分支名：`codex/summary-mixed-source-worker`  
依赖 PR1 的契约，可基于 PR1 堆叠；主责：后端；估算 3–5 人日。

1. 拆分来源，保留聊天清洗边界、文档快照校验和 token 预算。
2. 统一证据编号，接入两类 Map 与混合/分层 Reduce。
3. 补冲突归纳提示词，以及引用、流式完成、取消、重试和失败处理。
4. 审计旧 Worker 入口与统计，确保混合任务不会被旧路径消费后报错。
5. 用固定证据和模拟模型通过确定性测试，再执行授权测试环境的真实模型质量检查。

完成标准：一个混合任务生成一份可追溯的结果；证据中存在有效两类信息时均可被引用；任何未恢复分块失败不会被标为完整成功。

### 6.3 PR3：Web 混选与结果交互

建议分支名：`codex/summary-mixed-source-web`  
依赖 PR1 契约冻结，可与 PR2 并行；主责：前端；估算 2–3 人日。

1. 先补选择、取消、清空、参与者确认、超限和时间状态的测试与 Story。
2. 改 scope、序列化与历史恢复，保证文档和聊天一直同时保留。
3. 接入混选能力门禁，兼容旧创建页并处理缓存、失效预览及请求竞态。
4. 核对引用面板、文档版本说明、来源计数和中英文提示。
5. 完成单元测试、类型检查、Story 视觉检查及 E2E 用例。

完成标准：创建、刷新、历史恢复、删除一类来源均不丢另一类；关闭能力或切换空间时不能提交不合法组合。

### 6.4 集成验收与发布准备

依赖三个 PR 均可联调；前后端与测试共同负责；估算 2–3 人日。

| 阶段 | 产出 | 放行条件 |
|---|---|---|
| M0 基线确认 | 最终后端/Web 提交、工作树、运行依赖 | 原纯文档和纯聊天可运行 |
| M1 契约完成 | PR1 与接口示例 | 权限、事务、幂等测试通过 |
| M2 管线完成 | PR2 与固定样例结果 | 混合生成和引用测试通过 |
| M3 交互完成 | PR3 与 E2E 结果 | 新旧入口与历史恢复通过 |
| M4 联调完成 | 验收记录、灰度与回退清单 | 关键用例全通过，已部署兼容 Worker |

如必须增加数据库迁移、历史版本定位接口或新的 Worker 协议，先记录原因、影响和新增人日，再调整计划；不在现有估算中静默扩展。

## 7. 验证计划

### 7.1 后端自动化

优先补在以下已有测试套件中：

| 测试文件或拟新增文件 | 覆盖点 |
|---|---|
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/api/handler/document_summary_create_test.go` | 两类来源保留、文档过滤、限额、权限与 Docs 失败 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/api/handler/summary_workspace_contract_test.go` | 混合上下文、时间与参与者边界 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/api/handler/agent_summary_workspace_source_update_test.go` | 来源变化、历史 scope、旧预览失效 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/api/handler/agent_summary_workspace_time_range_test.go` | 混选不清空时间、默认时间与纯文档回归 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/service/summary_workflow_test.go` | 事务、权限、数量、幂等与快照复用 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/worker/specified_sources_test.go` | 文档及 Derived 来源不进入聊天检索 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/worker/document_sources_test.go` | 快照加载、哈希、分块及原拒绝混选测试替换 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/worker/personal_citation_pipeline_test.go` | 引用编号与最终持久化 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/worker/mixed_sources_test.go`（拟新增） | 两路证据、类型碰撞、空会话、失败、取消与预算 |
| `/home/mlamp/octo-smart-summary/.codex-worktrees/summary-doc-capability/internal/service/mixed_summary_prompt_test.go`（拟新增） | 冲突、证据约束及防止来源正文变成指令 |

在后端实施工作树中运行。以下是计划命令，本次编写文档没有执行这些测试：

```bash
go test ./internal/api/handler ./internal/service ./internal/worker ./internal/citationtext
go test -race ./internal/service ./internal/worker
```

使用仓库要求的 Go 工具链及 CGO/native tokenizer 依赖。检查带构建标签和 MySQL 专用用例是否实际执行，不能把环境缺依赖或被跳过的权限/事务测试记为通过。SQLite 通过不能代替 MySQL 集成验证。

### 7.2 Web 自动化与视觉检查

扩展 scope、adapter、Service、Feature、旧创建页和引用组件现有测试；新增混选 E2E 场景。命令中的绝对路径需在开工后替换为新 Web 实施工作树，不能误在参考工作树修改代码。

```bash
pnpm --dir /home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary test
pnpm --dir /home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/packages/dmworksummary typecheck
pnpm --dir /home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig i18n:check
pnpm --dir /home/mlamp/octo-smart-summary/.codex-worktrees/summary-docs-no-appconfig/apps/web test:e2e e2e-kit/tests/summary
```

E2E 应先按现有配置准备浏览器、依赖和应用服务。Mock E2E 证明交互契约，不能代替真实 Docs/聊天权限联调。Story 验证浅色、深色、长标题、空态、加载、异常及窄屏状态；需要新 Story 时复用现有组件，不另做产品原型。

### 7.3 人工验收矩阵

| 编号 | 场景 | 通过标准 |
|---|---|---|
| A01 | 1 文档 + 1 群聊，显式时间范围 | 两类来源落库，结果引用与证据一致 |
| A02 | 多文档 + 群聊/私聊/Thread | 保留现有聊天类型边界，来源不碰撞、不自动扩大 |
| A03 | 纯聊天、纯文档 | 与原有主流程一致 |
| A04 | 先选文档/先选聊天、取消与清空 | 顺序不影响最终作用域，取消不修改选中项 |
| A05 | 文档 10/11 篇，合计 30/31 个 | 前后端在适用上限处一致允许或拒绝 |
| A06 | 相同 ID、不同来源类型；重复添加 | 不误去重；同类型同 ID 只保留一次 |
| A07 | 会话无权限、文档无权限、跨空间 | 不创建或执行未经授权的结果，不泄露其他空间来源 |
| A08 | 聊天范围内无消息 | 有其他证据时说明缺口；不编造聊天进展 |
| A09 | 长文档 + 大量聊天、截断 | 两类证据均有合理覆盖，明确限制，无非法引用 |
| A10 | 文档与聊天存在矛盾 | 表达分歧、双方引用及待确认项，不自动判定聊天覆盖文档 |
| A11 | 提交后编辑文档，再重试/重新生成 | 使用任务原文档版本；新建任务才读取新版 |
| A12 | 双击提交、网络重试、并发重放 | 同一用户请求只创建一个任务，来源与快照不被覆盖 |
| A13 | Docs 超时、Map/Reduce 失败、任务取消 | 状态准确，未完整生成不显示为成功，可按既有机制恢复 |
| A14 | 刷新/返回/历史恢复/切换空间 | 混合上下文完整，旧作用域响应不污染新空间 |
| A15 | 能力缺失/关闭、旧客户端、新旧 Worker | 禁止不兼容的新提交；已接受任务由兼容 Worker 执行 |
| A16 | 引用打开、文档更新、分享权限 | 类型、摘录和版本准确，无新增原文权限 |
| A17 | 个人任务完成与来源会话 | 不自动发群，不因 origin 推导变成多人或会话公开任务 |
| A18 | 已有预览后改成混选、附带参与者或引用总结 | 旧预览失效，不支持组合有明确提示，不静默丢数据 |

确定性测试固定模型响应，验证数据流和引用；真实模型验收至少准备“互补信息、相同事实、冲突信息、长内容、恶意来源指令”五类样例，每类执行 3 次并记录失败。不得仅凭一次主观读起来顺畅就认为质量通过。

## 8. 灰度、监控与回退

### 8.1 发布顺序

1. 部署兼容混选的 Worker，确认所有消费该队列的实例均已更新，或已完成明确的队列隔离。
2. 部署支持新契约的 API，混选新建开关仍关闭，完成健康检查和单一来源回归。
3. 部署 Web；通过能力响应控制混选交互，缺失字段默认关闭。
4. 在已确认权限的测试空间开放混选，执行 A01、A07、A11、A12、A14、A16、A17。
5. 观察生成、引用、耗时和费用数据后逐步放量，并记录实际镜像/提交与验收人。

仅由 API 宣告能力不能证明 Worker 已兼容。开关是“新任务准入”，不能让已接受的混合任务在执行中因开关关闭而突然不可处理。

### 8.2 观测与上线门槛

| 维度 | 应记录 |
|---|---|
| 请求 | 纯聊天/纯文档/混合模式、来源数量、创建成功率、权限拒绝与幂等重放 |
| 读取 | 文档抓取耗时、聊天检索耗时、快照失败、空消息和截断 |
| 执行 | Map/Reduce 耗时、调用次数、输入/输出 token、重试、取消与队列积压 |
| 结果 | 生成成功率、引用校验失败、来源覆盖限制及异常类别 |
| 保密 | 只记录内部任务标识和计数，不记录正文、标题、凭据或消息内容 |

上线必须满足：无已知权限绕过、无混合引用错配、无静默丢来源、无未兼容 Worker 消费风险。延迟、token 成本和普通失败率先对相同量级的单一来源样例建立基线，再在 M4 验收记录中填写具体阈值；阈值未填写不进入广泛放量。

### 8.3 回退规则

发生问题先关闭混合新建能力，保留纯来源入口。已创建混合任务仍由兼容 Worker 排空或按授权流程处理，不能立刻整体回滚为不认识混合来源的 Worker。

已有混合总结与引用必须继续可读。不要删除来源、快照或历史任务作为回退手段；如必须回滚 API/Worker 版本，先确认队列、运行中任务和历史读取兼容性。涉及删除或无法恢复的数据变更必须另行确认。

## 9. 完成定义与交接记录

### 9.1 一期完成定义

1. 本文确认的个人手动混选行为在 Workbench 和仍启用的旧入口均可用，纯来源流程无回归。
2. 两类来源完整保存、权限分别检查、时间和文档版本语义一致，幂等与失败行为符合计划。
3. 混合总结能交叉归纳，两类引用准确，覆盖不足和冲突不被隐藏。
4. 相关自动化、真实权限联调和人工验收通过，记录实际执行而不是只提供命令。
5. 发布/回退清单、观测阈值及运行版本齐备，能力开关可控，未完成项无隐瞒。

### 9.2 实施期间持续填写

| 项目 | 当前值 |
|---|---|
| 最终后端/Web 实施基线 | 开工前核实 |
| 开发负责人、验收负责人 | 待安排 |
| PR1 / PR2 / PR3 | 尚未创建 |
| 自动化与真实联调结果 | 尚未执行 |
| 实际部署版本、灰度空间、监控阈值 | 发布前填写 |

本文只交付计划，不代表已创建分支、提交 PR、实现功能、运行测试或部署服务。下一实施动作是核对集成基线并冻结 PR1 接口契约，再创建独立开发工作树。
