# Smart-Summary 时区统一 + 计时 + 周月选择 改造报告

日期: 2026-06-04 (UTC 记录, 系统已统一为 Asia/Shanghai/北京时间)
项目: `/root/projects/octo-smart-summary`
状态: **go build ./... 通过, go vet ./... 通过, go test ./internal/service/... 通过 (CGO 全量测试 worker/pipeline 亦通过)**
本次 **未** commit, 未 apply migration, 未重启服务 —— working tree 改动全部保留。

---

## 一、编译 / 测试结果

环境: 宿主无 go, 用容器编译。默认 `GOPROXY=https://proxy.golang.org` 在本机不可达, 已改用 `GOPROXY=https://goproxy.cn,direct` + 缓存卷 `gocache`。

命令:
```
docker run --rm -e GOPROXY=https://goproxy.cn,direct \
  -v /root/projects/octo-smart-summary:/app -v gocache:/go \
  -w /app golang:1.25-alpine sh -c 'go build ./... && go vet ./... && go test ./internal/service/...'
```

输出:
```
=== go build ./... ===
BUILD_OK
=== go vet ./... ===
VET_DONE                 (无告警)
=== go test ./internal/service/... ===
ok  github.com/Mininglamp-OSS/octo-smart-summary/internal/service  7.04s
```

全量 `go test ./...` 备注:
- `internal/worker` 与 `internal/pipeline` 在 **CGO_ENABLED=0**（alpine 默认）下因 `go-sqlite3 requires cgo` 报错；这是 **环境性失败, 与本次改动无关**。
- 开启 CGO 后（`apk add gcc musl-dev && CGO_ENABLED=1`）重跑：
  ```
  ok  github.com/Mininglamp-OSS/octo-smart-summary/internal/worker    0.34s
  ok  github.com/Mininglamp-OSS/octo-smart-summary/internal/pipeline  0.005s
  ```
  二者均通过, 证明本次对 worker 包(计时/时区)的改动正确。
- 我没有改动 `internal/pipeline` 任何文件(见 git status)。

---

## 二、需求 1：设置时间晚于当前则当天执行

**做了什么**: 新增 `service.NextRunInitial(...)`, 专用于 **创建/更新时的首次起算**, 语义是「今天的 run_time 若仍晚于当前时间, 就今天跑; 否则推进到下一周期」。原 `NextRunWithInterval` 改为 **ADVANCE 形态**(跑过一次后的周期推进), 始终至少推进一个完整间隔, 二者职责分离, 互不影响。

- 天模式: candidate = 今天的 run_time; `candidate > now` → 今天; 否则 +N 天。
- 周模式(无指定周几): 同天模式(今天 run_time 若未到就今天)。
- 周模式(指定周几): 取该周几最近一次未来的 run_time(今天恰好是该周几且未过点则今天, 否则该周几本周/下周)。
- 月模式: candidate = 本月目标日(day_of_month, 未指定则用当前日) run_time; 未过则本月, 否则推进一个月并月末钳位。
- cron 模式: 不变(cron 自身已编码时刻)。

**改了哪些文件**:
- `internal/service/schedule.go`: 新增 `NextRunInitial`, 拆分 ADVANCE/INITIAL 语义。
- `internal/api/handler/schedule.go`: 创建路径、更新路径改用 `NextRunInitial`（toggle 重启用仍走 ADVANCE 形 `NextRunWithInterval`, 保持「重新启用必须严格未来」的既有不变式)。
- `internal/service/schedule_test.go`: 新增 `TestNextRunInitial_DayToday / DayPassed / WeekTodayNoDOW / MonthThisMonth` 等单测。

---

## 三、需求 2：时区统一到北京时间 (Asia/Shanghai) —— 最重要

**做了什么**:
1. 把 `internal/` 与 `cmd/` 下所有 **非测试** 文件里的 `time.Now().UTC()` 全部替换为 `timezone.Now()`(返回 Asia/Shanghai 当前时间), 并为每个文件补 import `internal/timezone`。
2. `schedule.go` 的 `applyRunTime` / `NextRunWithInterval` / `NextRunInitial` 基准时间 `from` 现在来自 `timezone.Now()`, 其 `Location()` 自然是 Asia/Shanghai, 因此 `applyRunTime` 用 `t.Location()` 构造的 run_time 时刻就是北京时间。
3. 注释里把 run_time 「UTC」改为「Asia/Shanghai(北京时间)」。`daysInMonth` 内的 `time.UTC` 仅做天数计算(不涉及时刻), 按要求保留; `addMonthsClamped` 用 `t.Location()` 构造, 保持北京时间。
4. **未改 DSN**(loc=Local 配合容器 TZ=Asia/Shanghai 已一致)。

**涉及文件(替换 time.Now().UTC() → timezone.Now() + import)**:
- `internal/service/summary.go`（替换后该文件不再直接用 `time`, 已删除多余 `time` import）
- `internal/worker/personal_processor.go`
- `internal/worker/processor.go`
- `internal/worker/meta_processor.go`
- `internal/worker/scheduler.go`
- `internal/api/handler/personal.go`
- `internal/api/handler/schedule.go`
- `internal/api/handler/edit.go`
- `internal/api/handler/task.go`

(测试文件中的 `time.Now().UTC()` 按惯例未动, 不影响业务时区。)

**验证 / 推演**: 单测 `TestNextRunInitial_TimezoneBeijing`:
- 给定 `now = 2026-06-04T10:00:00+08:00`, `run_time=17:00`, 天模式 →
  `next_run = 2026-06-04T17:00:00+08:00`, `Hour()==17`, 时区 offset == `+08:00`(28800s), **不再偏 8 小时**。
- scheduler 用 `timezone.Now()`(北京时间)与 `next_run_at`(北京时间)比较, 能在北京 17:00 触发。

> 备注(需 Ares/老板知晓): 历史 migration `20260604-02-add-interval-months-runtime.sql` 中 `run_time` 列的 `COMMENT` 文字仍写着 `(UTC)`。这是已(或将)应用的 DDL 注释, 修改文件不会改变线上库的列注释, 故 **未改动该历史文件** 以免混淆; 语义上该列现在表示北京时间。如需纠正线上注释, 可在后续 migration 里 `ALTER ... MODIFY COLUMN run_time ... COMMENT 'HH:MM (Asia/Shanghai 北京时间)'`(可选, 纯文档性)。

---

## 四、需求 3：每个环节计时 + 写日志文件

**做了什么**:
1. 新增轻量计时日志器 `internal/timing/timing.go`:
   - `timing.Record(taskNo, stage, d)` / `timing.Observe(taskNo, stage, start)` / `timing.Stage(...)`。
   - 既 `log.Printf` 到 stdout, 又 **追加写到独立日志文件**, 每行:
     `2026-06-04T17:00:00+08:00 task_no=ST... stage=<环节> took_ms=<毫秒>`(时间戳为北京时间)。
   - 目录不存在时 `os.MkdirAll(0755)` 自动创建; 文件 `O_CREATE|O_WRONLY|O_APPEND`(append 模式); 并发写加锁; 打开失败优雅降级为仅 stdout。
   - 默认路径常量 `timing.DefaultLogPath = /var/log/smart-summary/timing.log`(容器内)。
2. 在主要环节补计时:
   - `internal/worker/processor.go` `executePipeline`: `execute_pipeline_total`, `fetch_messages`。
   - `internal/worker/personal_processor.go` `executePersonalPipeline`: `personal_pipeline_total`, `fetch_messages`, `resolve_user_names`, `llm_map_summary`(LLM 总结 Map 阶段), `llm_reduce_summary`(LLM Reduce 阶段), `build_citations`; 以及 `processPersonalSummary` 的 `persist_personal_result`(结果落库/状态更新)。
   - 原有 `internal/pipeline/fetch.go` 的 "took %dms" 日志保留不动(已覆盖 Layer1~4.5 与子阶段)。
3. `cmd/summary-worker/main.go`: 支持环境变量 `TIMING_LOG_PATH` 覆盖默认路径(留空则用默认)。

**烟雾测试(已跑通)**: 写一行后读回,
`2026-06-04T18:37:14+08:00 task_no=ST_test stage=stage_x took_ms=1234` —— 目录自动创建、append、北京时间戳均正确。

**日志文件路径**:
- 容器内: `/var/log/smart-summary/timing.log`(可被 `TIMING_LOG_PATH` 覆盖)。
- 建议宿主挂载(防容器重启丢失): 例如 `/root/octo-deploy/logs/smart-summary` → 容器 `/var/log/smart-summary`。
  在 summary-worker 服务定义里加:
  ```yaml
  volumes:
    - ./logs/smart-summary:/var/log/smart-summary
  ```
  （注: `octo-deploy/docker-compose.yaml` 当前 **未包含** summary-worker/summary-api 服务定义, 仅有 `SUMMARY_API_URL` 引用; smart-summary 镜像由 `Dockerfile.worker` / `Dockerfile.api` 构建。请在实际部署 summary-worker 的 compose/k8s 清单里加上该挂载——**挂载由 Ares/老板确认后施加**。)

---

## 五、需求 4：每周/每月时, 时间前面让用户选周几/几号

### 后端(已完成)
- 表新增字段(migration, **未 apply**): `migrations/sql/20260604-04-add-dow-dom.sql`
  - `day_of_week TINYINT NOT NULL DEFAULT 0` —— 周模式: 1=周一..7=周日, 0=不限。
  - `day_of_month TINYINT NOT NULL DEFAULT 0` —— 月模式: 1..31(月末钳位), 0=不限。
  - 含 `+migrate Up/Down`, 命名/格式对齐既有 migration; `embed.go` 用 `//go:embed *.sql` 自动包含。
- `internal/model/model.go` `SummarySchedule` 加 `DayOfWeek` / `DayOfMonth` 字段。
- `internal/service/schedule.go`:
  - `alignDayOfWeek(t, dow)`: 周模式(interval_days 为 7 的倍数)对齐到指定 ISO 周几。
  - `alignDayOfMonth(t, dom)`: 月模式对齐到指定几号, 复用 `daysInMonth` 月末钳位(如 31→Feb 28/29)。
  - `NextRunWithInterval` / `NextRunInitial` 新增 `dayOfWeek, dayOfMonth` 参数并接入对齐逻辑。
  - 新增 `ValidateDayOfWeek` / `ValidateDayOfMonth`。
- `internal/api/handler/schedule.go`: 创建/更新请求体接收 `day_of_week` / `day_of_month`, 校验并入库; List/Get 响应返回这两字段; 创建/更新走 `NextRunInitial`(带 dow/dom), scheduler 与 toggle 走 ADVANCE 形(带 dow/dom)。
- 单测: `TestNextRunInitial_WeekDOW / WeekDOWTodayAhead / WeekDOWTodayPassed / MonthDOM / MonthDOMPassed / MonthDOMClamp`, 以及 ADVANCE 形 `TestNextRunWithInterval_WeekDOWAdvance / MonthDOMAdvance`。

API 契约(新增可选字段, 向后兼容; 不传即 0=不限):
```
POST/PUT /api/v1/summary-schedules
{ ..., "interval_days": 7, "day_of_week": 1, "run_time": "09:00" }   // 每周一 09:00
{ ..., "interval_months": 1, "day_of_month": 15, "run_time": "09:00" } // 每月15号 09:00
```

### 前端(待改, 未改动 —— 给出清单与建议)
技术栈: React + `@douyinfe/semi-ui`, monorepo `pnpm`。相关包: `packages/dmworksummary`。

需要改的文件:
1. **`packages/dmworksummary/src/components/ScheduleForm.tsx`**（主表单, 核心改动）
   - 现状: 「频率」区块为 `每 [InputNumber every] [Select unit(day/week/month)] 在 [Select runTime]`。
   - 改法: 当 `unit==="week"` 时, 在 **时间选择前面** 插入一个「周几」下拉(周一..周日, 值 1..7, 可含「不限」=0); 当 `unit==="month"` 时插入「几号」下拉(1..31, 可含「不限」=0)。
   - 新增 state: `const [dayOfWeek, setDayOfWeek] = useState<number>(initialValues?.day_of_week ?? 0)`、`const [dayOfMonth, setDayOfMonth] = useState<number>(initialValues?.day_of_month ?? 0)`。
   - `handleSubmit` 的 `onSubmit({...})` 里追加 `day_of_week: unit==="week" ? dayOfWeek : 0`、`day_of_month: unit==="month" ? dayOfMonth : 0`。
   - 切换 unit 时建议重置另一个维度(week 清 dayOfMonth, month 清 dayOfWeek)。
2. **`packages/dmworksummary/src/types/summary.ts`**
   - 在 `CreateScheduleParams` / `UpdateScheduleParams` / `ScheduleItem`(约 line 197/213/225 三处) 各加可选 `day_of_week?: number;`、`day_of_month?: number;`。
3. **`packages/dmworksummary/src/utils/summaryHelpers.ts`**
   - `scheduleItemToConfig` / `scheduleToParams` 若需要透传 dow/dom 做回填, 在此扩展(当前它们只处理 every/unit/time; dow/dom 可在 Form 层直接从 `initialValues` 回填, 不强制改 helper)。
4. **`packages/dmworksummary/src/api/summaryApi.ts`**
   - 若 create/update 用强类型 body, 确认透传新增字段(多数情况直接随对象传出即可)。
5. **i18n**: `packages/dmworksummary/src/i18n/zh-CN.json`(及对应 en) 加 `summary.schedule.config.weekday`(周几)、`summary.schedule.config.dayOfMonth`(几号)及周一..周日文案。

建议(不强求跑通前端构建):
- 「周几」下拉用 1..7 对应周一..周日, 与后端 ISO 一致; 「不限」用 0。
- 「几号」下拉 1..31, 「不限」用 0; 选 29/30/31 时可加提示「短月将顺延到当月最后一天」(后端已做月末钳位)。
- UI 顺序: `每 N 周/月` → `周几/几号` → `在 HH:MM`(把周几/几号放在时间选择前面, 符合需求)。

---

## 六、改动文件清单(git status)

新增:
- `internal/timezone/`（Ares 已建的 timezone 包目录, 本次首次纳入引用）
- `internal/timing/timing.go`（新建计时日志器）
- `migrations/sql/20260604-04-add-dow-dom.sql`（新增 dow/dom 字段, 未 apply）

修改:
- `cmd/summary-worker/main.go`
- `internal/api/handler/{edit,personal,schedule,task}.go`
- `internal/model/model.go`
- `internal/service/{schedule,summary}.go`
- `internal/service/schedule_test.go`
- `internal/worker/{meta_processor,personal_processor,processor,scheduler}.go`

---

## 七、仍需 Ares / 老板确认或手动操作的事项

1. **apply migration**: `20260604-04-add-dow-dom.sql`（新增 `day_of_week` / `day_of_month`）—— 本次只写文件未执行, 请确认后 apply。
2. **compose 挂载日志目录**: 在实际部署 summary-worker 的 compose/清单中挂载 `/var/log/smart-summary` 到宿主(示例见第四节); 当前 `octo-deploy/docker-compose.yaml` 未含 summary-worker 服务定义, 需确认 smart-summary 的真实部署位置。
3. **重新构建镜像**: 代码改动需 `Dockerfile.worker` / `Dockerfile.api` 重新 build 并部署(本次未构建、未重启服务)。
4. **前端改动**: 需求4 前端(第五节清单)尚未实施, 请确认由谁实现/审。
5. **(可选)run_time 列注释**: 线上库列注释仍写 UTC, 如需纠正可加一条纯文档性 migration(见第三节备注)。
6. **commit**: 本次按要求 **未 commit**, working tree 改动保留, 待你确认后再提交。
