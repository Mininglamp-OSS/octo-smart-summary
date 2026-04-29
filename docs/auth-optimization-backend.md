# 后端鉴权改造方案

## 背景

参考 dmworkim PR #1227 的 Auth Verify API 设计，优化 smart-summary 后端的鉴权机制。

## 现状分析

### 当前实现

```
前端 → Token header → summary-api → Redis GET token:{token} → uid@name@ → 取 uid
```

**依赖**：
- `REDIS_ADDR` — 直连 dmworkim 的 Redis
- `REDIS_DB` — Redis 数据库编号
- `IM_MYSQL_DSN` — 查用户名（`ResolveUserName`）

**问题**：
1. 直连 Redis 耦合 dmworkim 内部实现，token 格式变了要跟着改
2. `X-User-Id` header 在 public 端口可伪造身份（安全漏洞，`AuthMiddleware` 和 `StrictAuthMiddleware` 均受影响）
3. `ResolveUID` 只返回 uid，丢掉了 name（value 格式 `{uid}@{name}@`，只取第一段）
4. Space 写操作没强制校验 space_id

### dmworkim Auth Verify API（PR #1227）

```
POST /v1/auth/verify
  {"token": "xxx"} → {uid, name, role, owned_bots: [{uid, name}]}

POST /v1/auth/verify-bot
  {"bot_token": "bf_xxx"} → {bot_uid, bot_name, owner_uid, owner_name, space_id}
```

- Rate limit: 1000 req/min/IP, burst 100
- 无需 auth header，供微服务内部调用

---

## 改造方案

### 优先级排序

| 优先级 | 改动 | 风险 | 工作量 |
|--------|------|------|--------|
| **P0** | X-User-Id 旁路修复 | 安全漏洞 | 15 行 |
| **P1** | Redis → Auth Verify API（含本地缓存） | 低 | 中 |
| **P2** | Space 写操作强制校验 | 低 | 5 行 |
| **P3** | 返回完整用户信息 | 无 | 小 |

---

### P0: X-User-Id 旁路修复（安全漏洞）

**问题**：`AuthMiddleware` 和 `StrictAuthMiddleware` 都从请求读取 `X-User-Id` header，当 token 为空时直接使用该值。外部用户可通过伪造 `X-User-Id` header 冒充任意身份。

- `AuthMiddleware`（`internal/middleware/space.go:40`）：token 为空时以 `X-User-Id` 作为 user_id，允许无凭据请求通过
- `StrictAuthMiddleware`（`internal/middleware/space.go:83`）：token 为空但 `X-User-Id` 有值时直接放行

**修复**：Public 端口的两个中间件都必须忽略 `X-User-Id`，仅通过 token 验证获取 uid。

```go
// internal/middleware/space.go

func AuthMiddleware(resolver TokenResolver) gin.HandlerFunc {
    return func(c *gin.Context) {
        token := c.GetHeader("Token")
-       userID := c.GetHeader("X-User-Id")
+       userID := ""

        if token != "" && resolver != nil {
            uid, err := resolver.ResolveUID(c.Request.Context(), token)
            if err != nil {
                c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
                    "code":    5001,
                    "message": "token resolution error",
                })
                return
            }
            if uid == "" {
                c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
                    "code":    4010,
                    "message": "invalid or expired token",
                })
                return
            }
            userID = uid
        }

        c.Set("user_id", userID)
        c.Set("token", token)
        c.Next()
    }
}

func StrictAuthMiddleware(resolver TokenResolver) gin.HandlerFunc {
    return func(c *gin.Context) {
        token := c.GetHeader("Token")
-       userID := c.GetHeader("X-User-Id")
+       userID := ""

        if token != "" && resolver != nil {
            uid, err := resolver.ResolveUID(c.Request.Context(), token)
            // ... 验证逻辑
            userID = uid
        }

        if userID == "" {
            c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
                "code":    401,
                "message": "authentication required",
            })
            return
        }

        c.Set("user_id", userID)
        c.Set("token", token)
        c.Next()
    }
}
```

**Internal 端口**（8081）无需修改——`SetupInternal`（`internal/api/router/router.go:83`）不注册任何 auth 中间件，仅供 Docker 内部网络的 worker 回调使用，外部不可达。

---

### P1: Redis → Auth Verify API（含本地缓存）

**目标**：去掉 `REDIS_ADDR` / `REDIS_DB` 依赖，通过 HTTP 调用 dmworkim 验证 token。

> **注意**：本地缓存为**必选项**，非可选。Auth Verify API 限速 1000 req/min/IP，不加缓存高峰期会被限流。

#### 1. 新增配置

```go
// internal/config/config.go

type Config struct {
    // ...
-   RedisAddr string
-   RedisDB   int
+   OctoAPIURL string  // e.g. "http://tangsengdaodaoserver:8090"
}
```

**环境变量**：`OCTO_API_URL`（替代 `REDIS_ADDR` 和 `REDIS_DB`）

#### 2. 新增 HTTP Token Resolver

```go
// internal/auth/http_resolver.go

package auth

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "time"
)

type HTTPTokenResolver struct {
    baseURL    string
    httpClient *http.Client
}

func NewHTTPTokenResolver(baseURL string) *HTTPTokenResolver {
    return &HTTPTokenResolver{
        baseURL: baseURL,
        httpClient: &http.Client{
            Timeout: 5 * time.Second,
        },
    }
}

type verifyRequest struct {
    Token string `json:"token"`
}

type verifyResponse struct {
    UID       string `json:"uid"`
    Name      string `json:"name"`
    Role      string `json:"role"`
    OwnedBots []struct {
        UID  string `json:"uid"`
        Name string `json:"name"`
    } `json:"owned_bots"`
}

func (r *HTTPTokenResolver) ResolveUID(ctx context.Context, token string) (string, error) {
    if token == "" {
        return "", nil
    }

    body, _ := json.Marshal(verifyRequest{Token: token})
    req, err := http.NewRequestWithContext(ctx, "POST", r.baseURL+"/v1/auth/verify", bytes.NewReader(body))
    if err != nil {
        return "", err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := r.httpClient.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusUnauthorized {
        return "", nil // invalid token
    }
    if resp.StatusCode != http.StatusOK {
        return "", fmt.Errorf("auth verify failed: %d", resp.StatusCode)
    }

    var result verifyResponse
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return "", err
    }

    return result.UID, nil
}
```

**Circuit Breaker 建议**：HTTP resolver 依赖外部服务，若 dmworkim 宕机或响应缓慢，所有请求都会被阻塞或超时。建议后续引入 circuit breaker（如 `sony/gobreaker`）：

- 连续 N 次失败后熔断，快速返回错误而非等待超时
- 熔断期间定期探测恢复
- 与缓存配合：熔断期间已缓存的 token 仍可用，仅新 token 受影响

初期可暂不实现，但 `HTTPTokenResolver` 的接口设计已支持后续包装。

#### 3. 本地缓存（必选）

Auth Verify API 限速 1000 req/min/IP，前端每次操作都带 token，高峰期同一 token 可能每秒被验证多次。必须加本地缓存减少 HTTP 调用。

```go
// internal/auth/cached_resolver.go

package auth

import (
    "context"
    "sync"
    "time"
)

type cacheEntry struct {
    uid      string
    expireAt time.Time
}

type CachedResolver struct {
    inner    TokenResolver
    cache    sync.Map // token → *cacheEntry
    ttl      time.Duration
    maxSize  int
}

func NewCachedResolver(inner TokenResolver, ttl time.Duration, maxSize int) *CachedResolver {
    r := &CachedResolver{
        inner:   inner,
        ttl:     ttl,
        maxSize: maxSize, // 防止内存无限增长，建议 10000
    }
    go r.evictLoop()
    return r
}

func (r *CachedResolver) ResolveUID(ctx context.Context, token string) (string, error) {
    if cached, ok := r.cache.Load(token); ok {
        entry := cached.(*cacheEntry)
        if time.Now().Before(entry.expireAt) {
            return entry.uid, nil
        }
        r.cache.Delete(token)
    }
    uid, err := r.inner.ResolveUID(ctx, token)
    if err == nil && uid != "" {
        r.cache.Store(token, &cacheEntry{uid: uid, expireAt: time.Now().Add(r.ttl)})
    }
    return uid, err
}

// evictLoop 定期清理过期条目，防止 sync.Map 无限增长。
func (r *CachedResolver) evictLoop() {
    ticker := time.NewTicker(r.ttl)
    defer ticker.Stop()
    for range ticker.C {
        now := time.Now()
        count := 0
        r.cache.Range(func(key, value any) bool {
            count++
            entry := value.(*cacheEntry)
            if now.After(entry.expireAt) {
                r.cache.Delete(key)
            }
            return true
        })
        // 若条目数超过 maxSize，强制清理最早过期的
        if count > r.maxSize {
            r.cache.Range(func(key, _ any) bool {
                r.cache.Delete(key)
                count--
                return count > r.maxSize
            })
        }
    }
}
```

参数建议：TTL 30 秒，maxSize 10000。TTL 不宜过长——token 被吊销后需要在 TTL 内生效。

#### 4. 启动改造

```go
// cmd/summary-api/main.go

-   rdb := appredis.New(cfg.RedisAddr, cfg.RedisDB)
+   httpResolver := auth.NewHTTPTokenResolver(cfg.OctoAPIURL)
+   authResolver := auth.NewCachedResolver(httpResolver, 30*time.Second, 10000)

    // Public API server
-   publicRouter := router.SetupPublic(summaryDB, imDB, hub, rdb, cfg.WorkerTriggerURL)
+   publicRouter := router.SetupPublic(summaryDB, imDB, hub, authResolver, cfg.WorkerTriggerURL)

    // ...

    // Graceful shutdown（移除 rdb.Close()）
-   if rdb != nil {
-       rdb.Close()
-   }
```

#### 5. 删除 Redis 包

```bash
rm -rf internal/redis/
```

同步删除 `cmd/summary-api/main.go` 中的 `appredis` import。

---

### P2: Space 写操作强制校验

**问题**：当前允许空 space_id，写操作可能污染数据。

**修复**：对 POST/PUT/DELETE 强制要求 space_id。

```go
// internal/middleware/space.go

func StrictSpaceMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        spaceID := c.GetHeader("X-Space-Id")
        if spaceID == "" {
            spaceID = c.GetHeader("X-Org-Id")
        }

        // 写操作强制要求 space_id
        if spaceID == "" && isWriteMethod(c.Request.Method) {
            c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
                "code":    40001,
                "message": "X-Space-Id header required for write operations",
            })
            return
        }

        c.Set("space_id", spaceID)
        c.Next()
    }
}

func isWriteMethod(method string) bool {
    return method == "POST" || method == "PUT" || method == "DELETE"
}
```

---

### P3: 返回完整用户信息（可选）

如果需要在 context 中存储更多用户信息：

```go
type UserInfo struct {
    UID  string
    Name string
    Role string
}

func (r *HTTPTokenResolver) ResolveUserInfo(ctx context.Context, token string) (*UserInfo, error) {
    // ... 调用 auth/verify，返回完整信息
}
```

当前实际只用 uid，name 通过 IM DB 查询，暂不需要。

---

## 配置变更

### 环境变量

| 旧 | 新 | 说明 |
|----|----|----|
| `REDIS_ADDR` | ❌ 删除 | 不再直连 Redis |
| `REDIS_DB` | ❌ 删除 | 不再直连 Redis |
| — | `OCTO_API_URL` | dmworkim API 地址 |

### internal/config/config.go

```go
-   RedisAddr string
-   RedisDB   int
+   OctoAPIURL string

// Load() 中：
-   RedisAddr: envStr("REDIS_ADDR", "localhost:6379"),
-   RedisDB:   envInt("REDIS_DB", 0),
+   OctoAPIURL: envStr("OCTO_API_URL", "http://tangsengdaodaoserver:8090"),
```

### docker-compose.yaml

```yaml
summary-api:
  environment:
-   REDIS_ADDR: "redis:6379"
-   REDIS_DB: "0"
+   OCTO_API_URL: "http://tangsengdaodaoserver:8090"
```

---

## 测试验证

1. **鉴权正常**：带 token 请求 → 200
2. **无 token 拒绝**：不带 token 请求 public 端口 → 401（StrictAuthMiddleware 路由）
3. **无效 token 拒绝**：带错误 token → 401
4. **X-User-Id 伪造拒绝**：只带 X-User-Id 不带 token → 401（AuthMiddleware 和 StrictAuthMiddleware 均拒绝）
5. **写操作无 space_id 拒绝**：POST/PUT/DELETE 不带 X-Space-Id → 400
6. **缓存生效**：相同 token 短时间内多次请求，仅首次调用 Auth API
7. **缓存过期**：等待 TTL 后重新验证

---

## 总结

| 改动 | 收益 |
|------|------|
| P0 X-User-Id 修复 | 堵住安全漏洞（AuthMiddleware + StrictAuthMiddleware） |
| P1 HTTP Resolver + 缓存 | 去掉 Redis 依赖，解耦；缓存确保不触发 1000 req/min 限速 |
| P2 Space 强制校验 | 防止跨 Space 数据污染 |

**改完后依赖**：
- ❌ Redis — 不再需要
- ✅ dmworkim API — HTTP 调用 auth/verify（经本地缓存）
- ✅ IM MySQL — 仍需查用户名（`ResolveUserName`）
