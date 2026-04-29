# 前端鉴权改造方案

## 背景

参考 dmwork-web PR #1042 的 Todo 模块实现，优化 smart-summary 前端的鉴权和 Space 切换机制。

## 现状分析

### 当前实现

`summaryApi.ts` 通过 `WKApp.apiClient` 发请求：

```typescript
// packages/dmworksummary/src/api/summaryApi.ts
import WKApp from '@dmwork/base/src/App';

export async function getSummaries(params) {
  return WKApp.apiClient.get('/api/v1/summaries', { params });
}
```

**问题**：
1. 依赖 `WKApp.apiClient` 的全局 baseURL，路径耦合
2. 没有独立的 axios 实例，无法单独配置 interceptor
3. 没有监听 `space-changed` 事件，切换 Space 后数据不刷新
4. 401 处理依赖全局 apiClient 逻辑

### Todo 模块最佳实践（PR #1042）

**独立 axios + interceptors**（`packages/dmworktodo/src/api/todoApi.ts`）：

```typescript
// packages/dmworktodo/src/api/todoApi.ts

import axios from 'axios';
import { WKApp } from '@dmwork/base';

// 1. 独立 axios 实例，不继承全局 baseURL
const todoAxios = axios.create({ baseURL: '' });

// 2. Interceptor 注入 token + space_id
todoAxios.interceptors.request.use((config) => {
  config.headers['token'] = WKApp.loginInfo.token;
  config.headers['X-Space-Id'] = WKApp.shared.currentSpaceId;
  return config;
});

// 3. 401 自动登出
todoAxios.interceptors.response.use(undefined, (err) => {
  if (err?.response?.status === 401) {
    WKApp.shared.logout();
  }
  return Promise.reject(err);
});
```

**监听 space-changed**（`packages/dmworktodo/src/module.tsx`，不在 API 文件中）：

```typescript
// packages/dmworktodo/src/module.tsx

let _spaceChangedHandler: (() => void) | null = null;

export default class TodoModule implements IModule {
  init(): void {
    // ...
    _spaceChangedHandler = () => {
      _pendingPayload = null;
    };
    WKApp.mittBus.on('space-changed', _spaceChangedHandler);
  }
}

// HMR 清理
if (import.meta.hot) {
  import.meta.hot.dispose(() => {
    if (_spaceChangedHandler) WKApp.mittBus.off('space-changed', _spaceChangedHandler);
    _spaceChangedHandler = null;
  });
}
```

---

## 改造方案

### 1. 创建独立 axios 实例

```typescript
// packages/dmworksummary/src/api/summaryApi.ts

import axios from 'axios';
import { WKApp } from '@dmwork/base';

const summaryAxios = axios.create({ baseURL: '' });

// Vite proxy 会将 /summary/api/v1/* 转发到 summary-api 服务
const BASE = '/summary/api/v1';
```

### 2. Vite 代理规则

在 `apps/web/vite.config.ts` 的 `proxy` 中添加（放在 `/api/` 通配规则之前）：

```typescript
// apps/web/vite.config.ts — server.proxy

// Summary service API — must be before the general /api/ rule
'/summary/api/v1': {
  target: env.VITE_SUMMARY_API_URL || 'http://localhost:8080',
  changeOrigin: true,
  secure: false,
  rewrite: (path: string) => path.replace(/^\/summary/, ''),
},
// Todo service API — must be before the general /api/ rule
'/todo/api/v1': {
  target: env.VITE_TODO_API_URL || 'http://localhost:8080',
  changeOrigin: true,
  secure: false,
  rewrite: (path: string) => path.replace(/^\/todo/, ''),
},
'/api/': {
  target: apiOrigin,
  // ...
},
```

`/summary/api/v1/summaries` → rewrite 去掉 `/summary` → 转发到 `summary-api:8080/api/v1/summaries`。

### 3. 添加 Request Interceptor

```typescript
summaryAxios.interceptors.request.use((config) => {
  const token = WKApp.loginInfo.token;
  if (token) {
    config.headers['token'] = token;
  }

  const spaceId = WKApp.shared.currentSpaceId;
  if (spaceId) {
    config.headers['X-Space-Id'] = spaceId;
  }

  return config;
});
```

### 4. 添加 Response Interceptor

```typescript
summaryAxios.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error?.response?.status === 401) {
      console.warn('[summaryApi] 401 Unauthorized, logging out');
      WKApp.shared.logout();
    }
    return Promise.reject(error);
  }
);
```

### 5. 封装请求方法

```typescript
function extractErrorMessage(err: unknown): string {
  const axiosErr = err as { response?: { data?: { error?: { message?: string } } } };
  const msg = axiosErr?.response?.data?.error?.message;
  const raw = msg || (err instanceof Error ? err.message : 'Request failed');
  return raw.length > 200 ? raw.slice(0, 200) + '…' : raw;
}

async function get<T>(path: string, params?: Record<string, unknown>): Promise<T> {
  try {
    const resp = await summaryAxios.get(`${BASE}${path}`, { params });
    return resp.data;
  } catch (err) {
    throw new Error(extractErrorMessage(err));
  }
}

async function post<T>(path: string, data?: unknown): Promise<T> {
  try {
    const resp = await summaryAxios.post(`${BASE}${path}`, data);
    return resp.data;
  } catch (err) {
    throw new Error(extractErrorMessage(err));
  }
}

// put, del 同理
```

### 6. 监听 Space 切换

在 `SummaryModule.init()` 中添加（与 TodoModule 一致，放在 module.tsx 而非 API 文件）：

```typescript
// packages/dmworksummary/src/module.tsx

import { WKApp } from '@dmwork/base';

let _spaceChangedHandler: (() => void) | null = null;

export class SummaryModule implements IModule {
  init(): void {
    // ... 路由注册 ...

    _spaceChangedHandler = () => {
      WKApp.mittBus.emit('summary-space-changed');
    };
    WKApp.mittBus.on('space-changed', _spaceChangedHandler);
  }
}

// HMR 清理
if (import.meta.hot) {
  import.meta.hot.dispose(() => {
    if (_spaceChangedHandler) {
      WKApp.mittBus.off('space-changed', _spaceChangedHandler);
      _spaceChangedHandler = null;
    }
  });
}
```

### 7. 列表页响应 Space 切换

`SummaryListPage` 是 class 组件（`extends Component`），使用生命周期方法而非 hooks：

```typescript
// packages/dmworksummary/src/pages/SummaryListPage.tsx

import { WKApp } from '@dmwork/base';

export default class SummaryListPage extends Component<{}, SummaryListPageState> {
  private searchTimer: ReturnType<typeof setTimeout> | null = null;
  private handleStatusChange_ = () => this.loadData();
  private handleSpaceChanged_ = () => this.loadData();

  componentDidMount() {
    this.loadData();
    window.addEventListener('summary-status-change', this.handleStatusChange_);
    WKApp.mittBus.on('summary-space-changed', this.handleSpaceChanged_);
  }

  componentWillUnmount() {
    if (this.searchTimer) clearTimeout(this.searchTimer);
    window.removeEventListener('summary-status-change', this.handleStatusChange_);
    WKApp.mittBus.off('summary-space-changed', this.handleSpaceChanged_);
  }

  // ...
}
```

---

## 迁移范围

summaryApi.ts 现有 **24 个** 导出方法，全部从 `WKApp.apiClient` 迁移到独立 `summaryAxios`：

| # | 方法 | HTTP | 路径 |
|---|------|------|------|
| 1 | `createSummary` | POST | `/summaries` |
| 2 | `listSummaries` | GET | `/summaries` |
| 3 | `getSummaryDetail` | GET | `/summaries/:id` |
| 4 | `deleteSummary` | DELETE | `/summaries/:id` |
| 5 | `regenerateSummary` | POST | `/summaries/:id/regenerate` |
| 6 | `cancelSummary` | POST | `/summaries/:id/cancel` |
| 7 | `confirmParticipation` | POST | `/summaries/:id/confirm` |
| 8 | `declineParticipation` | POST | `/summaries/:id/decline` |
| 9 | `acceptInvitation` | POST | `/summaries/:id/accept` |
| 10 | `respondToTask` | POST | `/summaries/:id/respond` |
| 11 | `getPersonalResult` | GET | `/summaries/:id/personal` |
| 12 | `submitPersonalResult` | POST | `/summaries/:id/submit` |
| 13 | `getMembers` | GET | `/summaries/:id/members` |
| 14 | `getParticipants` | GET | `/summaries/:id/participants` |
| 15 | `getTemplates` | GET | `/summary-templates` |
| 16 | `inferScope` | GET | `/summary-infer` |
| 17 | `getSchedule` | GET | `/summary-schedules/:id` |
| 18 | `createSchedule` | POST | `/summary-schedules` |
| 19 | `listSchedules` | GET | `/summary-schedules` |
| 20 | `updateSchedule` | PUT | `/summary-schedules/:id` |
| 21 | `deleteSchedule` | DELETE | `/summary-schedules/:id` |
| 22 | `toggleSchedule` | PUT | `/summary-schedules/:id/toggle` |
| 23 | `getChatCandidates` | GET | `/summary-chat-candidates` |
| 24 | `getMemberCandidates` | GET | `/summary-member-candidates` |

> **注意**：`getChatCandidates` 和 `getMemberCandidates` 当前手动将 `space_id` 作为查询参数注入（`{ param: { ...params, space_id: spaceId } }`）。添加 interceptor 后，`X-Space-Id` 已通过 header 自动注入到每个请求，应移除这两个方法中的手动 `space_id` 注入，改由后端从 `X-Space-Id` header 读取。

---

## 完整改造后的 summaryApi.ts

```typescript
import axios from 'axios';
import { WKApp } from '@dmwork/base';
import type {
  ApiResponse,
  ChatCandidate,
  CreateSummaryParams,
  CreateScheduleParams,
  InferResult,
  ListSummariesParams,
  ListSummariesResponse,
  MemberCandidate,
  MemberStatus,
  Participant,
  PersonalResult,
  ScheduleItem,
  SourceItem,
  SummaryDetail,
  SummaryTemplate,
  UpdateScheduleParams,
} from '../types/summary';

const summaryAxios = axios.create({ baseURL: '' });

summaryAxios.interceptors.request.use((config) => {
  const token = WKApp.loginInfo.token;
  if (token) {
    config.headers['token'] = token;
  }
  const spaceId = WKApp.shared.currentSpaceId;
  if (spaceId) {
    config.headers['X-Space-Id'] = spaceId;
  }
  return config;
});

summaryAxios.interceptors.response.use(
  (resp) => resp,
  (err) => {
    if (err?.response?.status === 401) {
      WKApp.shared.logout();
    }
    return Promise.reject(err);
  }
);

const BASE = '/summary/api/v1';

function extractErrorMessage(err: unknown): string {
  const axiosErr = err as { response?: { data?: { error?: { message?: string } } } };
  const msg = axiosErr?.response?.data?.error?.message;
  const raw = msg || (err instanceof Error ? err.message : 'Request failed');
  return raw.length > 200 ? raw.slice(0, 200) + '…' : raw;
}

async function get<T>(path: string, params?: Record<string, unknown>): Promise<T> {
  try {
    const resp = await summaryAxios.get(`${BASE}${path}`, { params });
    return resp.data;
  } catch (err) {
    throw new Error(extractErrorMessage(err));
  }
}

async function post<T>(path: string, data?: unknown): Promise<T> {
  try {
    const resp = await summaryAxios.post(`${BASE}${path}`, data);
    return resp.data;
  } catch (err) {
    throw new Error(extractErrorMessage(err));
  }
}

async function put<T>(path: string, data?: unknown): Promise<T> {
  try {
    const resp = await summaryAxios.put(`${BASE}${path}`, data);
    return resp.data;
  } catch (err) {
    throw new Error(extractErrorMessage(err));
  }
}

async function del<T>(path: string): Promise<T> {
  try {
    const resp = await summaryAxios.delete(`${BASE}${path}`);
    return resp.data;
  } catch (err) {
    throw new Error(extractErrorMessage(err));
  }
}

// ─── Core Summary Operations ───────────────────────────

export async function createSummary(params: CreateSummaryParams): Promise<{ task_id: number }> {
  return post('/summaries', params);
}

export async function listSummaries(params: ListSummariesParams): Promise<ListSummariesResponse> {
  return get('/summaries', params as Record<string, unknown>);
}

export async function getSummaryDetail(taskId: number): Promise<SummaryDetail> {
  return get(`/summaries/${taskId}`);
}

export async function deleteSummary(taskId: number): Promise<void> {
  return del(`/summaries/${taskId}`);
}

export async function regenerateSummary(taskId: number): Promise<{ task_id: number }> {
  return post(`/summaries/${taskId}/regenerate`);
}

// ─── Status Management ─────────────────────────────────

export async function cancelSummary(taskId: number): Promise<void> {
  return post(`/summaries/${taskId}/cancel`);
}

export async function confirmParticipation(taskId: number, sources: SourceItem[]): Promise<void> {
  return post(`/summaries/${taskId}/confirm`, {
    sources: sources.map((s) => ({
      source_type: s.source_type,
      source_id: s.source_id,
    })),
  });
}

export async function declineParticipation(taskId: number): Promise<void> {
  return post(`/summaries/${taskId}/decline`);
}

export async function acceptInvitation(taskId: number): Promise<void> {
  return post(`/summaries/${taskId}/accept`);
}

export async function respondToTask(taskId: number, action: 'accept' | 'reject'): Promise<void> {
  return post(`/summaries/${taskId}/respond`, { action });
}

// ─── Personal Results ──────────────────────────────────

export async function getPersonalResult(taskId: number): Promise<PersonalResult> {
  return get(`/summaries/${taskId}/personal`);
}

export async function submitPersonalResult(taskId: number): Promise<void> {
  return post(`/summaries/${taskId}/submit`);
}

export async function getMembers(taskId: number): Promise<MemberStatus[]> {
  const data = await get<{ members: MemberStatus[] }>(`/summaries/${taskId}/members`);
  return data?.members || [];
}

// ─── Participants & Data ───────────────────────────────

export async function getParticipants(taskId: number): Promise<Participant[]> {
  const data = await get<{ participants: Participant[] }>(`/summaries/${taskId}/participants`);
  return data.participants;
}

export async function getTemplates(): Promise<SummaryTemplate[]> {
  const data = await get<SummaryTemplate[]>('/summary-templates');
  return data || [];
}

export async function inferScope(topic: string): Promise<InferResult> {
  return get('/summary-infer', { topic } as Record<string, unknown>);
}

// ─── Schedule CRUD ─────────────────────────────────────

export async function getSchedule(scheduleId: number): Promise<ScheduleItem> {
  return get(`/summary-schedules/${scheduleId}`);
}

export async function createSchedule(params: CreateScheduleParams): Promise<ScheduleItem> {
  return post('/summary-schedules', params);
}

export async function listSchedules(): Promise<ScheduleItem[]> {
  const data = await get<ScheduleItem[]>('/summary-schedules');
  return data || [];
}

export async function updateSchedule(scheduleId: number, params: UpdateScheduleParams): Promise<ScheduleItem> {
  return put(`/summary-schedules/${scheduleId}`, params);
}

export async function deleteSchedule(scheduleId: number): Promise<void> {
  return del(`/summary-schedules/${scheduleId}`);
}

export async function toggleSchedule(scheduleId: number, isActive: boolean): Promise<ScheduleItem> {
  return put(`/summary-schedules/${scheduleId}/toggle`, { is_active: isActive });
}

// ─── Candidate Selection ───────────────────────────────
// space_id 已由 interceptor 通过 X-Space-Id header 注入，
// 无需再手动添加 space_id 查询参数。

export async function getChatCandidates(params?: { keyword?: string; chat_type?: string }): Promise<ChatCandidate[]> {
  const data = await get<ChatCandidate[]>('/summary-chat-candidates', params as Record<string, unknown>);
  return data || [];
}

export async function getMemberCandidates(params?: { keyword?: string }): Promise<MemberCandidate[]> {
  const data = await get<MemberCandidate[]>('/summary-member-candidates', params as Record<string, unknown>);
  return data || [];
}
```

---

## 改动清单

| 文件 | 改动 |
|------|------|
| `api/summaryApi.ts` | 独立 axios + interceptors，迁移全部 24 个方法 |
| `module.tsx` | 监听 `space-changed`，转发 `summary-space-changed` |
| `pages/SummaryListPage.tsx` | `componentDidMount` 中注册 mittBus 监听，`componentWillUnmount` 中清理 |
| `apps/web/vite.config.ts` | 添加 `/summary/api/v1` 代理规则 |

---

## 测试验证

1. **token 自动注入**：请求 header 包含 `token: xxx`（小写，与 todoApi.ts 一致）
2. **Space-Id 自动注入**：请求 header 包含 `X-Space-Id: xxx`
3. **401 自动登出**：token 过期时跳转登录页
4. **Space 切换刷新**：切换 Space 后列表自动刷新
5. **Vite 代理**：`/summary/api/v1/*` 正确转发到 summary-api 服务
6. **getChatCandidates/getMemberCandidates**：确认后端从 `X-Space-Id` header 读取 space_id，不再依赖查询参数
7. **向后兼容**：不影响现有功能

---

## 总结

| 改进点 | 收益 |
|--------|------|
| 独立 axios 实例 | 不耦合全局 apiClient |
| Request interceptor | token/space_id 实时注入 |
| Response interceptor | 401 统一处理 |
| Vite 代理规则 | summary-api 独立部署，路径与 todo 一致 |
| space-changed 监听（module.tsx） | 切换 Space 自动刷新 |
| 全量迁移 24 方法 | 消除 WKApp.apiClient 依赖 |

**参考**：`packages/dmworktodo/src/api/todoApi.ts` + `packages/dmworktodo/src/module.tsx`（PR #1042）
