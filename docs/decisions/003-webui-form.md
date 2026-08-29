# ADR-003：WebUI 形态 — SPA + go:embed

- 状态：✅ 已拍板
- 日期：2026-08-29

## 背景

WebUI 不是简单设置页，而是完整管理面：频道、转发规则、调度、AI 配置、投稿审核、统计、实时日志、系统状态等。旧 Sakura-Bot（Vue 3 SPA + API）与 TG-Forwarder（aiohttp + 原生 JS 的规则 CRUD + 实时日志）都验证过这类交互复杂度适合 SPA。

## 决策

**SPA + `go:embed`。**

```text
开发 → pnpm build → dist/ → go:embed → sakura-bot 二进制
```

- 前端独立构建，构建产物嵌入 Go 二进制。
- 运行时**不需要 Node.js**；Node/npm/pnpm 只存在于开发和 CI 阶段。
- Go 进程同时提供：`/api/*` JSON API、WebSocket/SSE 实时数据、SPA 静态文件、history fallback。
- 最终部署仍然只有一个 `sakura-bot` 二进制；不部署 nginx 前端服务，不把前端拆成独立容器。

### 配置真相源约束（强制）

前端**绝不能直接成为配置真相源**：

```text
SPA → HTTP API → Go Service Layer → MySQL
```

WebUI、Telegram 命令、Scheduler，无论谁修改配置，都必须走同一套 Go service/repository 层。禁止前端直改某个 JSON/YAML/内存对象——杜绝旧 Sakura-Bot 的 `.env + config.json + 杂项文件` 三套配置源问题。

### 实时能力边界

- **WebSocket**：实时日志、Telegram 连接状态、正在执行的任务、系统事件。
- **REST/JSON**：CRUD、配置、统计查询。
- 不为了「全实时」把所有 API 都做成 WebSocket。

## 备选与否决理由

- **服务端模板 + HTMX**：无构建链、体积小，但规则编辑、实时日志过滤、仪表盘图表等复杂交互开发明显更繁琐，长期还债。
- **独立前端服务**：违背单二进制原则（ADR-002），多一个运维对象。
- **无 WebUI**：与用户硬性约束冲突（.env 含 WebUI 用户名/密码）。
