# ADR-005：Go 基础库选型包

- 状态：✅ 已拍板（2026-08-29 实时核实维护状态后确定）
- 日期：2026-08-29

## 决策

| 模块 | 选择 | 核实状态（2026-08） | 说明 |
|---|---|---|---|
| Go 工具链 | **Go 1.26.x（最新稳定）** | — | openai-go 要求 ≥1.25；go-sql-driver 要求 ≥1.24 |
| MySQL 驱动 | **go-sql-driver/mysql** | ✅ v1.9.0，2026-04 仍在发版 | 纯 Go、轻量；MySQL 5.7+/MariaDB 10.5+ |
| 数据访问 | **sqlx** | ✅ 生态健康 | `database/sql` 薄扩展：struct 扫描、命名参数；SQL 直写，无 codegen 工作流 |
| HTTP Router | **标准库 `net/http.ServeMux`** | ✅ Go 1.22+ 原生方法+通配路由 | 零依赖；中间件需求仅 JWT 鉴权/访问日志/recover，手写足够 |
| 结构化日志 | **`log/slog`（标准库）** | ✅ 随 Go 维护 | JSON 输出；自定义 Handler 实现 WebUI 日志环形缓冲与 WebSocket 推送 |
| 定时调度 | **go-co-op/gocron v2** | ✅ 活跃维护 | cron 表达式 + 运行时动态增删任务（WebUI 改调度需要） |
| DB Migration | **pressly/goose** | ✅ 活跃 | SQL 迁移 `//go:embed` 嵌入、**作为库在启动时自动执行**（单二进制无人值守：启动即迁移）；完整历史表 |
| 参数校验 | **go-playground/validator/v10** | ✅ v10.30.3（2026-05-29） | struct tag 校验 API DTO |
| AI SDK | **openai/openai-go（官方）** | ✅ v3.45.0+（要求 Go ≥1.25） | `option.WithBaseURL()` 原生支持 DeepSeek/SiliconFlow 等 OpenAI 兼容端点 |
| WebSocket | **coder/websocket** | ✅ 活跃（nhooyr/websocket 官方继任，context-first） | 实时日志/状态/事件推送 |
| RAG 检索引擎 | **qdrant/go-client** | ✅ 活跃（详见 ADR-006） | Qdrant 为部署新增外部服务 |
| .env 解析 | **joho/godotenv + 手写 struct 加载** | 未单独核实 | 几十年事实标准；.env 仅十几项，随时可换自写 |

## 备选与否决理由

- **sqlc**：编译期类型安全更强（改表结构编译器直接报错），但构建链多一步 codegen 流程；21 张表、单人维护、SQL 不复杂，sqlx 零负担更符合长期低维护。
- **GORM / ent**：ORM magic 与隐藏复杂度；与旧项目「无 ORM 直写 SQL」经验相悖。
- **chi**：中间件/路由分组生态好，但本项目用不上，标准库足够。
- **zerolog**：极限性能，本项目不需要。
- **robfig/cron**：被指 2020 年后未维护、50+ 积压 PR；gocron 活跃且支持动态任务。
- **golang-migrate**：只记 dirty version，历史信息少；Atlas 声明式自动规划对单人项目过重。
- **gorilla/websocket**：曾有归档历史、状态起伏，新项目社区共识是 coder/websocket。
- **sashabaranov/go-openai**：社区库活跃但非官方；官方 SDK + WithBaseURL 已满足需求。

## 来源

[go-sql-driver/mysql releases](https://github.com/go-sql-driver/mysql/releases) · [sqlc vs GORM vs sqlx 2026](https://reintech.io/blog/sqlc-vs-gorm-vs-sqlx-go-database-libraries-compared-2026) · [robfig/cron](https://github.com/robfig/cron) · [go-co-op/gocron](https://pkg.go.dev/github.com/go-co-op/gocron) · [websocket.org Go 指南](https://websocket.org/guides/languages/go/) · [Coder blog: websocket](https://coder.com/blog/websocket) · [openai/openai-go](https://github.com/openai/openai-go) · [pressly/goose](https://github.com/pressly/goose) · [go-playground/validator releases](https://github.com/go-playground/validator/releases) · [Go 官方博客：1.22 路由增强](https://go.dev/blog/routing-enhancements)
