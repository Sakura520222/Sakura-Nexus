# 01 运行时与组件

- 状态：✅ 已冻结（R3.1.1，2026-08-29 总体设计批准）
- 受约束 ADR：[001](../decisions/001-telegram-stack.md) · [002](../decisions/002-runtime-model.md) · [003](../decisions/003-webui-form.md) · [005](../decisions/005-go-libraries.md) · [008](../decisions/008-rich-message-transport.md)

## 1. 进程与生命周期

### 1.1 启动序列（composition root 顺序，失败即按策略终止）

```text
1. main：load .env → 校验必填 → setup slog（含环形缓冲 handler）
2. MySQL：连接池 + goose Up（启动即迁移，embed SQL）
3. settings 中心：加载 MySQL settings → 内存快照（各 scope Go struct 校验）
4. platform 构造（唯一触碰具体库的层）：
   a. repositories（sqlx）
   b. BotClient（gotd，STRICT：失败 → 退出码 1，systemd 拉起重试）
   c. UserClient（gotd，LENIENT：失败 → 进入 degraded 等待，WebUI 登录向导可用）
   d. Outbound（platform/telegram 内部含 Rich MessageRenderer 路由，见 §4）
   e. AI client（platform/ai，openai-go + WithBaseURL）
   f. Qdrant client（P1 起；P0 不构造）
5. 构造全部领域 service（构造函数注入各自接口）——此阶段**不启动任何 service**
6. 全部注册到 lifecycle（含 WebServer：它是普通注册 service，CORE）
7. supervisors 统一启动全部 service
8. readiness barrier：全部 CORE service 就绪后 → ready（就绪通知 / health 转 ok）
```

### 1.2 组件注册：`service` 抽象

```go
// internal/app/lifecycle.go
type service interface {
    Name() string
    Run(ctx context.Context) error    // 阻塞至 ctx 取消；返回 error = OWN_FATAL（见 §1.3）
    Shutdown(ctx context.Context) error
}
```

App 按启动顺序持有 `[]service`；关闭按注册**逆序**调用 `Shutdown`（各自超时受 root shutdown 预算约束）。

### 1.3 服务健康模型（两级错误 × 两种处置）

| 状态 | 语义 | 处置 |
|---|---|---|
| 正常运行 | — | — |
| **DEPENDENCY_UNAVAILABLE** | 依赖（如 UserClient）不可用 | **service 自身保持存活**：在 Run 内等待 `Availability` 恢复信号（§1.3 接口），不退出、不重启。例：UserClient 掉线时 Forwarding/Summary/RAG 只是暂停工作，不重启 |
| **OWN_FATAL** | 自身不可恢复错误 | supervisor **指数退避重启该 service**（仅该服务） |
| CORE fatal | CORE 服务（MySQL 连接、BotClient、WebServer）OWN_FATAL | 全局 cancel → 优雅退出（exit 1）→ systemd `Restart=on-failure` 兜底 |

- 可恢复错误（断线、FloodWait、429/5xx、DB 闪断）在 service/platform 内部 retry，永不冒泡为 Run 返回。
- 依赖状态模型：platform 客户端实现**可重复连接的状态接口**，不使用一次性 close 的 ready channel（closed channel 永久可读且不可重开，无法表达「连接→断线→重连→再断线」循环）：

```go
type Availability interface {
    IsReady() bool
    WaitReady(ctx context.Context) error        // 阻塞至就绪或 ctx 取消
    SubscribeState() <-chan DependencyState     // 每次状态翻转发送新值；实现方只发送，channel 由订阅方持有
}
// DependencyState = Ready | Unavailable（内部以 mutex/atomic + generation channel 实现）
```

  service 处于 DEPENDENCY_UNAVAILABLE 时在 Run 内 `WaitReady` / 监听 `SubscribeState`，恢复即继续，全程不重启。
- panic：仅 supervisor 的 goroutine boundary recover → 记 stack → 等同该服务 OWN_FATAL。禁止业务代码裸 recover 继续跑。

### 1.4 优雅退出序列与退出码

```text
SIGTERM/SIGINT 或 CORE fatal → cancel root ctx
→ 各 service Run 返回（停止接收新任务）
→ 逆序 Shutdown（drain 在途任务；总预算 SHUTDOWN_TIMEOUT_SECONDS，默认 30s）
→ UserClient / BotClient close（session 与 update state 落库，见 02 §2.1）
→ MySQL 池关闭
→ exit：0 正常 / 1 CORE fatal / 2 配置缺失 / **75 重启请求**（WebUI restart：优雅退出后以 75 退出——非零退出码使 systemd `Restart=on-failure` 与 docker `unless-stopped` 均会拉起新进程）
```

### 1.5 health endpoint

- **公开** `GET /api/health`（无鉴权）：仅 `{"status":"ok|degraded|down","version":"…","uptime":…}`。Docker HEALTHCHECK 使用。
- **组件细项**（mysql/bot/user/forwarding/scheduler 各自状态、degraded 原因、恢复入口）在**鉴权后**的 `GET /api/system/status`。
- systemd 部署使用 `Restart=on-failure`（非 watchdog；文档与注释不得使用 watchdog 表述）。

## 2. 代码组织与依赖方向

### 2.1 包结构

```text
sakura-nexus/
├── cmd/sakura-nexus/main.go      # composition root：.env → App 构建 → Run。无业务逻辑
├── internal/
│   ├── app/          # App 组合、lifecycle（service 注册/逆序关闭/supervisor）、health 聚合
│   ├── config/       # .env 加载（struct）+ settings 中心（MySQL，scope→struct 校验+快照+变更通知）
│   ├── logging/      # slog setup、环形缓冲 handler、组件 logger 工厂
│   ├── platform/     # 基础设施具体实现（全项目唯一 import gotd/qdrant/sqlx/openai-go 的层）
│   │   ├── mysql/    # sqlx 池、goose runner、repositories 实现、telegram state 存储
│   │   ├── telegram/ # gotd UserClient / BotClient、updates 分发、Outbound（内含 Rich MessageRenderer）
│   │   ├── botapi/   # ADR-008：sendRichMessage net/http 客户端（限流/429/重试）
│   │   ├── qdrant/   # Qdrant client 适配（P1；collection/alias/reindex 底层调用）
│   │   └── ai/       # openai.go：openai-go 封装（chat / embedding / vision / classification）
│   ├── forwarding/   # 转发领域：engine、filters、album 聚合、规则模型、自有最小接口
│   ├── summary/      # P1：调度、增量抓取、水位、报告组装
│   ├── rag/          # P1 起：ingest、retrieval、context builder；P2：rerank/memory/agent
│   ├── conversation/ # P2：讨论会话、记录/触发分离
│   ├── webapi/       # HTTP 路由、DTO、auth、WebSocket、RAG Query Harness
│   └── domain/       # 跨领域纯数据类型（MessageRef、ChatRef、MessageContent、AIResponse…）
├── web/              # Vue 3 SPA（pnpm build → dist/ → go:embed）
├── migrations/       # goose SQL（//go:embed）
├── Makefile
└── Dockerfile / compose.yaml / deploy/
```

### 2.2 依赖方向规则（CI 强制）

```text
cmd → app → {forwarding, summary, rag, conversation, webapi, config, logging}
领域包 → 只 import domain + 自身接口定义
platform/* ← 仅 app 引用（构造后注入领域）
```

- golangci-lint **depguard** 强制：`internal/forwarding|summary|rag|conversation|webapi` 禁止 import `github.com/gotd/td`、`github.com/qdrant/go-client`、`github.com/jmoiron/sqlx`、`github.com/openai/openai-go`；`net/http` 仅 webapi 允许。
- **禁止** `App` 暴露 `DB / Qdrant / UserTG / BotTG / AI` 字段给领域；禁止 `internal/utils`、`internal/common`、`internal/manager` 类大杂烩包。

### 2.3 接口由消费者定义

每个领域包定义**自己的最小接口**；同一个 platform 实现凭 Go structural typing 同时满足各消费者。例：

```go
// forwarding 包内
type Sender interface {                       // 最小面：转发只需要这些
    Send(ctx context.Context, req domain.SendRequest) (domain.SentMessage, error)
}
type Fetcher interface {
    OnNewMessage(chats []int64, h func(ctx context.Context, m domain.ChannelMessage)) (cancel func())
    GetHistory(ctx context.Context, chatID int64, minID int64, limit int) ([]domain.ChannelMessage, error)
    DownloadMedia(ctx context.Context, m domain.ChannelMessage, dest string) error
    JoinChannel(ctx context.Context, chatID int64) error
}

// summary 包内（P1）
type Sender interface {                       // summary 自己的最小面（可含 Rich 内容发送）
    Send(ctx context.Context, req domain.SendRequest) (domain.SentMessage, error)
}

// conversation 包内（P2）同理由自己定义
```

`MessageRenderer` **不是领域接口**：它是 `platform/telegram` 内部实现细节（Outbound 收到 `domain.MessageContent` 后自行决定 Rich 渲染与 fallback）。领域只知道「发送这份 MessageContent」。

## 3. 接口边界清单

### 3.1 P0 需要的接口（各消费者定义）

```go
// forwarding：Sender / Fetcher（见 §2.3）/ RuleRepo / ForwardedRepo
type RuleRepo interface {
    ListEnabled(ctx context.Context) ([]Rule, error)
    Create / Update / Delete / SetEnabled(...)
}
type ForwardedRepo interface {
    Exists(ctx context.Context, src domain.MessageRef, target domain.ChatRef) (bool, error) // R3.1.1：target 亦 ChatRef
    Record(ctx context.Context, rec ForwardedRecord) error
    CleanupBefore(ctx context.Context, retention time.Duration) (int64, error)
}

// summary（P1）：Sender（自有最小面）+ Fetcher.GetHistory + SummaryRepo + CursorRepo
// rag（P1）：Embedder / Retriever / MessageRepo / ReindexStateRepo
```

### 3.2 P1/P2 预留边界（只签名，不做仿真实现）

```go
// rag（P1 定义）
type Retriever interface {
    Retrieve(ctx context.Context, q RetrievalQuery) ([]Candidate, error) // P1: dense+filter 实现
}
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}
// P2（仅签名，P1 提供 no-op：返回 ErrNotImplemented / 空结果）
type Reranker interface { Rerank(ctx context.Context, q string, c []Candidate, topK int) ([]Candidate, error) }
type QueryAnalyzer interface { Analyze(ctx context.Context, q string) (QueryPlan, error) }
type VisionProcessor interface { Describe(ctx context.Context, media domain.MediaRef) (domain.VisionDescription, error) }
type MemoryStore interface { /* P2 定义 */ }
```

规则：**只留边界，不写仿真实现**；no-op 实现放各领域包内一处（`noop.go`），注入与否由 app 按 P0/P1/P2 构造决定。

## 4. 出站消息抽象

### 4.1 统一模型（不为 Rich Message 长出平行业务 API）

```go
// domain：Telegram 通用 chat 引用（R3.1）
// 裸 ID 数值空间在 user/chat/channel 间重叠，任何 chat 引用必须携带 kind（Bot API 正负/-100 编码的本质即补充类型信息）
type PeerKind uint8

const (
    PeerUser PeerKind = iota
    PeerChat           // basic group
    PeerChannel        // channel 与 supergroup（MTProto 同类）
)

type ChatRef struct {
    Kind PeerKind
    ID   int64 // MTProto 裸 ID（正数）
}

// domain 承载数据；各消费者以自己的最小 Sender 接口引用
type SendRequest struct {
    Chat     ChatRef           // R3.1：kind + raw id
    Style    SendStyle         // Auto（默认）/ Plain / Rich
    Content  *MessageContent   // 结构化内容（AI 输出：text + 可选 media 引用 + metadata）
    Text     string            // Plain 直发文本
    Entities []Entity          // 可选：原 entities 透传（转发复制语义）
    Media    []MediaRef        // 本地文件或媒体引用
    Caption  string
    ReplyTo  int64             // 回复目标消息
    Markup   *Keyboard         // inline keyboard
    Silent   bool
}
```

### 4.2 实现内部路由（platform/telegram/Outbound，领域不可见）

```text
SendRequest
   ├─ Content != nil → 内部 MessageRenderer（normalizer + validate + block 切分）
   │      ├─ 成功 → BotAPIRichSender（sendRichMessage，ADR-008 例外通道）
   │      └─ formatting reject → safe fallback → 普通 formatting / 纯文本（MTProto）
   └─ 否则 → gotd MTProto：send_message（entities 透传）/ send_file（media/相册）/ forward
```

### 4.3 能力覆盖矩阵（两条通道都支持，业务无感）

| 能力 | MTProto 通道 | Bot API Rich 通道（ADR-008） |
|---|---|---|
| reply 指定消息 | ✅ reply_to | ✅ reply_parameters（讨论线程用它，不用 message_thread_id） |
| inline keyboard | ✅ | ✅ reply_markup |
| 媒体/相册 | ✅ send_file | ✅ 媒体块（≤50 附件/条） |
| entities 透传 | ✅ | 由 renderer 从 Content 重新生成 |
| 长消息 | 分段（4096） | block 边界切分（32,768 字符/500 blocks/16 层/表格≤20 列） |
| 流式预览 | — | Draft（**仅私聊**；群/讨论群用 typing 状态 + 一次发送） |

## 5. 并发规则

### 5.1 goroutine 所有权

- 每个 goroutine 有唯一 owner（创建它的 service）；owner 负责其生命周期与 ctx 传递。
- supervisor 为每个 service 起一个 goroutine；service 内部 worker 由该 service 统一 `errgroup`（派生 ctx）管理，Run 返回即全部回收。

### 5.2 channel 规则

- 每个 channel 必须有：**唯一 owner goroutine（接收侧）**、**显式容量**、**明确关闭责任（发送方唯一时发送方 close；多发送方时禁止 close，用 ctx 取消 + drain）**。
- channel 短清单（禁止网状自由 channel）：
  - 转发发送队列（容量 100，**阻塞背压**：转发不允许丢，满则等待/告警）
  - 日志环形缓冲→WS 推送（容量 512，**丢弃最旧**）
  - **RAG derived-index 队列**（P1，容量 1000）：**只是加速器——durability ≠ queue delivery**。New/Edit 索引任务队满时可丢并计数（`index_state=pending` 留痕，repair 补做）；Delete invalidation 入队失败只计数/告警，**不得阻塞 canonical MySQL commit**——Delete 的持久保证在事务内置的 `delete_pending` 状态，最终删除由 repair 状态机收敛（05 §4，违反 ADR-006 的「已删内容仍可检索」不可能发生）
  - 数据流边界：`Telegram event → MySQL canonical + revision（永不允许因队满丢失，先于队列写入）→ derived-index 队列 → AI augmentation / embedding / Qdrant`
  - 客户端依赖状态：`Availability` 接口（§1.3；不使用一次性 close channel）
  - 跨组件配置事件：不走全局 channel 总线，用 settings 中心订阅回调（订阅者明确、可枚举）。

### 5.3 Telegram update 分发

- gotd `updates.Dispatcher` 在 platform/telegram 内注册；按 chat/事件类型路由到领域 handler（forwarding：NewMessage；P1 rag：New/Edit/Delete；P2 conversation：讨论群消息）。
- 领域 handler 收到 `domain.ChannelMessage`（已剥离 gotd 类型）；**handler 内 panic 由 dispatcher 边界 recover 记日志**，不影响其他 handler 与连接。

## 6. 配置体系

### 6.1 `.env` v2（bootstrap only；完整文件即 `.env.example`）

```ini
# ---- MySQL（必需）----
MYSQL_HOST=localhost
MYSQL_PORT=3306
MYSQL_USER=sakura
MYSQL_PASSWORD=
MYSQL_DATABASE=sakura_bot
MYSQL_MAX_OPEN_CONNS=5

# ---- Telegram 唯一 Bot（必需；token 同时供 ADR-008 Rich 通道复用）----
TELEGRAM_BOT_TOKEN=
TELEGRAM_API_ID=
TELEGRAM_API_HASH=

# ---- 真实账号（P0 转发依赖；未配置时经 WebUI 登录向导生成）----
USERBOT_PHONE_NUMBER=

# ---- Qdrant（P1 起必需；P0 留空即可）----
QDRANT_URL=
QDRANT_API_KEY=

# ---- WebUI（必需）----
WEBUI_HOST=127.0.0.1   # 裸机默认仅本机（安全默认）；Docker 部署经 compose override 改 0.0.0.0
WEBUI_PORT=8080
WEBUI_USERNAME=admin
WEBUI_PASSWORD=

# ---- 可选 ----
LOG_LEVEL=info
SHUTDOWN_TIMEOUT_SECONDS=30
```

### 6.2 MySQL settings scope（有 schema 的分层配置，非 JSON 垃圾桶）

| scope | 内容（Go struct 定义字段，编译期 schema，写入前校验） | 期 |
|---|---|---|
| `system` | 语言、启动通知、维护开关、**telegram_admin_ids**（Bot 管理命令白名单；空 = Telegram 管理命令全部禁用，用户聊天/问答不受影响；不设独立 admins 表） | P0 |
| `forwarding` | show_default_footer、dedup_days、content_dedup、默认延迟区间 | P0 |
| `logging` | level（覆盖 .env 的运行时级） | P0 |
| `ai` | 同一 typed `AISettings`，字段按期启用——**P0**：base_url / api_key / rewrite_model / temperature / timeout（P0 转发 AI 改写即依赖）；**P1**：+ summary_model / embedding_model / embedding_dimension；**P2**：+ classification / vision（与 ADR-007、05 §1 对齐；**key 见 6.4**） | P0 |
| `summary` | 调度默认值、报告开关、提示词（频道级在 channel_settings） | P1 |
| `taxonomy` | closed taxonomy 分类清单 | P2 |
| `rag` | top_k、阈值、索引保留策略 | P1 |
| `qa` | 配额、人格 | P2 |

- 频道级配置在 `channel_settings` 表（结构见 02-storage）；**运行水位不在配置表**（见 02 `summary_cursors`）。
- settings 表结构：`(scope PK, data JSON, updated_at)`；每 scope 一个 Go struct + validator，非法值拒绝写入。

### 6.3 加载顺序与热更新

启动：`.env` → MySQL → settings 全量加载为快照 → 注入各 service。运行时：WebUI/TG 命令修改 → 写 MySQL → 快照原子替换 → settings 中心逐个调用该 scope 订阅者的 `OnConfigChanged`（进程内直接调用，无轮询）。**所有配置写入只经 config 中心一处**（Invariant 4 的配置实例）。

### 6.4 Secrets 边界

| 值 | 位置 | WebUI 可见性 |
|---|---|---|
| Bot token / API hash / MySQL / Qdrant 密码 / WebUI 密码 | 仅 `.env`（文件权限 600） | 永不回显 |
| AI provider API key | settings `ai` scope 的 secret 字段 | 仅可写入，回显为 `•••`+尾 4 位 |
| 其余业务配置 | settings / channel_settings | 正常回显 |
