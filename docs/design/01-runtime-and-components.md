# 01 运行时与组件

- 状态：📝 已成文，待用户审
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
   c. UserClient（gotd，LENIENT：失败 → 降级告警，WebUI 登录向导可用；
      依赖 User 的功能（转发/抓取）进入 degraded 状态，不 fatal）
   d. Outbound（Sender 实现：内部路由 MTProto / Bot API Rich，见 §4）
   e. Qdrant client（P1 起；P0 不构造，注入 nil 占位接口）
5. 领域 service 构造（构造函数注入各自接口）并按序注册
6. 每个注册 service 起 supervisor goroutine → Run(ctx)
7. WebServer（标准库 ServeMux + embed SPA）启动
8. 就绪通知（管理员 TG 消息，可配关闭）
```

### 1.2 组件注册：`service` 抽象

```go
// internal/app/lifecycle.go
type service interface {
    Name() string
    Run(ctx context.Context) error    // 阻塞至 ctx 取消；返回 error = 该服务不可恢复
    Shutdown(ctx context.Context) error
}
```

App 按启动顺序持有 `[]service`；关闭按注册**逆序**调用 `Shutdown`（各自超时受 root shutdown 预算约束）。

### 1.3 错误两层与 fatal 判定

| 类别 | 服务 | Run 返回 error 的后果 |
|---|---|---|
| CORE | MySQL 连接、BotClient、WebServer | 全局 cancel → 优雅退出（exit 1）→ systemd Restart=on-failure |
| DEGRADED | UserClient、ForwardingService、SummaryService、RAGService | 记 CRITICAL + 管理员通知；服务停止但进程存活；WebUI 显示 degraded 原因与恢复入口（如 UserBot 登录向导）；服务自身实现「等待依赖恢复后重新 Run」（supervisor 自动重启该 service，指数退避） |

- 可恢复错误（断线、FloodWait、429/5xx、DB 闪断）**永远在 service/platform 内部 retry**，不冒泡为 Run 返回。
- panic：仅 supervisor 的 goroutine boundary recover → 记 stack → 等同该服务 Run 返回 fatal（CORE → 全局退出；DEGRADED → 退避重启）。禁止业务代码裸 recover 继续跑。

### 1.4 优雅退出序列与退出码

```text
SIGTERM/SIGINT 或 CORE fatal → cancel root ctx
→ 各 service Run 返回（停止接收新任务）
→ 逆序 Shutdown（drain 在途任务；总预算 SHUTDOWN_TIMEOUT_SECONDS，默认 30s）
→ UserClient / BotClient close（含 gotd session 落库）
→ WebServer shutdown → MySQL 池关闭
→ exit：0 正常 / 1 CORE fatal / 2 配置缺失
```

### 1.5 health endpoint

`GET /api/health`（无需鉴权）：`{mysql, bot_client, user_client, forwarding, scheduler, web}` 各项 `ok | degraded | down` + 版本号 + uptime。Docker HEALTHCHECK 与 systemd watchdog 探活用；细项鉴权后在 `/api/system/status`。

## 2. 代码组织与依赖方向

### 2.1 包结构

```text
sakura-bot/
├── cmd/sakura-bot/main.go      # composition root：.env → App 构建 → Run。无业务逻辑
├── internal/
│   ├── app/          # App 组合、lifecycle（service 注册/逆序关闭）、health 聚合
│   ├── config/       # .env 加载（struct）+ settings 中心（MySQL，scope→struct 校验+快照+变更通知）
│   ├── logging/      # slog setup、环形缓冲 handler、组件 logger 工厂
│   ├── platform/     # 基础设施具体实现（全项目唯一 import gotd/qdrant/sqlx 的层）
│   │   ├── mysql/    # sqlx 池、goose runner、gotd session storage、repositories 实现
│   │   ├── telegram/ # gotd UserClient / BotClient、updates 分发、Outbound 路由实现
│   │   ├── botapi/   # ADR-008：sendRichMessage net/http 客户端（限流/429/重试）
│   │   └── qdrant/   # Qdrant client 适配（P1；含 collection/alias/reindex 底层调用）
│   ├── forwarding/   # 转发领域：engine、filters、album 聚合、规则模型、repo 接口（消费者定义）
│   ├── summary/      # P1：调度、增量抓取、水位、报告组装
│   ├── rag/          # P1 起：ingest、retrieval、context builder；P2：rerank/memory/agent
│   ├── conversation/ # P2：讨论会话、记录/触发分离
│   ├── webapi/       # HTTP 路由、DTO、auth、WebSocket、RAG Query Harness
│   └── domain/       # 跨领域纯数据类型（MessageRef、ChatRef、AIResponse…，无行为依赖）
├── web/              # Vue 3 SPA（pnpm build → dist/ → go:embed）
├── migrations/       # goose SQL（//go:embed）
├── Makefile
└── Dockerfile / compose.yaml / deploy/
```

### 2.2 依赖方向规则（CI 强制）

```text
cmd → app → {forwarding, summary, rag, conversation, webapi, config, logging}
领域包（forwarding/summary/rag/conversation/webapi）→ 只 import domain + 自身接口定义
platform/* ← 仅 app 引用（构造后注入领域）
领域包之间：只经对方暴露的接口（如 summary 需要 Forwarding 的抓取？否——共用 Fetcher）
```

- 用 golangci-lint **depguard** 在 CI 强制：`internal/forwarding|summary|rag|conversation|webapi` 禁止 import `github.com/gotd/td`、`github.com/qdrant/go-client`、`github.com/jmoiron/sqlx`、`net/http`（webapi 除外，它本身是 HTTP 层）。
- **禁止** `App` 暴露 `DB / Qdrant / UserTG / BotTG` 字段给领域（构造期使用后即不外传）；禁止 `internal/utils`、`internal/common`、`internal/manager` 类大杂烩包——工具函数就近放领域内或 `domain`。

### 2.3 接口由消费者定义（Go 惯例）

每个领域包在自己文件里定义所需接口（如 forwarding 定义 `Fetcher / Sender / RuleRepo / ForwardedRepo`），platform 实现这些接口；**不存在** `internal/interfaces` 巨型接口包。接口保持最小面（只列该消费者用到的方法）。

## 3. 接口边界清单

### 3.1 P0 需要的接口

```go
// forwarding 包（消费者定义；实现：platform/telegram + platform/mysql）
type Fetcher interface {
    OnNewMessage(chats []int64, h func(ctx context.Context, m domain.ChannelMessage)) (cancel func())
    GetHistory(ctx context.Context, chatID int64, minID int64, limit int) ([]domain.ChannelMessage, error)
    DownloadMedia(ctx context.Context, m domain.ChannelMessage, dest string) error
    JoinChannel(ctx context.Context, chatID int64) error
}
type Sender interface {
    Send(ctx context.Context, req SendRequest) (domain.SentMessage, error)
}
type MessageRenderer interface { // Rich Markdown 规范化/校验（ADR-008）
    Render(ctx context.Context, c domain.MessageContent) (domain.RenderedMessage, error)
}
type RuleRepo interface {
    ListEnabled(ctx context.Context) ([]Rule, error)
    Create / Update / Delete / SetEnabled(...)  // WebUI 与命令共用
}
type ForwardedRepo interface {
    Exists(ctx, src domain.MessageRef, target int64) (bool, error)
    Record(ctx, rec ForwardedRecord) error
    CleanupBefore(ctx, retention time.Duration) (int64, error)
}

// summary（P1）需要：Fetcher.GetHistory + Sender + SummaryRepo
// rag（P1）需要：Embedder / Retriever / RetrieverRepo
```

### 3.2 P1/P2 预留边界（只签名，不做假实现）

```go
// rag（P1 定义）
type Retriever interface {
    Retrieve(ctx context.Context, q RetrievalQuery) ([]Candidate, error) // P1: dense+filter 实现
}
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}
// P2（仅签名，P1 提供 no-op：返回 ErrNotImplemented 或空结果，保证编译与调用安全）
type Reranker interface { Rerank(ctx context.Context, q string, c []Candidate, topK int) ([]Candidate, error) }
type QueryAnalyzer interface { Analyze(ctx context.Context, q string) (QueryPlan, error) }
type VisionProcessor interface { Describe(ctx context.Context, media domain.MediaRef) (domain.VisionDescription, error) }
type MemoryStore interface { /* P2 定义 */ }
```

规则：**只留边界，不写仿真实现**；no-op 实现放 rag 包内一处（`noop.go`），注入与否由 app 按 P0/P1/P2 构造决定。

## 4. 出站消息抽象

### 4.1 统一模型（不为 Rich Message 长出平行业务 API）

```go
// forwarding 包定义；domain 承载数据
type SendRequest struct {
    ChatID   int64
    Style    SendStyle          // Auto（默认）/ Plain / Rich
    Content  *domain.MessageContent // 结构化内容（AI 输出：text + 可选 media 引用 + metadata）
    Text     string             // Plain 直发文本
    Entities []domain.Entity    // 可选：原 entities 透传（转发复制语义）
    Media    []domain.MediaRef  // 本地文件或媒体引用
    Caption  string
    ReplyTo  int64              // 回复目标消息
    Markup   *domain.Keyboard   // inline keyboard
    Silent   bool
}
```

### 4.2 实现内部路由（platform/telegram/Outbound）

```text
SendRequest
   ├─ Content != nil → MessageRenderer.Render（normalizer + validate + block 切分）
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
- supervisor 为每个 service 起一个 goroutine；service 内部再起的 worker goroutine 由该 service 统一 `errgroup`（派生 ctx）管理，Run 返回即全部回收。

### 5.2 channel 规则

- 每个 channel 必须有：**唯一 owner goroutine（接收侧）**、**显式容量**、**明确关闭责任（发送方唯一时发送方 close；多发送方时禁止 close，用 ctx 取消 + drain）**。
- 本项目 channel 短清单（禁止网状自由 channel）：
  - 转发发送队列（容量 100，**阻塞背压**：转发不允许丢，满则等待/告警）
  - 日志环形缓冲→WS 推送（容量 512，**丢弃最旧**：日志可丢）
  - RAG ingest 队列（P1，容量 1000，**丢弃+计数**：可丢，reindex 可补）
  - 跨组件事件（配置变更等）：**不走全局 channel 总线**，用 settings 中心回调注册（订阅者明确、可枚举）。

### 5.3 Telegram update 分发

- gotd `updates.Dispatcher` 在 platform/telegram 内注册；按 chat/事件类型路由到已注册的领域 handler（forwarding 注册 NewMessage；P1 rag 注册 New/Edit/Delete；P2 conversation 注册讨论群消息）。
- 领域 handler 收到的是 `domain.ChannelMessage`（已剥离 gotd 类型），返回即结束；**handler 内 panic 由 dispatcher 边界 recover 记日志，不影响其他 handler 与连接**。

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
WEBUI_HOST=0.0.0.0
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
| `system` | 语言、启动通知、维护开关 | P0 |
| `forwarding` | show_default_footer、dedup_days、content_dedup、默认延迟区间 | P0 |
| `logging` | level（覆盖 .env 的运行时级） | P0 |
| `ai` | provider（base_url/model/temperature/embedding/vision 分类配置，**key 只存此处的 secret 列，见 6.4**） | P1 |
| `summary` | 每频道调度的默认值、报告开关、提示词（频道级在 channel_settings） | P1 |
| `taxonomy` | closed taxonomy 分类清单 | P1 |
| `rag` | top_k、阈值、保留策略 | P1 |
| `qa` | 配额、人格（P2） | P2 |

- 频道级配置在 `channel_settings` 表（结构见 02-storage），不在 settings。
- settings 表结构：`(scope PK, data JSON, updated_at)`；每 scope 一个 Go struct + validator，非法值拒绝写入。

### 6.3 加载顺序与热更新

启动：`.env` → MySQL → settings 全量加载为快照 → 注入各 service。运行时：WebUI/TG 命令修改 → 写 MySQL → 快照原子替换 → settings 中心逐个调用该 scope 订阅者的 `OnConfigChanged`（进程内直接调用，无轮询）。**所有配置写入只经 config 中心一处**（Invariant 4 的配置实例）。

### 6.4 Secrets 边界

| 值 | 位置 | WebUI 可见性 |
|---|---|---|
| Bot token / API hash / MySQL / Qdrant 密码 / WebUI 密码 | 仅 `.env`（文件权限 600） | 永不回显 |
| AI provider API key | settings `ai` scope 的 secret 字段 | 仅可写入，回显为 `•••`+尾 4 位 |
| 其余业务配置 | settings / channel_settings | 正常回显 |
