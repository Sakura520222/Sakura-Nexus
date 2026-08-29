# Sakura-Bot 源项目调研报告

> 调研对象：`E:\项目\Sakura-Bot`（git 仓库，remote: github.com/Sakura520222/Sakura-Bot，当前分支 `feat/channel-post-interaction`，版本 `core/__init__.py` 中为 **1.8.9**，工作区有未提交的 channel_post_chat 新功能文件）。许可证 AGPL-3.0。
> 调研时间：2026-08-29（Explore 子代理产出）

## 1. 技术栈与依赖

`requirements.txt`（全部为最低版本约束，无锁定）：

| 依赖 | 版本 | 用途 |
|---|---|---|
| telethon | >=1.34.0 | MTProto 客户端：主 Bot **和** UserBot（真实账号）都用它 |
| python-telegram-bot | >=20.0 | **QA Bot 专用**（Bot API HTTP 长轮询） |
| openai | >=1.0.0 | LLM（DeepSeek 等 OpenAI 兼容 API） |
| chromadb | >=0.4.0 | 向量库（嵌入式，落盘 `data/vectors/`） |
| aiomysql | >=0.2.0 | 异步 MySQL（连接池） |
| fastapi / uvicorn[standard] / PyJWT | >=0.104 / >=0.24 / >=2.8 | WebUI API（进程内 uvicorn） |
| apscheduler | >=3.10.0 | 定时任务 |
| pydantic / pydantic-settings | >=2.0 | `.env` 配置模型 |
| python-dotenv / aiofiles / watchdog / aiohttp / packaging | — | .env 加载 / 异步文件 / 配置热重载文件监控 / HTTP |

开发依赖：pytest、pytest-cov、pytest-asyncio、ruff、isort、pylint、mypy、safety、pip-audit、pre-commit。

前端 `web/package.json`：Vue 3.5 + vue-router 4.4 + naive-ui 2.40 + axios，Vite 6 + TypeScript 5.6，构建产物提交在 `web/dist/`。

**Python 版本**：Dockerfile 用 `python:3.13-slim`，README 徽章 3.13+；`pyproject.toml` ruff target `py311`；CLAUDE.md 说 3.11+（使用 `X | Y` 语法）。实际最低约 3.11，推荐 3.13。

## 2. 入口与进程模型

### 启动链

- `main.py`（唯一入口）：setup 日志 → `validate_required_settings()` → 同步初始化 `ConfigManager(data/config.json)`（启用 watchdog 热重载）→ 新建事件循环 → 注册 SIGTERM/SIGINT 信号（注释明确：Windows 上 SIGTERM 可能不可用）→ 设置全局 shutdown_event → **`start_qa_bot()` 以 subprocess 拉起 `qa_bot.py`** → 运行 `run_with_shutdown()`：创建 AsyncIOEventBus、订阅配置事件、启动 `FileWatcher("data")`，然后 `AppBootstrap.run()`。
- `core/bootstrap/app_bootstrap.py`：14 步初始化（校验配置 → 错误处理 → **MySQL 初始化** → **主 Bot Telethon 客户端** → UserBot(可选) → APScheduler → 注册全部命令 → 注册命令菜单 → 跨 Bot 通信 → 评论区欢迎 → 转发 → 实时 RAG → 频道帖子聊天 → **WebUI API（uvicorn 后台 task，同一事件循环）** → 启动通知）→ `client.run_until_disconnected()`。
- 关闭：`core/system/shutdown_manager.py` 按序关闭（各组件超时由 `SHUTDOWN_TIMEOUT_*` 环境变量控制）；WebUI 触发的重启通过 `data/.restart_flag`（值为 `webui_restart` 等）+ `os.execv` 原地替换进程。

### 部署方式

- `start.bat`（Windows）：创建 venv → pip install → npm build 前端 → `python main.py`。
- `start.sh`（Linux 一键部署）：git reset 更新代码 → venv → pip install → **npm 构建前端** → 检查 `data/.env` → **用 PM2 托管** `pm2 start main.py --name sakura-bot`。
- `Dockerfile`：多阶段（node:20 构建前端 → python:3.13-slim），ENTRYPOINT `docker-entrypoint.sh`（校验 `TELEGRAM_API_ID/HASH/BOT_TOKEN`，创建 data 下默认文件），CMD `python main.py`。
- `docker-compose.yml`：挂载 `./data`，env_file `data/.env`，端口 8080，**内存限制 1024M / 预留 256M**，restart unless-stopped。

### 进程模型结论

**2 个 OS 进程**：主进程（主 Bot Telethon + UserBot + APScheduler + FastAPI/uvicorn + ChromaDB 全部共存在一个 asyncio 循环里）+ QA Bot 子进程（python-telegram-bot 独立循环）。两进程通过 **MySQL 队列表**通信。

### 后台/定时任务（APScheduler）

| 任务 | 周期 | 位置 |
|---|---|---|
| 每频道定时总结 `main_job` | 每频道 cron（daily/weekly 配置） | 主进程 `core/system/scheduler.py` |
| `cleanup_old_poll_regenerations` | 每天 03:00 | 主进程 |
| 检查 `request_queue`（QA→主 总结请求） | 每 30 秒 | 主进程 |
| 检查 `submissions` 待审核投稿 | 每 30 秒 | 主进程 |
| QA Bot 健康检查 + 自动重启 | 每 60 秒 | 主进程（重启时 Telegram 通知管理员） |
| 消费 `notification_queue` 推送订阅者 | 每 30 秒 | **QA 子进程**（job_queue） |
| 实时 RAG 批量入库 worker | 有界队列 1000，批量 5 条/5 秒 | 主进程 asyncio task |
| 自动趣味投票队列 worker | 队列模式 | 主进程 |

`qa_bot.py`：独立的**用户侧问答 Bot**（`QA_BOT_TOKEN`），PTB `run_polling`；由 `core/system/process_manager.py` 在主进程启动时以 `subprocess.Popen` 拉起，主进程每 60 秒健康检查可自动重启它；WebUI `/api/system/qa-bot/*` 和主 Bot `/qa_start` 等命令可控制它。

## 3. 两个 Bot 分别是什么（合并为 1 个的关键输入）

### Bot A：主 Bot（`TELEGRAM_BOT_TOKEN`，Telethon MTProto，session `data/sessions/bot_session.session`）

全部职责（管理员向）：
- 全部管理命令（`core/commands/`，中英文别名）：频道增删查、每频道总结时间、提示词/投票提示词/AI 配置、频道投票配置、评论区欢迎配置、帖子聊天配置、转发规则全套（16 个 cmd_forwarding_*）、UserBot 管理（join/leave/list/status）、QA Bot 控制、历史/统计/导出、暂停/恢复/重启/更新/关机、语言切换、日志级别、清缓存/清数据库。
- 定时 AI 总结：抓消息（优先 UserBot）→ LLM 总结 → 发报告（回源频道/管理员）→ 存 MySQL + 向量库 → 讨论组投票。
- 投票：总结后生成趣味投票、投票重生成按钮、按票数重生成请求。
- 评论区欢迎：频道新帖后在讨论组发欢迎语 + "申请总结"按钮。
- 频道帖子聊天：讨论组内回复频道帖 + `@qa` 前缀触发 RAG 问答（当前分支新功能）。
- 频道消息转发：监听（优先 UserBot）→ 过滤 → **用 Bot 发送**。
- 实时 RAG：频道新消息向量入库（监听优先 UserBot）。
- 投稿审核：轮询 `submissions`，发审核消息给管理员，approve/reject 回调后发到目标频道。
- 接收 QA Bot 的总结请求（轮询 `request_queue`）。
- 启动通知、数据库迁移建议、QA Bot 崩溃通知。
- **WebUI 鉴权根**：管理 Token = `sha256(TELEGRAM_BOT_TOKEN)[:16]`；JWT 密钥 = `sha256("webui:"+TELEGRAM_BOT_TOKEN)`。**QA Bot 与 WebUI 无关**。

### Bot B：QA Bot（`QA_BOT_TOKEN` + `QA_BOT_USERNAME`，python-telegram-bot）

全部职责（用户向，`qa_bot.py` + `core/handlers/submission_handler.py`）：
- `/start /help /status /clear /ask /view_persona`：RAG 问答（Agentic RAG + 固定管线双模式）、多轮会话、配额限制（`QA_BOT_USER_LIMIT`=3/人/天，`QA_BOT_USER_DAILY_LIMIT`=200/天全局）。
- `/listchannels /subscribe /unsubscribe /mysubscriptions`：频道总结订阅。
- `/request_summary`：写 `request_queue`，请求主 Bot 管理员生成总结。
- `/submit /cancel_submit`：投稿 ConversationHandler（标题/正文/媒体/匿名/落款），AI 润色后写 `submissions` 表供主 Bot 审核。
- 推送：每 30 秒消费 `notification_queue`，用 QA Bot 身份把新总结推送给订阅用户。

跨 Bot 通信 = MySQL 表 `request_queue` / `notification_queue` / `submissions`，两侧各 30 秒轮询——**合并为 1 进程后可直接删除**。

## 4. core/ 架构与消息事件流

### 子模块职责

- `core/config.py`（约 1700 行遗留全局配置）+ `core/config/`：`manager.py`（config.json 原子写 + 回滚 `config.json.last_valid`）、`file_watcher.py`（watchdog 监控 data/，500ms 防抖）、`event_bus.py`（AsyncIOEventBus 优先级订阅）、`events.py`、`validator.py`、`telegram_notifier.py`（配置错误→Bot API 通知首个管理员）。
- `core/settings.py`：pydantic-settings 读取 `data/.env`（`override=True`）。
- `core/bootstrap/app_bootstrap.py`：14 步启动编排器。
- `core/ai/`：`ai_client.py`（LLM 调用 + 热重载重建客户端）、`qa_engine_v3.py`（Agentic RAG + 固定管线回退）、`agent_tools.py`（Function Calling 检索工具）、`vector_store.py`（ChromaDB，collections：summaries/messages）、`embedding_generator.py`（BGE-M3@SiliconFlow）、`reranker.py`（BGE-reranker-v2-m3）、`intent_parser.py`、`memory_manager.py`（频道画像）、`conversation_manager.py`（多轮会话，存 `conversation_history`）、`quota_manager.py`（`usage_quota` 配额）。
- `core/commands/`：主 Bot 命令按功能分 12 个文件；`core/bot_commands.py` 注册菜单；`core/initializers/command_registrar.py` 统一注册。
- `core/forwarding/`：`forwarding_handler.py`（去重 → 关键词/正则/黑名单过滤 → copy/forward + 自定义底栏）、`filters.py`、`media_utils.py`（转发策略）、`download_manager.py`（媒体下载）。
- `core/handlers/`：`userbot_client.py`（UserBot 生命周期/登录/重连）、`realtime_rag_handler.py`、`auto_poll_handler.py`、`channel_comment_welcome.py`、`channel_post_chat_handler.py`、`submission_handler.py`（QA 侧）、`submission_review_handler.py`（主 Bot 侧轮询审核）、`mainbot_menu_handler.py`、`mainbot_push_handler.py`、`mainbot_request_handler.py`。
- `core/infrastructure/`：`database/`（`base.py` 抽象、`manager.py` 单例、`mysql.py` 全部建表/CRUD、`submission_repo.py`）、`logging/`（组件级日志，SafeRotatingFileHandler 10MB×5）、`config/`（prompt/poll/channel/system 各配置管理器）、`utils/`、`exceptions.py`。
- `core/initializers/`：每个启动步骤一个类。
- `core/system/`：`scheduler.py`、`error_handler.py`（指数退避 + record_error）、`process_manager.py`（QA 子进程）、`shutdown_manager.py`。
- `core/telegram/`：`client_management.py`（active client 单例）、`messaging.py`（`fetch_last_week_messages`、`send_report`、`send_long_message`）、`client_utils.py`（分段/Markdown 修复/实体校验）、`poll_handlers.py`、`keyboards.py`。
- 其它：`history_handlers.py`、`qa_user_system.py`（users/subscriptions）、`summary_time_manager.py`（`.last_summary_time.json`）、`services/`、`migrations/`、`i18n/`（zh-CN/en-US 双字典 `t()`）。

### 三类主流程

1. **定时总结**：APScheduler cron → `main_job(channel)` → 增量抓取（**优先 UserBot** `iter_messages`）→ LLM 总结 → 主 Bot 发报告 → MySQL + 向量库 → 投票（可选）→ 更新水位 → 订阅通知进 `notification_queue`。
2. **转发**：`NewMessage(e.is_channel)` 挂在 UserBot（否则 Bot）→ 校验源频道 → 媒体组 1 秒聚合 → 内容哈希去重 → 过滤 → 策略决策 → **主 Bot 发送**（失败回退 UserBot，v1.8.7/1.8.9）→ 统计。
3. **实时 RAG**：`NewMessage/MessageEdited/MessageDeleted`（UserBot 优先）→ 白名单 → 有界队列 1000 背压丢弃 → 批量 embedding → ChromaDB messages collection。

## 5. Telegram 客户端使用

| 客户端 | 库 | 用途 | session |
|---|---|---|---|
| 主 Bot | Telethon（MTProto） | 收管理员命令 + **一切发送** | `data/sessions/bot_session.session` |
| UserBot（真实账号） | Telethon | 历史抓取 `iter_messages`、实时监听、转发回退发送 | `data/sessions/user_session.session` |
| QA Bot | python-telegram-bot（Bot API） | 用户侧命令 + 订阅推送 | 无 session |
| 临时抓取回退 | Telethon 临时客户端 | UserBot 不可用时复用 user_session 临时连接 | 同 user_session |

即：**抓取走 UserBot/真实账号；发送走主 Bot**——方向与重写目标一致，但现状存在「UserBot 未启用时降级为 Bot 监听/抓取」路径。

## 6. web/（WebUI）

- 形态：**Vue 3 SPA**（naive-ui，深/浅主题，axios + localStorage token + 路由守卫 + 401 拦截），构建产物 `web/dist/` 由 FastAPI 挂载，SPA 回退 index.html。
- 后端：FastAPI 工厂；进程内 uvicorn 后台 task（共享事件循环），默认 `0.0.0.0:8080`。CORS 全开；JWT AuthMiddleware。
- 鉴权现状：① Token 登录（token = `sha256(TELEGRAM_BOT_TOKEN)[:16]`）；② Telegram Login Widget（HMAC + `REPORT_ADMIN_IDS`）；③ dev 免认证。**没有用户名/密码体系**——重写需新增。
- 页面：Login、Dashboard、Channels、AIConfig、Schedules、Forwarding、Interaction、Commands、Stats、System（含 QA Bot 控制）、UserBot、VectorStore、Database。
- API：14 个 router（auth、dashboard、channels、ai、schedules、forwarding、system、stats、interaction、summaries、userbot、vector-store、tables、health）。

## 7. 配置系统（三层，重写需收敛）

1. **`data/.env`**：pydantic-settings + core/config.py 均在 import 时 `load_dotenv(override=True)`。
2. **`data/config.json`**：运行时可变配置，WebUI/命令修改，watchdog 热重载，原子写 + 回滚。
3. **data/ 杂项文件**：prompt.txt、poll_prompt.txt、qa_persona.txt、.last_summary_time.json、discussion_cache.json、.restart_flag。

### 全部 .env 配置项（默认值）→ 重写归属

| 分类 | 变量（默认） | 重写归属 |
|---|---|---|
| Telegram 主 Bot | `TELEGRAM_API_ID`、`TELEGRAM_API_HASH`、`TELEGRAM_BOT_TOKEN`（必填） | **保留 .env** |
| QA Bot | `QA_BOT_TOKEN`、`QA_BOT_USERNAME`、`QA_BOT_ENABLED` | **合并后删除** |
| 真实账号 | `USERBOT_ENABLED=False`、`USERBOT_PHONE_NUMBER`、`USERBOT_SESSION_PATH`、`USERBOT_FALLBACK_TO_BOT=False` | **保留 .env（改造）** |
| AI | `LLM_API_KEY`、`DEEPSEEK_API_KEY`、`LLM_BASE_URL`、`LLM_MODEL`、`EMBEDDING_*`、`RERANKER_*`、`VECTOR_DB_PATH` | **迁 MySQL 配置表** |
| 管理/语言 | `REPORT_ADMIN_IDS=""`、`LANGUAGE=zh-CN`、`TARGET_CHANNEL` | **迁 MySQL** |
| 日志 | `LOG_LEVEL` 等一整套 | MySQL（级别可 .env 覆盖） |
| 投票 | `ENABLE_POLL` 等四项 | **迁 MySQL** |
| 数据库 | `MYSQL_HOST/PORT/USER/PASSWORD/DATABASE/CHARSET/POOL_*` | **保留 .env**（连接引导必需） |
| 配额 | `QA_BOT_USER_LIMIT=3`、`QA_BOT_DAILY_LIMIT=200`、`QA_BOT_PERSONA` | **迁 MySQL** |
| WebUI | `WEBUI_ENABLED=False`、`WEBUI_HOST/PORT`、`WEBUI_DEV_MODE` | **.env（新增用户名/密码）** |
| 关闭超时 | `SHUTDOWN_TIMEOUT_*` | 保留可调 |

### config.json 结构（运行时可变配置全集）

`channels[]`、`send_report_to_source`、`enable_poll`、`poll_regen_threshold`、`enable_vote_regen_request`、`log_level`、`summary_schedules{url:{frequency,days[],hour,minute}}`、`channel_poll_settings{url:{enabled,send_to_channel,public_voters}}`、`enable_auto_poll`、`channel_auto_poll_settings`、`comment_welcome.default{enabled,welcome_message,button_text,button_action}`、`channel_comment_welcome{url:{enabled}}`、`forwarding{enabled,show_default_footer,rules[{source_channel,target_channel,keywords[],blacklist[],patterns[],blacklist_patterns[],copy_mode,forward_original_only,custom_footer}]}`、`channel_post_chat`（新分支）。

## 8. 数据存储

### MySQL（aiomysql，`core/infrastructure/database/mysql.py`，db_version=6，utf8mb4/InnoDB）

15 张表：`summaries`、`db_version`、`usage_quota`、`channel_profiles`、`conversation_history`、`users`、`subscriptions`、`request_queue`、`notification_queue`、`forwarded_messages`、`forwarding_stats`、`poll_regenerations`、`poll_voters`、`submissions`、`system_audit_logs`。

### 文件存储（与「全 MySQL」目标冲突点）

- `data/sessions/*.session`：Telethon SQLite session（Bot + UserBot）。
- `data/vectors/`：**ChromaDB 嵌入式库**（chroma.sqlite3 + HNSW bin），collections：summaries + messages。
- data/ 杂项见第 7 节。
- `.sakura/`：非运行时数据（AI 生成的项目概述、每 PR 反思 md）。
- `logs/`：组件级轮转日志。

## 9. 内嵌 TG-Forwarder/ 子目录（结论：可忽略）

- 无 `.gitmodules`、目录内无 `.git`、`git ls-files` 为 0、被 `.gitignore` 显式忽略。
- 内容只是残留拷贝（旧 SQLite、__pycache__、备份）。主项目代码**零引用**。
- Sakura-Bot 的转发功能（`core/forwarding/`）是独立实现。**重写时整体忽略，不迁移。**

## 10. tests/ 覆盖

约 55 个测试文件（pytest + pytest-asyncio auto，marker：unit/integration/slow/telegram/database；`--cov=core`）。覆盖配置总线/监控、AI 全家桶、转发过滤/媒体组、投稿、QA 用户系统、Web API、MySQL 修复等。无端到端真实 Telegram 测试。

## 11. 文档中的设计说明与坑

- `.sakura/SAKURA.md` 架构债（对重写很有价值）：env 与 JSON **配置双通道**布尔解析不一致；JSON 文件直写**无锁**；全局单例仓储 + 无依赖注入导致**测试困难**；延迟导入滥用。
- Windows 绑定：start.bat、SIGTERM 容忍、`DETACHED_PROCESS`、getppid 平台分支、web commands 平台分支——主逻辑跨平台，Linux 长期运行无硬性障碍。
- 内存/稳定性：docker 1G/256M 限制；RAG 有界队列背压；uvicorn access_log 关闭；telethon/apscheduler/httpx 日志降噪；QA Bot 60s 自动重启；watchdog 500ms 防抖；`.restart_flag` + `os.execv` 重启。
- 当前分支未提交功能：channel_post_chat（讨论组回复帖子 + @qa RAG），重写需决定是否纳入。

## 12. 面向最终用户的功能全景（重写保留清单）

**管理侧（主 Bot / WebUI）**
1. 多频道 AI 总结：daily/weekly/多天调度、增量抓取、自定义提示词、报告回源频道/管理员、长消息分段。
2. 历史管理：/history、/stats、/export、WebUI 统计。
3. 投票互动：AI 生成双语趣味投票、重生成按钮、票数阈值自动重生成、公开/匿名、自动趣味投票。
4. 评论区欢迎：频道新帖 → 讨论组欢迎语 + "申请总结"按钮。
5. 频道帖子聊天：讨论组回复帖子 + 前缀触发 RAG（新分支）。
6. 频道消息转发：多规则、关键词/正则/黑名单、copy/forward、自定义底栏、去重、统计、热重载。
7. WebUI 全套管理。
8. UserBot：真实账号抓取、自动加入源频道、join/leave/list/status。

**用户侧（QA Bot，合并后为同一 Bot 的用户命令）**
9. RAG 问答：Agentic + 管线回退、多轮对话、限定频道、流式编辑回复、人格。
10. 配额：每人每日 + 全局每日、管理员豁免。
11. 订阅推送：/subscribe 系列，新总结自动推送。
12. 总结请求：/request_summary。
13. 投稿系统：多步投稿、AI 润色、管理员审核、发布到频道。

**基础设施语义（保留）**：配置热重载、优雅关闭、错误重试/降级、i18n、组件化日志、「抓取走真实账号、发送走 Bot」分工、MySQL 唯一持久层（需去掉 ChromaDB 与 session 文件）。
