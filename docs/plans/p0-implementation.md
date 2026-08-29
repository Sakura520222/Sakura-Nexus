# Sakura-Bot Go P0 实施计划

- 状态：📝 编写完成，待用户批准——**批准前零实现代码**
- 日期：2026-08-29
- 依据：已冻结的 [ADR 001–008](../decisions/README.md) 与总体设计 [overview](../design/overview.md) + 01–07（R3.1.1）

## 1. 范围锁定（P0 = A + B + C，ADR-007）

**做**：Go skeleton/lifecycle、config、MySQL migrations、gotd User/Bot + 全部持久状态、updates/recovery、转发引擎全量、AI rewrite、媒体/相册、Outbound/Rich 基础设施、WebUI（auth/API/7 页面）、systemd/Docker、测试、production 验收。

**不做（即使设计已冻结）**：Qdrant、Summary、Dense RAG、讨论会话、投稿/投票——P0 只允许在设计中已冻结的接口/schema 边界存在，**不创建** `internal/summary`、`internal/rag`、`internal/conversation`、`internal/platform/qdrant` 包，不建 P1/P2 业务表。

## 2. 总原则

1. **风险前置（本计划最重要的排序原则）**：本次重写最大工程风险 = gotd 双客户端稳定性 + 持久状态闭环。因此**在写任何转发/引擎/WebUI 代码之前**，Phase 1 就用最小代码 + 真实账号验证 Telegram 连通性（GATE-1）。绝不「先铺一大堆骨架」。
2. **测试门禁**：每个任务合入前必须通过——`gofmt -l` 空、`golangci-lint run`（含 depguard，Phase 1 起生效）、`go test -race ./...` 全绿；标注 `I` 的任务加跑 `go test -race -tags integration ./...`（GitHub Actions MySQL service container / 本地 compose）。
3. **Commit 粒度**：每任务 1–3 个 commit（`feat:/fix:/chore:/test:`，Conventional Commits）；每个 commit 单独可编译、测试绿。**禁止**跨任务大 commit。
4. **可验证交付物**：每任务定义了「验证」一栏——不满足验证标准的任务不算完成。
5. **冒烟脚本（S 类验证）**：`cmd/smoke/`（smoke-bot / smoke-user / smoke-forward / smoke-rich），手动执行、不进 CI；需要真实 `.env` 凭据。

## 3. 任务依赖 DAG

```text
Phase 0 地基                Phase 1 Telegram 风险验证            Phase 2 存储/配置
T0.1 module 骨架 ──┬─→ T1.1 Bot 连通+session ──┬─→ T1.3 状态持久化 ─→ T2.1 migrations(P0 表)
T0.2 CI 流水线 ────┤        （GATE-1a）        │   +Manager+recovery   T2.2 sqlx pool
T0.3 config .env ──┘   T1.2 User 连通+login ──┘      （GATE-1b）      T2.3 settings 中心
                              （GATE-1a）                                 T2.4 repositories
        │                                                          │
        ▼                                                          ▼
Phase 3 转发引擎（GATE-2）                              Phase 4 Rich 出站
T3.1 domain → T3.2 过滤链 → T3.3 相册聚合 ┐             T4.1 botapi HTTP 客户端
T3.4 Outbound MTProto ───────────────────┼→ T3.5 engine T4.2 normalizer/validator/splitter
T3.6 媒体临时文件 → T3.7 AI rewrite ─────┘   编排+队列    T4.3 Outbound 路由+lazy capability
T3.8 底栏 → T3.9 backfill                     │                │
        └────────────── GATE-2 端到端 ────────┴── GATE-2b Rich 冒烟
                       │
                       ▼
Phase 5 生命周期 + WebUI（GATE-3）
T5.1 lifecycle/supervisor/Availability → T5.2 webapi 骨架(auth/session/health)
→ T5.3 业务 API(forwarding/channels/userbot 向导/system/WS) → T5.4 前端 7 页面 + embed
        │
        ▼
Phase 6 部署与验收（GATE-4 = P0 Done）
T6.1 healthcheck 子命令 → T6.2 Docker/compose 双文件/systemd → T6.3 README
→ T6.4 P0 验收执行（07 §3 checklist + 24h 稳定性）
```

**首次连接真实 Telegram 的时机：Phase 1 T1.1（全计划第 4 个任务）**——在任何转发引擎、WebUI、业务逻辑代码存在之前。

## 4. 任务表（验证类型：U=单元测试 I=集成测试 S=手动冒烟）

### Phase 0：工程地基

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T0.1 | Go module 骨架：`go.mod`（sakura-bot）、`cmd/sakura-bot/main.go`（仅 --version）、`internal/{app,config,logging,platform/{mysql,telegram,botapi,ai},forwarding,webapi,domain}` 空包、Makefile、`.env.example`（01 §6.1 全量） | 可编译空项目 | `go build ./...` | — |
| T0.2 | CI：GitHub Actions（lint→test→build，07 §2 清单；compose config 校验需 compose 文件后补入） | `.github/workflows/ci.yml` | push 后 CI 绿 | T0.1 |
| T0.3 | `internal/config`：.env struct + 必填校验 + 加载（godotenv） | 单测：缺必填报全量缺失项 | U | T0.1 |

### Phase 1：Telegram 风险验证（GATE-1）

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T1.1 | **Bot 客户端最小连通**：`platform/telegram.BotClient` + MySQL `session.Storage`（gotd_sessions 表，此时 migrations 先行仅建 telegram 持久化四表+peers/aliases——T2.1 之前的最小迁移 0001）+ bot token 登录 + `getMe` | `cmd/smoke/smoke-bot`：登录→getMe→打印身份→优雅关闭（session 落库） | **S（首次真实 TG）**+ I（storage 往返） | T0.3 |
| T1.2 | **User 客户端最小连通**：`UserClient` + `sakura-bot login-user` CLI 交互子命令（手机号/验证码/2FA → session 落库）+ 断线重连观察 | `cmd/smoke/smoke-user`：登录→收一条 update→打印→重启免登录 | S + I | T1.1 |
| T1.3 | **持久状态闭环**：`telegram_update_states`/`telegram_channel_states`/`telegram_peers`/`telegram_peer_aliases` 的 gotd 存储实现 + `updates.Manager` 接入（含 OnTooLong/OnLoadChannelStateFailed/OnLoadUserStateFailed/OnChannelInaccessible 四个回调的骨架处置）+ dispatcher 雏形（New/Edit/Delete → canonical messages 表写入协议） | 单测（状态机/fake）+ I（storage 往返/换号清旧）+ **S：smoke-recovery**（重启 catch-up、拔网重连、真实频道发消息→messages 落库） | U+I+S | T1.2 |
| **GATE-1** | **风险检查点**：真实频道消息可靠落库 + 重启 catch-up + 断线恢复全部成立。**不成立 → 停止实施，回到设计层重估（可能涉及 ADR 修订）** | 冒烟记录写入 progress.md | S | T1.3 |

### Phase 2：存储与配置

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T2.1 | goose 全量 P0 迁移（0002+：settings、channels、channel_settings、forward_rules、forwarded_messages、forwarding_stats、messages、message_revisions、system_audit_logs；embed + 启动即 Up） | 迁移文件 + runner | I（幂等：Up×2） | GATE-1 |
| T2.2 | sqlx pool + `Database` 帮助（事务/重试按 03 §1.4 语义） | 单测（mock）+ I | U+I | T2.1 |
| T2.3 | settings 中心：P0 scopes（system/forwarding/logging/ai）typed struct + 加载快照 + 热更回调 | U（校验/快照/回调） | U | T2.1 |
| T2.4 | repositories：channels、forward_rules（含 ChatRef 列）、forwarded_messages（五列 PK）、messages 写入协议（New/Edit/Delete 单一入口） | I（CRUD/事务/唯一键冲突语义） | I | T2.2 |

### Phase 3：转发引擎（GATE-2）

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T3.1 | `internal/domain`：ChatRef/PeerKind、MessageRef、SendRequest/SendStyle、ChannelMessage、MediaRef、Entity、Keyboard、AIResponse | 编译 + 类型单测 | U | T2.4 |
| T3.2 | 过滤链纯函数（03 §3.2 顺序；含相册聚合文本/媒体并集入参） | 表驱动全正反例（07 §1.1） | U | T3.1 |
| T3.3 | 相册聚合器（quiet/hard/集满三条件 + 全成员 flush） | 假时钟单测（07 §1.1） | U | T3.1 |
| T3.4 | Outbound MTProto：send_message（entities 透传、>4096 entity 边界分段）、send_file（attributes 保留）、forward_messages | fake Sender 测试 + S（smoke-send 单条） | U+S | T1.3, T3.1 |
| T3.5 | engine 编排：规则匹配（ChatRef）→ 过滤 → 去重 → 发送队列（容量 100 阻塞背压、单消费者、随机延迟、FloodWait 矩阵）→ 真实成败统计 | 全链 fake 单测（多规则/去重/失败不计成功） | U | T3.2–T3.4, T2.4 |
| T3.6 | 媒体下载：流式临时文件、大小上限、即时删除+启动清理 | U（临时文件生命周期）+ S | U+S | T3.4 |
| T3.7 | AI rewrite：`platform/ai`（openai-go、WithBaseURL、重试/降级矩阵） | U（fake openai；降级路径） | U | T3.5 |
| T3.8 | 底栏模板 + 源链接规则（含私有频道 stripped id） | U（占位符全量） | U | T3.5 |
| T3.9 | 回溯补发（GetHistory→完整过滤链→水位更新，上限 200） | U（fake Fetcher） | U | T3.5 |
| **GATE-2** | `cmd/smoke/smoke-forward`：真实源频道→目标频道端到端（文本/单媒体/相册三 case + 相册全成员 dedup 查库验证） | 冒烟记录 | S | T3.6–T3.9 |

### Phase 4：Rich 出站（ADR-008）

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T4.1 | `platform/botapi`：net/http 客户端（PeerKind 三态编码、429/Retry-After、5xx 退避、token 脱敏日志） | U（fake server：429/5xx/编码） | U | T3.4 |
| T4.2 | RichMarkdownNormalizer + validator + block 切分（32768/500/16 层/50 媒体/20 列） | **golden 样例测试**（07 §1.1 边界集） | U | T4.1 |
| T4.3 | Outbound 路由：Content→Renderer→Rich；lazy capability detection（method-not-supported 语义）→ fallback 链 | U（路由/fallback/flag）+ S（**smoke-rich**：真实 Rich 发送 + 不支持时降级观察） | U+S | T4.2 |

### Phase 5：生命周期 + WebUI（GATE-3）

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T5.1 | `internal/app`：service 抽象、supervisor（OWN_FATAL 退避/DEPENDENCY_UNAVAILABLE 等待）、Availability 实现、退出码（0/1/2/75）、readiness barrier | U（生命周期状态机、假服务编排顺序） | U | GATE-2 |
| T5.2 | webapi 骨架：标准库路由、`/api/health`、auth（opaque session、Cookie、失败锁定、RemoteAddr）、audit 中间件 | httptest（鉴权/锁定/豁免表） | U | T5.1 |
| T5.3 | 业务 API：settings/forwarding（CRUD+backfill）/channels/userbot 登录向导三步/system（pause/resume/restart=exit 75/log-level/audit）/WS 日志流 | httptest + I（repo 往返） | U+I | T5.2 |
| T5.4 | 前端：Vite+Vue3+TS+Naive UI 脚手架 → fetch 封装 → Login/Dashboard/Forwarding/Channels/Logs/System/Settings 七页 → `pnpm build` → `go:embed` | `vue-tsc --noEmit` + build + **S：浏览器全流程（登录→建规则→看日志→重启）** | U+S | T5.3 |
| **GATE-3** | 浏览器完成：规则 CRUD → 真实频道触发转发 → 实时日志可见 → WebUI 重启按钮生效 | 冒烟记录 | S | T5.4 |

### Phase 6：部署与验收（GATE-4 = P0 Done）

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T6.1 | `sakura-bot healthcheck` 子命令（net/http GET 本机 /api/health） | 手测 | S | T5.2 |
| T6.2 | Dockerfile（多阶段+distroless+HEALTHCHECK 子命令）、compose.yaml + compose.full.yaml、deploy/sakura-bot.service + qdrant.service 示例 | `docker compose config -q`（两组合）+ 本地容器起停 | S | GATE-3 |
| T6.3 | README（快速开始：.env→login-user→WebUI；部署两形态；冒烟清单） | 文档 | — | T6.2 |
| T6.4 | **P0 验收执行**：07 §3 P0 checklist 逐项（含 gap-too-long 定向恢复、相册全成员 dedup、24h 稳定性+内存记录） | 验收报告（progress.md） | S | T6.2, T6.3 |
| **GATE-4** | P0 完成 → 交付用户验收 | — | — | T6.4 |

## 5. 测试门禁（汇总）

- 每个 commit：`gofmt -l` 为空、`golangci-lint run`（depguard 自 T1.1 起）、`go test -race ./...` 全绿。
- `I` 标注任务：`go test -race -tags integration ./...`（MySQL service container）。
- `S` 标注任务：对应 smoke 脚本手动执行并把输出摘要记入 progress.md。
- GATE（1/2/3/4）：全部为**硬门禁**——未通过不得进入后续 Phase；GATE-1 失败触发设计层重估流程。

## 6. 与设计的对应关系

- 每任务的模块/接口/schema 直接引用冻结设计（01 §n / 02 §n / 03 §n），实施中发现设计不可行 → **停止并回报**，修订设计文档（标注修订号）并获用户确认后继续——不得静默偏离（ADR 索引门禁规则）。
