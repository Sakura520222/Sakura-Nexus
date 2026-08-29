# Sakura-Bot Go P0 实施计划（R1）

- 状态：📝 R1 修订版，待用户核对 diff——**批准前零实现代码**
- 日期：2026-08-29（R0 同日；R1 落实用户审核的 4 项必改 + Gate 加强 + 前移/命名/计数修正）
- 依据：已冻结的 [ADR 001–008](../decisions/README.md) 与总体设计 [overview](../design/overview.md) + 01–07（R3.1.1）
- 规模：**32 个任务 + 4 个正式 Gate**（GATE-1/2/3/4）+ 1 个非 Gate 冒烟检查点（Rich smoke checkpoint）

## 1. 范围锁定（P0 = A + B + C，ADR-007）

**做**：Go skeleton/lifecycle、config、MySQL migrations、gotd User/Bot + 全部持久状态、updates/recovery、转发引擎全量、AI rewrite、媒体/相册、Outbound/Rich 基础设施、WebUI（auth/API/7 页面）、systemd/Docker、测试、production 验收。

**不做（即使设计已冻结）**：Qdrant、Summary、Dense RAG、讨论会话、投稿/投票——P0 不创建 `internal/summary`、`internal/rag`、`internal/conversation`、`internal/platform/qdrant` 包，不建 P1/P2 业务表。

**全新实现铁律（用户重申，2026-08-29）**：**不从旧 Sakura-Bot / TG-Forwarder 迁移任何内容**——不迁移数据（02 §5：全新初始化，无导入工具）、不复制代码、不逐行翻译旧实现。旧项目的全部价值已固化在 [调研报告](../research/) 中，仅作为**业务需求与语义参考**；v2 实现完全依据已冻结的 ADR 与总体设计（01–07），从 `T0.1` 的第一行 Go 开始全新编写。

## 2. 总原则

1. **风险前置**：最大工程风险 = gotd 双客户端稳定性 + 持久状态闭环。**第 5 个任务（T1.1）即首次连接真实 Telegram**，位于任何转发引擎/WebUI/业务代码之前；GATE-1 失败 → 停止实施，回设计层重估。
2. **正式代码从第一天生活在正式 lifecycle 中**（R1）：Phase 1 的 smoke 允许最小 one-shot runner（风险实验代码可以没有完整 lifecycle）；**GATE-1 通过后第一件事就是 T2.0 production lifecycle**，之后所有业务 service（Forwarding/Outbound/WebServer）直接以正式 service 形态实现，避免 Phase 5 大面积接线重构。
3. **测试门禁**：每任务合入前——`gofmt -l` 空、`golangci-lint run`（`.golangci.yml` + depguard 自 T0.2 存在、T1.1 起生效）、`go test -race ./...` 全绿；`I` 任务加跑 `go test -race -tags integration ./...`（GitHub Actions MySQL service container / 本地 compose）。
4. **Commit 粒度**：每任务 1–3 个 commit（Conventional Commits），每个 commit 单独可编译、测试绿，禁止跨任务大 commit。
5. **冒烟（S 类）**：`cmd/smoke/` 手动执行、不进 CI、需真实 `.env` 凭据；输出摘要记入 progress.md。
6. **单一实现原则**：同一逻辑只实现一次——CLI 与 WebUI 复用同一 `UserAuthService`/auth flow 状态机（WebUI 只是 presentation adapter）；MessageRepository 在 T1.3 正式实现一次，后续不再重写。

## 3. 任务依赖 DAG

```text
Phase 0 地基                 Phase 1 Telegram 风险验证（GATE-1）
T0.1 module 骨架 ──┬─→ T1.0 最小 domain 类型（ChatRef/PeerKind/MessageRef/
T0.2 渐进 CI(仅Go) │       ChannelMessage/MediaRef/Entity）
     + .golangci  └─→ T1.1 Bot 连通（Auth().Bot+Self()）+ 0001 迁移
T0.3 config .env          + 最小 sqlx 池（T2.2 完善）
                               ↓
                          T1.2 User 连通 + login-user CLI（UserAuthService 唯一实现）
                               ↓
                          T1.3 状态闭环（Manager+UpdateHandler+UpdateHook+
                               AffectedHook+StateStorage+dispatcher）+
                               正式 MessageRepository/canonical writer
                               ↓
                          ══ GATE-1：真实频道消息可靠落库 + 重启 catch-up + 断线恢复 ══
                               ↓（不成立 → 停止，回设计层）
Phase 2 正式生命周期 + 存储/配置
T2.0 lifecycle/supervisor/Availability/退出码 → T2.1 0002_p0_business.sql（7 表）
→ T2.2 Database 完善（重试语义/事务帮助）→ T2.3 settings 中心
→ T2.4 repositories（Channel/Rule/Forwarded/Stats/Audit）
                               ↓
Phase 3 转发引擎（GATE-2）
T3.1 outbound domain 补全（SendRequest/SendStyle/Keyboard/SentMessage/AIResponse）
→ T3.2 过滤链 → T3.3 相册聚合 → T3.4 Outbound MTProto → T3.5 engine+队列
  （含 contiguous cursor 语义）→ T3.6 媒体临时文件 → T3.7 AI rewrite
→ T3.8 底栏 → T3.9 backfill（contiguous cursor）
                               ↓
                          ══ GATE-2：smoke-forward 端到端（文本/媒体/相册+全成员 dedup）══
                               ↓
Phase 4 Rich 出站（ADR-008）
T4.1 botapi HTTP → T4.2 normalizer/validator/splitter(golden) → T4.3 路由+lazy capability
                               ↓
                          ── Rich smoke checkpoint（非 Gate：真实 Rich 发送 + 降级观察）──
                               ↓
Phase 5 服务接线 + WebUI（GATE-3）
T5.1 接线收口（WebServer/全部 service 注册、readiness barrier、exit 75 全链）
→ T5.2 webapi 骨架 → T5.3 业务 API（userbot 向导复用 UserAuthService）
→ T5.4 前端 7 页面 + embed + CI 追加前端 job
                               ↓
                          ══ GATE-3：浏览器全流程 ══
                               ↓
Phase 6 部署与验收（GATE-4 = P0 Done）
T6.1 healthcheck 子命令 → T6.2 Docker/compose 双文件/systemd + CI 追加 docker job
→ T6.3 README → T6.4 P0 验收执行（07 §3 checklist + 24h）
```

**首次连接真实 Telegram：T1.1（全计划第 5 个任务）。**

## 4. 迁移划分（R1 修正）

| 迁移 | 表 | 服务的任务 |
|---|---|---|
| `0001_telegram_foundation.sql` | gotd_sessions、telegram_update_states、telegram_channel_states、telegram_peers、telegram_peer_aliases、**messages、message_revisions** | T1.1 建迁移；T1.3 使用——**GATE-1 的风险验证基座** |
| `0002_p0_business.sql` | settings、channels、channel_settings、forward_rules、forwarded_messages、forwarding_stats、system_audit_logs | T2.1 |

## 5. 任务表（验证：U=单元 I=集成 S=手动冒烟）

### Phase 0：工程地基

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T0.1 | Go module 骨架：`go.mod`（**`module github.com/Sakura520222/Sakura-Bot`**，从第一天用 canonical path）、`cmd/sakura-bot/main.go`（仅 --version）、`internal/{app,config,logging,platform/{mysql,telegram,botapi,ai},forwarding,webapi,domain}` 空包、Makefile、`.env.example`（01 §6.1 全量） | 可编译空项目 | `go build ./...` | — |
| T0.2 | **渐进 CI（R1：仅 Go 部分）**：gofmt、golangci-lint（**交付 `.golangci.yml` + depguard 规则落点**）、`go test -race`、`go build`、MySQL integration 基础设施（service container + `integration` build tag 骨架）。web/Docker job **不在此阶段创建**（分别由 T5.4/T6.2 追加），不用 `if: exists(...)` 假绿 | `.github/workflows/ci.yml` + `.golangci.yml` | push 后 CI 绿 | T0.1 |
| T0.3 | `internal/config`：.env struct + 必填校验 + 加载 | U：缺必填报全量缺失项 | U | T0.1 |

### Phase 1：Telegram 风险验证（GATE-1）

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T1.0 | **最小 domain 类型（R1 前移）**：ChatRef/PeerKind、MessageRef、ChannelMessage、MediaRef、Entity——dispatcher 与 canonical writer 的输入输出类型，保证 repository **不吃 gotd `tg.Message`** | 类型 + 单测 | U | T0.3 |
| T1.1 | **Bot 客户端最小连通**：`platform/telegram.BotClient` + MySQL `session.Storage` + **`0001_telegram_foundation.sql`（§4 七表）** + 最小 sqlx 池（T2.2 完善）+ 登录验证流程 **`Auth().Bot(ctx, token)` → `Self(ctx)` → 校验 `self.Bot == true` / ID / username**（MTProto，不调 HTTP getMe） | `cmd/smoke/smoke-bot`：登录→Self→打印身份→优雅关闭（session 落库） | **S（首次真实 TG）** + I（storage 往返） | T1.0, T0.2 |
| T1.2 | **User 客户端最小连通**：`UserClient` + `sakura-bot login-user` CLI 子命令——**UserAuthService/auth flow 状态机在此唯一实现**（手机号/验证码/2FA），后续 T5.3 WebUI 向导仅作 presentation adapter 复用，不得重写第二套 | `cmd/smoke/smoke-user`：登录→收一条 update→打印→重启免登录 | S + I | T1.1 |
| T1.3 | **状态闭环（R1 扩充）**：① `telegram_update_states/channel_states/peers/aliases` 的 gotd 存储实现；② **完整 Manager wiring：`updates.Manager` + `UpdateHandler` + `updhook.UpdateHook` + `updhook.AffectedHook`（自身 read/delete 返回的 affectedMessages 同步更新本地 PTS）+ `StateStorage` + dispatcher 雏形**；③ 四个 recovery callback 处置（channel 级补抓 / global 全量 reconciliation / inaccessible→unavailable）；④ **正式 MessageRepository / canonical writer**（New/Edit/Delete 单一写入协议，02 §2.3） | U（状态机/fake writer）+ I（storage 往返/换号清旧）+ **S：smoke-recovery**（重启 catch-up、拔网重连、真实频道发消息→messages 落库） | U+I+S | T1.2 |
| **GATE-1** | 真实频道消息可靠落库 + 重启 catch-up + 断线恢复全部成立（smoke-bot/user/recovery 为其验证内容）。**不成立 → 停止实施，回设计层重估（可能涉及 ADR 修订）** | 冒烟记录入 progress.md | S | T1.3 |

### Phase 2：正式生命周期 + 存储/配置

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T2.0 | **production lifecycle（R1 前移）**：service 抽象、supervisor（OWN_FATAL 指数退避重启该服务 / DEPENDENCY_UNAVAILABLE 等待 Availability）、退出码（0/1/2/75）、readiness barrier；后续全部业务 service 以此形态实现 | U（生命周期状态机、假服务编排/关闭顺序） | U | GATE-1 |
| T2.1 | `0002_p0_business.sql`（§4 七表）+ goose runner（embed + 启动即 Up） | 迁移文件 | I（幂等 Up×2） | T2.0 |
| T2.2 | Database 完善：事务帮助、幂等 retry 语义（03 §1.4：事务提交状态未知不得自动重放） | U + I | U+I | T2.1 |
| T2.3 | settings 中心：P0 scopes（system/forwarding/logging/ai）typed struct + 快照 + 热更回调 | U | U | T2.1 |
| T2.4 | repositories（R1：MessageRepo 已在 T1.3，此处只补）：ChannelRepo、RuleRepo（含 ChatRef 列）、ForwardedRepo（五列 PK）、StatsRepo、AuditRepo | I（CRUD/唯一键冲突语义） | I | T2.2 |

### Phase 3：转发引擎（GATE-2）

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T3.1 | outbound domain 补全（R1 拆分）：SendRequest/SendStyle/Keyboard/SentMessage/AIResponse | 编译 + 类型单测 | U | T2.4 |
| T3.2 | 过滤链纯函数（03 §3.2 顺序；相册聚合文本/媒体并集入参） | 表驱动全正反例 | U | T3.1 |
| T3.3 | 相册聚合器（quiet 400ms 重置 / hard 2s / 集满 10；全成员 flush） | 假时钟单测 | U | T3.1 |
| T3.4 | Outbound MTProto：send_message（entities 透传、>4096 entity 边界分段）、send_file（attributes 保留）、forward_messages | fake Sender + S（单条发送冒烟） | U+S | T1.3, T3.1 |
| T3.5 | engine 编排：规则匹配（ChatRef）→ 过滤 → 去重 → 发送队列（容量 100 阻塞背压、单消费者、随机延迟、FloodWait 矩阵）→ 真实成败统计 → **contiguous cursor（R1 必改 4：见 §6）** | 全链 fake 单测（多规则/去重/失败不计成功/**cursor 不越过 transient failure**） | U | T3.2–3.4, T2.4 |
| T3.6 | 媒体下载：流式临时文件、大小上限、即时删除 + 启动清理 | U + S | U+S | T3.4 |
| T3.7 | AI rewrite：`platform/ai`（openai-go、WithBaseURL、重试/降级矩阵） | U（fake openai、降级路径） | U | T3.5 |
| T3.8 | 底栏模板 + 源链接规则（私有频道 stripped id） | U | U | T3.5 |
| T3.9 | backfill（GetHistory→完整过滤链→水位更新，上限 200）——**cursor 语义与 T3.5 一致（§6）**；验证含恢复用例 | U（fake Fetcher + 恢复场景） | U | T3.5 |
| **GATE-2** | `cmd/smoke/smoke-forward`：真实源→目标端到端（文本/单媒体/相册三 case + 相册全成员 dedup 查库验证） | 冒烟记录 | S | T3.6–3.9 |

### Phase 4：Rich 出站（ADR-008）

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T4.1 | `platform/botapi`：net/http 客户端（PeerKind 三态编码、429/Retry-After、5xx 退避、token 脱敏日志） | U（fake server） | U | T3.4 |
| T4.2 | RichMarkdownNormalizer + validator + block 切分（32768/500/16 层/50 媒体/20 列） | **golden 样例**（07 §1.1 边界集） | U | T4.1 |
| T4.3 | Outbound 路由：Content→Renderer→Rich；lazy capability detection（method-not-supported 语义）→ fallback 链 | U + S（**Rich smoke checkpoint**：真实 Rich 发送 + 不支持时降级观察——检查点，非 Gate） | U+S | T4.2 |

### Phase 5：服务接线 + WebUI（GATE-3）

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T5.1 | 接线收口（R1：lifecycle 主体已在 T2.0）：WebServer 及全部 service 正式注册、readiness barrier 完整化、WebUI restart→exit 75 全链验证 | U + S | U+S | GATE-2, T4.3 |
| T5.2 | webapi 骨架：标准库路由、`/api/health`、auth（opaque session、Cookie、失败锁定、RemoteAddr）、audit 中间件 | httptest | U | T5.1 |
| T5.3 | 业务 API：settings/forwarding（CRUD+backfill）/channels/**userbot 向导三步（复用 T1.2 UserAuthService，presentation adapter）**/system（pause/resume/restart=75/log-level/audit）/WS 日志流 | httptest + I | U+I | T5.2 |
| T5.4 | 前端：Vite+Vue3+TS+Naive UI 脚手架 → fetch 封装 → 七页（Login/Dashboard/Forwarding/Channels/Logs/System/Settings）→ `pnpm build` → `go:embed`；**CI 追加前端 job（pnpm install/vue-tsc/build）** | vue-tsc + build + **S：浏览器全流程** | U+S | T5.3 |
| **GATE-3** | 浏览器完成：登录→建规则→真实频道触发转发→实时日志可见→WebUI 重启生效 | 冒烟记录 | S | T5.4 |

### Phase 6：部署与验收（GATE-4 = P0 Done）

| # | 任务 | 交付物 | 验证 | 依赖 |
|---|---|---|---|---|
| T6.1 | `sakura-bot healthcheck` 子命令（net/http GET 本机 /api/health） | 手测 | S | T5.2 |
| T6.2 | Dockerfile（多阶段+distroless+HEALTHCHECK 子命令）、compose.yaml + compose.full.yaml、deploy/sakura-bot.service + qdrant.service 示例；**CI 追加 docker job（docker build + 双 compose config -q）** | compose config 两组合通过 + 本地容器起停 | S | GATE-3 |
| T6.3 | README（快速开始、两形态部署、冒烟清单） | 文档 | — | T6.2 |
| T6.4 | **P0 验收执行**：07 §3 P0 checklist 逐项（gap-too-long 定向恢复、相册全成员 dedup、24h 稳定性 + 内存记录） | 验收报告（progress.md） | S | T6.2, T6.3 |
| **GATE-4** | P0 完成 → 交付用户验收 | — | — | T6.4 |

## 6. Contiguous cursor 语义（R1 必改 4，冻结为引擎不变量）

`forward_rules.last_message_id` 是**最高连续 terminal cursor**，不是「见过的最大 message ID」：

```text
terminal（可推进 cursor）：filtered/skipped、dedup already-sent、send success
non-terminal（不得越过推进）：transient send failure（FloodWait 超限、重试耗尽、临时错误）

例：100 failed；101、102 success → cursor 保持 99
下次 backfill：GetHistory(minID=99) → 100 重试 → 101/102 dedup 跳过 → 100 成功后连续推进至 102
```

- 否则「failed + backfill recoverable」只是文档承诺。T3.5（引擎写入侧）与 T3.9（backfill 读取侧）的验证条件都必须包含上述恢复用例。
- 永久性失败（如消息已被源删除、目标频道被踢）按配置策略处理：有限次重试后标记 terminal 并记录，避免 cursor 永久卡死。

## 7. 测试门禁（汇总）

- 每个 commit：`gofmt -l` 空、`golangci-lint run`（`.golangci.yml` + depguard）、`go test -race ./...` 全绿。
- `I` 任务：`go test -race -tags integration ./...`（MySQL service container；T0.2 搭建基础设施）。
- `S` 任务：smoke 脚本手动执行，输出摘要记入 progress.md。
- GATE 1/2/3/4 为硬门禁；Phase 4 的 Rich 冒烟是检查点（checkpoint）非 Gate——命名严格区分，不使用 GATE-1a/1b/2b 之类的分叉编号。

## 8. 与设计的对应关系

- 每任务的模块/接口/schema 直接引用冻结设计（01 §n / 02 §n / 03 §n）。实施中发现设计不可行 → **停止并回报**，修订设计文档（标注修订号）并获用户确认后继续——不得静默偏离（ADR 索引门禁规则）。
