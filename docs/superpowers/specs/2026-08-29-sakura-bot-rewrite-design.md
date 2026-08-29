# Sakura-Bot 重写整合 — 设计文档

- 日期：2026-08-29
- 状态：已定稿（自主模式下由代理拟定，用户可随时修订；修订后按新设计调整实施）
- 输入：[docs/research/sakura-bot-report.md](../../research/sakura-bot-report.md)、[docs/research/tg-forwarder-report.md](../../research/tg-forwarder-report.md)

## 1. 背景与目标

将 `E:\项目\Sakura-Bot`（v1.8.9，双 Bot 双进程）重写为本仓库的全新实现，并整合 `E:\项目\TG-Forwarder` 的转发能力。目标：**Linux 服务器上稳定、长期、低占用地运行**。

## 2. 硬性约束（用户指定）

1. 单 Bot：原主 Bot + QA Bot 收敛为 **1 个 Bot**。
2. 所有数据、配置写入 **MySQL**。
3. `.env` 仅含：Telegram Bot Token、WebUI 用户名/密码、真实 Telegram 账号相关配置（外加 MySQL 连接参数——这是访问数据库的引导配置，无法入库，属于隐含必需）。
4. 所有消息抓取/获取由**真实账号**执行；所有发送由 **Bot 账号**执行。
5. Linux 长期运行、低占用。

## 3. 关键决策与备选方案对比

### 3.1 进程模型 → **A. 单进程单 asyncio 事件循环**

| 方案 | 优点 | 缺点 |
|---|---|---|
| **A. 单进程**（Telethon×2 + APScheduler + FastAPI/uvicorn 共存一个循环）✅ | 无进程间通信；删除队列轮询与子进程管理；占用最低；代码简单 | 单组件崩溃影响全局（以组件级异常隔离 + systemd/docker 自动重启兜底） |
| B. 双进程（保留 QA 独立） | 隔离好 | 违背「单 Bot」约束；队列轮询复杂度回来了 |
| C. 抓取/发送分进程 | 理论隔离 | 过度设计；两进程都要 Telegram 连接，占用翻倍 |

### 3.2 Bot 库 → **A. 纯 Telethon（删除 python-telegram-bot）**

QA Bot 的全部用户功能（私聊命令、按钮、会话状态机）Telethon 均可实现（Bot 账号的 MTProto 客户端收发私聊消息与回调）。单库双客户端，依赖更少，且 Bot 发送也走 MTProto（比 Bot API HTTP 更快、无轮询）。

### 3.3 向量存储 → **A. MySQL BLOB + 进程内 numpy 检索（删除 ChromaDB）**

| 方案 | 评估 |
|---|---|
| **A. `embeddings` 表（float16 BLOB）+ numpy top-k + 内存缓存** ✅ | 满足「全 MySQL」；去掉 ChromaDB 重依赖（其本身内存占用数百 MB 级、依赖树庞大）；规模受保留期控制（默认 90 天清理）后全量载入内存矩阵乘完全够用（10 万条 × 1024 维 float16 ≈ 200MB 上限，实际远低） |
| B. 保留 ChromaDB 文件库 | 违背「所有数据写 MySQL」；重依赖与内存开销 |
| C. MySQL 9 原生 VECTOR 类型 | 绑定 MySQL 9+（用户现有库未知），生态尚新 |

### 3.4 Telethon session → **A. 自定义 MySQLSession（StringSession 序列化 + 防抖落库）**

继承 `MemorySession`：启动时从 `sessions` 表反序列化；`save()` 触发防抖（≥30s 间隔或关键事件）序列化为 `StringSession` 写回 MySQL，退出时强制落库。代价：极端情况下重启丢失最近 updates 状态 → Telethon 自动 get difference 补拉，可接受。彻底消灭 session 文件。（B 备选：保留文件 session——违背约束，弃。）

### 3.5 配置中心 → **MySQL `settings` 表（scope, key, value JSON）+ 进程内事件总线**

单进程后无需 watchdog/轮询：WebUI 或 Bot 命令改配置 → 写 MySQL → 发 `ConfigChanged` 进程内事件 → 订阅模块即时生效（AI 客户端重建、转发规则缓存刷新、调度器重排）。内存保留一份已解析配置快照（pydantic 模型）。

### 3.6 Web → **FastAPI + uvicorn（进程内后台 task）+ Vue 3 SPA（naive-ui）**

沿用源项目形态与页面经验，鉴权改为 `.env` 用户名/密码换取 JWT（HS256，密钥派生自密码+盐），替换原「token=sha256(bot_token)」与 Telegram Login Widget（Widget 依赖可保留为可选，但默认用户名密码）。

## 4. 系统架构

```
┌─────────────────────────── 单进程 asyncio ───────────────────────────┐
│  main.py → Bootstrap（显式依赖注入容器，无全局单例）                    │
│                                                                      │
│  ┌──────────┐  抓取/监听   ┌──────────────┐   发送    ┌──────────┐   │
│  │ UserBot   │──────────▶│ 功能模块       │─────────▶│ Bot      │   │
│  │ (Telethon │            │ · 转发引擎     │          │ (Telethon│   │
│  │  真实账号) │            │ · 定时总结     │          │  唯一Bot) │   │
│  └──────────┘            │ · 实时RAG入库  │          └──────────┘   │
│         ▲                │ · 投票/欢迎    │                ▲         │
│         │                │ · 投稿/订阅/QA │                │         │
│  ┌──────┴──────┐         └──────┬───────┘        ┌──────┴───────┐  │
│  │ MySQLSession │                │                │  命令路由     │  │
│  └─────────────┘         ┌──────┴───────┐        │ (管理员/用户) │  │
│                          │ APScheduler   │        └──────────────┘  │
│  ┌─────────────┐         └──────────────┘                           │
│  │ aiomysql 池  │◀─── 全部 repositories（无 ORM，手写 SQL）           │
│  └─────────────┘                                                    │
│  ┌────────────────────────────────────┐                             │
│  │ FastAPI/uvicorn（WebUI API + SPA） │── 配置中心读写 + 审计        │
│  └────────────────────────────────────┘                             │
└──────────────────────────────────────────────────────────────────────┘
            MySQL 8（唯一持久层：配置/业务数据/向量/session/审计）
```

数据流铁律（约束 4 的实现）：
- **只有 UserBot** 调用 `iter_messages` / `get_messages` / 事件监听（NewMessage/MessageEdited/MessageDeleted）。
- **只有 Bot** 调用 `send_message` / `send_file` / `edit_message` / 投票发送 / 回复。
- 删除源项目的「UserBot 不可用时降级为 Bot 抓取」路径：需要抓取的功能（转发、总结、RAG 入库）在 UserBot 未登录时标记 disabled 并告警，不降级。

## 5. `.env` 最终定义（其余一切配置进 MySQL）

```ini
# ---- MySQL（引导配置）----
MYSQL_HOST=localhost
MYSQL_PORT=3306
MYSQL_USER=sakura
MYSQL_PASSWORD=change-me
MYSQL_DATABASE=sakura_bot

# ---- Telegram 唯一 Bot ----
TELEGRAM_BOT_TOKEN=            # 必填
TELEGRAM_API_ID=               # MTProto 必填（Bot 客户端也用）
TELEGRAM_API_HASH=             # 必填

# ---- 真实账号（UserBot）----
USERBOT_PHONE_NUMBER=          # 可选；登录流程用
USERBOT_SESSION_STRING=        # 可选：预置 StringSession；缺省则通过 WebUI 登录向导生成

# ---- WebUI ----
WEBUI_ENABLED=true
WEBUI_HOST=0.0.0.0
WEBUI_PORT=8080
WEBUI_USERNAME=admin
WEBUI_PASSWORD=change-me

# ---- 可选：日志与关闭行为 ----
LOG_LEVEL=INFO
SHUTDOWN_TIMEOUT_TOTAL=30
```

AI 的 api_key/base_url/model、管理员 ID 列表、语言、投票/欢迎/QA 配额等**全部迁入 MySQL 配置中心**，由 WebUI 维护。首次启动 MySQL 为空时使用代码内置默认值，WebUI 首页引导补全。

## 6. MySQL Schema（v1，utf8mb4/InnoDB，`db_version` 管理）

| 表 | 关键列 | 用途 |
|---|---|---|
| `settings` | PK(scope,key)，value JSON | 配置中心（ai/system/poll/welcome/post_chat/qa/forwarding_global/logging…） |
| `admins` | user_id PK | Telegram 管理员（空 = 无人可用 Bot 管理命令，仅 WebUI 可管理） |
| `channels` | id PK，channel_id UNIQUE，username，title，type | 频道注册表 |
| `channel_settings` | channel_id PK，summary/poll/welcome/post_chat JSON，last_summary_message_id，last_summary_at | 每频道配置与总结水位 |
| `forward_rules` | id PK，source_chat_id+source_username，target_chat_id+target_username，enabled，keywords/blacklist/patterns/blacklist_patterns/media_types JSON，forward_original_only，copy_mode，ai_enabled，ai_prompt，custom_footer，delay_min/delay_max，last_message_id | 转发规则（整合两项目规则模型） |
| `forwarded_messages` | PK(source_chat_id,source_message_id,target_chat_id)，rule_id，target_message_id，content_hash，forwarded_at | 去重 + 目标消息映射（未来编辑/删除同步钩子） |
| `forwarding_stats` | PK(rule_id,date)，count | 按天统计 |
| `summaries` | id PK，channel_id，summary_text，message_count，period_start/end，ai_model，message_ids JSON，report_message_id，poll_message_id | 总结记录 |
| `subscriptions` | PK(user_id,channel_id,sub_type) | 用户订阅 |
| `users` | user_id PK，first_name，username，language，last_seen | Bot 用户 |
| `usage_quota` | PK(user_id,quota_date) | QA 配额 |
| `conversation_history` | id PK，user_id，session_id，role，content | 多轮对话 |
| `embeddings` | id PK，kind(summary/message)，channel_id，ref_id，vector(float16 BLOB)，dim，text_hash，created_at；INDEX(kind,channel_id)、INDEX(created_at) | 向量库（替代 ChromaDB） |
| `channel_profiles` | channel_id PK，profile JSON | 频道画像 |
| `submissions` | id PK，user_id，title，content，media JSON，anonymous，signature，ai_polished_content，status，reviewer_id，review_message_id，published_message_id | 投稿 |
| `poll_regenerations` / `poll_voters` | 同源项目 | 投票重生成 |
| `sessions` | name PK('userbot'/'bot')，session_data MEDIUMTEXT，updated_at | Telethon 会话 |
| `runtime_state` | key PK，value JSON | 杂项运行状态（讨论组缓存等） |
| `system_audit_logs` | id PK，user_id，action，detail JSON | WebUI 审计 |
| `db_version` | version | schema 版本 |

原 `request_queue`/`notification_queue`/`submissions 轮询` 机制删除（单进程直接调用）。

## 7. 模块设计（目录即架构）

```
sakura_bot/
  main.py            入口：日志 → .env 校验 → Bootstrap.run()
  settings.py        pydantic-settings（仅 .env 项，见 §5）
  bootstrap.py       启动编排 + 依赖注入容器（AppContext dataclass 传递）
  shutdown.py        优雅关闭（组件注册逆序）
  db/                pool.py（aiomysql 池）schema.py（建表/迁移）repositories/*.py
  config_center.py   MySQL 配置中心：加载 → pydantic 快照 → 事件总线广播
  events.py          进程内 AsyncIO 事件总线
  clients/           userbot.py、bot.py、mysql_session.py、client_manager.py
  forwarding/        engine.py（事件入口+相册聚合）、filters.py、sender.py（三态发送+限流+重试）、footer.py、forwarding.py（规则仓库+缓存）
  summarizer/        jobs.py（APScheduler 任务）、fetcher.py（UserBot 抓取）、report.py（Bot 发送+分段）、polls.py
  rag/               embedder.py、store.py（MySQL 向量+numpy 检索）、rerank.py、qa_engine.py、conversation.py
  features/          welcome.py、post_chat.py、submissions.py、subscriptions.py、push.py
  commands/          router.py（管理员/用户分流）、admin/*.py、user/*.py
  webapi/            app.py、auth.py（用户名密码→JWT）、routes/*.py、static 挂载 web/dist
  ai/                client.py（OpenAI 兼容，热重建）
  utils/             logging.py（组件级+轮转+环形缓冲）、i18n.py、retry.py、text.py（分段/entities 修复）、tempfile 流式下载
web/                 Vue 3 + Vite + naive-ui SPA（重新实现页面集）
tests/               unit + integration（见 §10）
deploy/              sakura-bot.service（systemd）
Dockerfile / docker-compose.yml（bot + 可选 mysql profile）
.env.example
```

### 7.1 转发引擎（两项目能力合并清单）

- 规则模型 = TG-Forwarder 的扁平多对多规则 ∪ Sakura-Bot 的过滤字段：`source→target`、keywords（子串）、blacklist、patterns（正则）、blacklist_patterns、media_types、forward_original_only、copy_mode、ai_enabled+ai_prompt、custom_footer、delay_min/max。
- 事件入口只挂 **UserBot**；源匹配：数字 chat_id 优先，username 次之（规则两列都存）。
- 相册聚合：grouped_id 动态窗口（首条后 2s 或集齐 10 条上限）+ 兜底 flush 定时器，修复源项目固定窗口丢消息竞态。
- 过滤链顺序：频道校验 → 相册聚合 → 去重查 →（原创限定 → 关键词 → 正则 → 黑名单 → 黑名单正则 → 媒体类型）→ AI 改写（失败降级原文）→ 底栏模板。
- 发送（仅 Bot）三态：
  1. 纯文本：`send_message(text=原文, entities=原 entities)`——保留原格式，优于 TG-Forwarder 的 Markdown 重解析；>4096 分段。
  2. 单媒体/相册：UserBot 流式 `download_media` 到临时文件（非 BytesIO，低内存）→ Bot `send_file`（保留 attributes/spoiler 等）→ 删除临时文件。
  3. copy_mode=forward 且 Bot 在源频道：`forward_messages` 原样转发（默认关闭，文档标注限制）。
- 限流与稳健：全局发送队列串行化；每规则随机延迟 [min,max]；`FloodWaitError` 按 wait 时间服从；失败指数退避重试 3 次；发送成功才写 `forwarded_messages`（修复 TG-Forwarder 假成功计数）；转发结果按真实成败计数入 `forwarding_stats`。
- 自动 join：新增规则时 UserBot 自动 JoinChannel(源)；目标频道校验 Bot 管理员身份并给出明确错误提示。
- catch-up：依赖 Telethon get difference 补发离线更新；`forward_rules.last_message_id` 记录水位，WebUI 提供「手动回溯补发 N 条」操作。
- 清理：每日定时清理超过保留期（默认 30 天，可配）的 forwarded_messages。

### 7.2 总结与互动（语义保留，实现移植）

- APScheduler 按频道 cron（daily/weekly/自定义 days+hour）→ UserBot 增量 `iter_messages`（排除上次报告消息 ID）→ LLM 总结 → Bot 发报告（分段）→ 写 summaries + embeddings → 可选投票（讨论组/频道、公开/匿名、票数阈值重生成）→ 更新水位 → 进程内直接推送给订阅者（Bot 发送）。
- 评论区欢迎、频道帖子聊天（@qa RAG）、自动趣味投票：按源语义移植。

### 7.3 QA（并入唯一 Bot）

- 用户命令：/start /help /ask /clear /status /view_persona /listchannels /subscribe /unsubscribe /mysubscriptions /request_summary /submit /cancel_submit。
- 管理命令（精简集，WebUI 为主）：/channels /summarize /pause /resume /status /userbot (join/leave/status) /stats /history /export /language /loglevel。
- 会话状态机：内存 dict + 过期清理（投稿多步流程），不再依赖 PTB ConversationHandler。
- Agentic RAG（Function Calling 检索工具）+ 固定管线回退、配额（默认 3/人/天 + 200/天，管理员豁免）。

### 7.4 WebUI 页面集（Vue 3 重写）

Login（用户名密码）、Dashboard（统计/连接状态/UserBot 状态/登录向导）、Channels、Schedules、AIConfig、Forwarding（规则 CRUD + 统计 + 回溯补发）、Interaction、QA（配额/人格/订阅总览）、Submissions（审核队列）、Commands、System（暂停/恢复/重启/日志级别/日志流 WebSocket/审计）、Database。鉴权：`POST /api/auth/login` → JWT；审计写 system_audit_logs。

## 8. 错误处理与可观测性

- 分层异常：`SakuraError` 基类（ConfigError/DBError/TelegramError/AIError…）；组件初始化失败策略可配（strict=退出 / lenient=跳过该功能并告警）。
- 重试：DB 短暂断连自动重连池；AI 指数退避；发送 FloodWait 服从；全部重试有日志与上限。
- 日志：组件级 logger（forwarder/summarizer/rag/userbot/bot/webapi），RotatingFile 10MB×5 + stdout；Telethon 噪音降噪；环形缓冲（deque 500）供 WebUI WebSocket 实时流。
- 健康：`/api/health`（DB 连通、两客户端 authorized 状态、调度器运行状态）→ Docker healthcheck 与 systemd 探活。
- 启动/关闭/严重错误通过 Bot 通知管理员（若已配置）。

## 9. 部署与低占用措施

- Dockerfile：多阶段（node:20 build 前端 → python:3.13-slim），非 root，`web/dist` 预构建随镜像（生产可不装 node）；健康检查；`PYTHONOPTIMIZE`。
- docker-compose：`bot` + `mysql`（profile=local，默认假设外部 MySQL）；mem_limit 建议 512m（去 ChromaDB 后 1G 上限绰绰有余）。
- systemd：`deploy/sakura-bot.service`（Restart=always, RestartSec=5, EnvironmentFile=.env）。
- 低占用手段：无 ORM、无 ChromaDB、无 PTB、媒体流式临时文件（非内存）、向量保留期清理、RAG 有界队列背压、日志轮转、uvicorn access_log 关闭、单事件循环复用连接。

## 10. 测试策略

- 单元（不依赖网络/Telegram）：过滤链（关键词/正则/黑名单/媒体类型/原创）、去重键、底栏模板、文本分段、相册聚合窗口逻辑、配置中心快照与事件、MySQLSession 序列化往返、限流/重试策略（用假时钟）、投稿状态机。
- 集成（真实 MySQL，docker 或本地）：schema 创建与迁移幂等、repositories CRUD、embeddings 存取与 numpy 检索正确性。
- Telegram 层：薄封装 + mock 客户端接口（UserBot/Bot 各一个 Protocol），核心业务全部依赖 Protocol 注入，测试注入 fake。

## 11. 范围分级（控制实施节奏）

- **P0（核心闭环，本阶段必须）**：骨架、.env、DB 池+schema+配置中心、MySQLSession、双客户端、转发引擎全量、WebUI 核心页（登录/仪表盘/转发/频道/系统/日志流）、命令精简集、Docker/systemd、单测、README。
- **P1（第二优先）**：定时总结+报告+水位、订阅与推送、投稿全流程、评论区欢迎、投票。
- **P2（第三优先）**：Agentic RAG 问答、实时向量入库、频道帖子聊天、en-US i18n、统计页全量。

## 12. 明确不做（YAGNI）

- 消息编辑/删除同步（schema 已留 `target_message_id` 钩子，未来可加）。
- 多 UserBot 账号、多租户。
- Bot 抓取降级路径（违反约束 4，彻底删除）。
- 原主 Bot 的 16 个 forwarding_* 管理命令全集（WebUI 承担，仅保留 /status /pause /resume 等精简集）。
- PTB、ChromaDB、watchdog 文件热重载、config.json、跨进程队列：全部删除。
