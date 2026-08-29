# 04 WebUI 与 API

- 状态：📝 R3.1 修订版，待快速一致性复核
- 受约束 ADR：[003](../decisions/003-webui-form.md) · [004](../decisions/004-frontend-stack.md)

## 1. 页面清单与路由

| 路由 | 页面 | 期 |
|---|---|---|
| `/login` | 用户名密码登录 | P0 |
| `/dashboard` | 概览：连接状态（User/Bot）、今日转发数、队列深度、degraded 提示、UserBot 登录向导入口 | P0 |
| `/forwarding` | 规则表格 CRUD（抽屉编辑）、启停、统计列、回溯补发按钮 | P0 |
| `/channels` | 频道注册表 CRUD、频道设置 | P0 |
| `/logs` | 实时日志流（WebSocket）+ 级别/组件/关键字过滤 | P0 |
| `/system` | 系统状态、暂停/恢复/重启、日志级别、审计日志表、Rich 能力 flag | P0 |
| `/settings` | settings 各 scope 表单（AI/转发全局/系统） | P0 |
| `/schedules` | 每频道总结调度管理 | P1 |
| `/summaries` | 总结历史、手动触发 | P1 |
| `/rag` | RAG Query Harness（调试页）+ reindex 管理 | P1 |
| `/interaction` | 投票/欢迎配置 | P2 |
| `/submissions` | 投稿审核队列 | P2 |
| `/conversations` | 讨论会话浏览 | P2 |

## 2. REST API 清单（`/api` 前缀；全部写操作落 system_audit_logs）

**auth（公开）**：`POST /auth/login` `{username,password}` → `Set-Cookie`（HttpOnly 会话，见 §4）；`POST /auth/logout`；`GET /auth/status`。

**system**：`GET /health`（公开，仅 status/version/uptime，01 §1.5）；`GET /system/status`（组件细项+指标+Rich flag+degraded 原因）；`POST /system/pause|resume|restart`；`PUT /system/log-level`；`GET /system/audit-logs`。

**userbot**：`GET /userbot/status`；登录向导三步 `POST /userbot/login/start {phone}` → `{request_id}`、`POST /userbot/login/code {request_id, code}`、`POST /userbot/login/password {request_id, password}`（2FA，可选步）；`POST /userbot/logout`；`POST /userbot/join {chat}`。

**channels**：`GET|POST /channels`；`GET|PUT|DELETE /channels/{id}`；`PUT /channels/{id}/settings`。

**forwarding**：`GET|POST /forwarding/rules`；`PUT|DELETE /forwarding/rules/{id}`；`POST /forwarding/rules/{id}/enable|disable`；`POST /forwarding/rules/{id}/backfill {limit}`；`GET /forwarding/stats?rule_id&days`；`GET|PUT /forwarding/settings`（scope=forwarding）。

**settings（通用）**：`GET /settings/{scope}`（secret 字段脱敏为 `•••`+尾 4）；`PUT /settings/{scope}`（校验失败 422 返回字段错误）。

**P1**：`GET|PUT /schedules/…`、`GET /summaries`、`POST /channels/{id}/summarize`、`POST /rag/query`、`POST /rag/answer`、`GET /rag/status`、`POST /rag/reindex {collection}`、`GET /rag/reindex/status`。

**P2**：投稿/投票/欢迎/会话相关 CRUD。

## 3. DTO 约定

- JSON `camelCase`；时间 ISO 8601 UTC；Telegram ID 以字符串传输。理由（R3.1 修正）：与 Go/MySQL `BIGINT` 模型统一、避免 Bot API 正负/`-100` 编码与裸 ID 混入 JS number、为大整数语义演进留余量（Bot API 当前承诺 dialog ID ≤52 有效位，尚无精度问题——仍统一 string）。
- 错误结构：`{"error": {"code": "VALIDATION_ERROR", "message": "…", "detail": {…字段错误…}}}`。
- 分页：`{"items": […], "total": n, "nextCursor?": "…"}`。
- **前端不成为配置真相源**（ADR-003）：所有写操作走上述 API → Go service 层 → MySQL；前端缓存仅展示用。

## 4. 会话鉴权（R3.1 重写：server-side opaque session，不用 JWT）

ADR-005 未拍板任何 JWT 库，Go 标准库亦无 JWT；本系统是单实例、单管理员、同源 SPA——引入 JWT 库或手写 JWT 均无收益。

```text
POST /auth/login
    ↓ crypto/subtle.ConstantTimeCompare 比较 .env 凭据
    ↓ crypto/rand 生成 256-bit session id（opaque）
    ↓ 服务端内存 session store（重启即失效，重新登录一次可接受）
    ↓ Set-Cookie: HttpOnly; SameSite=Strict; Secure（TLS 时）; Path=/; Max-Age=12h
```

- 收益：不新增 JWT library、token 不暴露给 JS、无 localStorage、无 refresh token。
- 会话固定 12h；登出即从 store 删除。
- 失败防护：同一来源 5 次失败锁 10 分钟（内存计数）。**IP 来源**：默认真实 TCP `RemoteAddr`；仅在显式配置 trusted proxy 后才采信 `X-Forwarded-For`（不无条件信任代理头）。成功登录写审计。
- 中间件：Cookie 校验；豁免：`/api/health`、`/api/auth/login`、静态资源。

## 5. WebSocket 协议（`/api/ws`）

鉴权：**同源 Cookie（§4）+ Origin 校验**，拒绝跨域连接（不传 token）。

```text
server → client:
  {type:"log", ts, level, component, msg}
  {type:"state", component, state}          // User/Bot 可用性、服务状态翻转
  {type:"task", name, progress, detail?}    // reindex/回溯进度
  {type:"ping"}（30s 心跳）
client → server:
  {type:"subscribe", levels?, components?, keyword?}   // 日志过滤在服务端做（环形缓冲容量内）
  {type:"ping"}
```

仅实时流走 WS（ADR-003 边界）；CRUD/配置/统计一律 REST。

## 6. RAG Query Harness（P1，`/rag` 页）

表单：问题文本、频道多选、时间范围、TopK、kind 过滤 → `POST /rag/query` → 结果表（`Score | 时间 | 类型 | 频道 | 消息（含 t.me 深链）`）→ 「使用这些 Context 调用 AI」→ `POST /rag/answer` → 展示 AI 输出与所用引用。属开发/管理能力（ADR-007），非用户聊天界面。

## 7. 前端架构（ADR-004 约束落地）

```text
API DTO（types.ts）
  → frontend model（纯 TS 类型 + 转换函数，无 Naive UI 依赖）
  → composable / store（pinia）
  → Naive UI 组件
```

- axios 拦截器：同源 Cookie 自动携带（`withCredentials` 同源默认）；401 → 跳 `/login`；错误 toast 统一格式。
- 组件层不反向泄漏（NDataTable 的 row 类型不出现在 model 层）。
- 构建：`pnpm build` → `dist/` → `go:embed`（运行时零 Node）；开发期 Vite proxy → 本地 Go 服务。
