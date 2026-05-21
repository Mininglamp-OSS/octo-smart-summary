# AGENTS.md

## 1. 项目背景

`octo-smart-summary` 是 OCTO 生态中的 LLM 会话总结服务（Go 1.25，Gin + GORM）。提供 API 与 Worker 双进程：API 处理用户请求与鉴权，Worker 异步执行 fetch → chunk → map/reduce LLM 总结流程，结果通过 WebSocket 实时推送。

## 2. 目录结构

| 路径 | 用途 |
|---|---|
| `cmd/summary-api`、`cmd/summary-worker` | 两个独立进程入口（Makefile `run-api` / `run-worker`） |
| `internal/api/router` | 路由装配（`SetupPublic` / `SetupInternal`） |
| `internal/api/handler` | HTTP handler，每个资源一文件（task/edit/personal/schedule/candidates/internal） |
| `internal/api/ws` | WebSocket Hub，按 user/task 维度广播 |
| `internal/auth`、`internal/middleware` | Token 解析、`StrictAuthMiddleware`、`StrictSpaceMiddleware` |
| `internal/config` | 纯 env 驱动配置，`config.Load()` |
| `internal/model` | GORM struct + 状态常量 + DTO |
| `internal/service` | 业务逻辑、`BizError`、命名解析、任务编号 |
| `internal/pipeline`、`internal/worker` | Worker 流水线与调度 |
| `internal/db` | DB 初始化 |
| `migrations/` | SQL 迁移（sql-migrate） |

## 3. 编码规范

- **HTTP 响应**：统一 `apiResponse{Code int, Message string, Data any}`，成功走 `ok(c, data)`，业务错误走 `bizErr(c, service.NewBizError(code, msg, httpStatus))`，框架级错误直接 `c.JSON(http.StatusBadRequest, apiResponse{Code: 40000, Message: err.Error()})`。
- **错误码约定**：`0` 成功；`40000` 参数错误；`40001` 校验失败；`40003/40004` 权限；`40005` 状态冲突；`40008` 资源不存在；`4010` 未认证；`50000` 内部错误。中文错误文案面向用户。
- **错误处理**：`if err != nil` 立即 `return`，handler 内 `log.Printf("[handler] xxx error: %v", err)`，禁止 panic，禁止吞掉 error。
- **命名**：handler 结构体 `XxxHandler` + 构造函数 `NewXxxHandler(deps...)`；导出方法对应一个路由（CreateXxx/ListXxx/GetXxx/UpdateXxx/DeleteXxx）；请求体 struct 用小写 `createSummaryReq` 私有类型。
- **配置**：所有配置通过 `internal/config/config.go` 的 `envStr/envInt/envBool/getEnvFloat`，必填项用 `config.ValidateRequired`。禁止散落 `os.Getenv`。
- **DB 操作**：多表写入用 `h.db.Transaction(func(tx *gorm.DB) error {...})`；软删用 `deleted_at IS NULL` 过滤；从 context 取身份用 `middleware.GetUserID(c)` / `middleware.GetSpaceID(c)`。
- **日志前缀**：`[handler]`、`[config]`、`[task]`、`[worker]` 等模块标签。

## 4. 路由规范

- **公网路由**（`SetupPublic` → `:8080`）：前缀 `/api/v1`，必须挂 `StrictAuthMiddleware(authResolver) + StrictSpaceMiddleware()`；WS 路由 `/ws`、`/ws/summaries` 同样鉴权；`/health` 无鉴权。
- **内部路由**（`SetupInternal` → `:8081`）：前缀 `/internal/`，**只允许 Worker 回调**（`/internal/task-event`、`/internal/worker-trigger`、`/internal/healthz`），不挂任何用户鉴权中间件，靠网络层隔离，**禁止暴露公网**。
- **路径命名**：资源集合用复数（`/summaries`、`/summary-schedules`）；动作/查询挂在 `/summaries/:id/<action>`（`/regenerate`、`/cancel`、`/accept`、`/respond`、`/submit`）；跨资源查询用 `summary-<noun>`（`/summary-infer`、`/summary-member-candidates`、`/summary-chat-candidates`、`/summary-templates`）。

## 5. Review Checklist

- [ ] 新路由是否加在正确的 router（`SetupPublic` 走 `/api/v1` + 鉴权；`SetupInternal` 走 `/internal/`）
- [ ] 涉及任务的 handler 是否调用 `authorizeTaskAccess` / `batchAuthorize`（creator / participant / group member 三重校验）
- [ ] 响应是否统一走 `ok`/`bizErr`/`apiResponse`，错误码是否复用现有码表
- [ ] 多表写入是否包在 `db.Transaction` 内，事务外才触发 worker / WS
- [ ] 用户输入是否做长度与上限校验（如 title/topic ≤1000 字符、sources ≤10、batch ≤50）
- [ ] 新增配置项是否走 `config.go` + `envXxx`，必填项是否加入 `ValidateRequired`
- [ ] 是否新增了 `log.Fatal`、`panic`、裸 `os.Getenv`、未关闭的 `resp.Body`
- [ ] 涉及 LLM/外部 HTTP 调用是否带超时（参考 `triggerClient`、`LLM_TIMEOUT`、`TOOL_CALL_TIMEOUT`）
- [ ] 是否新增/修改了表结构 → 必须配套 `migrations/` SQL
- [ ] `go vet ./...` 通过（`.golangci.yml` 当前仅启用 `govet`）

## 6. 禁止事项

- 禁止把 `/internal/*` 路由加到 `SetupPublic`，或把 `/api/v1/*` 加到 `SetupInternal`。
- 禁止在 handler 内绕过 `authorizeTaskAccess` 直接查 task（除 List/Batch 已实现自身权限过滤）。
- 禁止破坏 `apiResponse` 响应契约（前端依赖 `code/message/data`），尤其不要直接 `c.JSON(200, someStruct)`。
- 禁止改 `/api/v1` 已有路径或字段语义（含状态码常量 `model.Status*`、`model.Participant*`），属于对外 API。
- 禁止在 handler 同步执行重活；触发 worker 用 `go h.triggerWorker(...)` 在事务提交后。
- 禁止硬删除 `summary_task`（用 `status=-1` + `deleted_at`）。
- 禁止改 `go.mod` 主依赖大版本（gin/gorm）而不验证 migration 与 handler 测试。

## 7. 测试命令

```bash
make build         # go build ./...
make test          # go test -v -count=1 ./...
go vet ./...       # 与 CI lint 一致
make run-api       # 本地起 API（依赖 MYSQL_DSN / IM_MYSQL_DSN / LLM_API_URL 等 env）
make run-worker    # 本地起 Worker
make docker-build  # 构建 api/worker 两个镜像
```
