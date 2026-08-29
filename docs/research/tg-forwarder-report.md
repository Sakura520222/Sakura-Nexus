# TG-Forwarder 项目调研报告

> 调研对象：`E:\项目\TG-Forwarder`（git 仓库，最新提交 `735c965 添加WebUI`）。`备份/` 目录为旧版快照（无 WebUI、web 模块为 api/models/services/websocket 多文件结构，后被整合为单文件 `src/web/app.py`），除 WebUI 外核心转发逻辑与当前版一致。以下引用均为相对该仓库根目录的路径。
> 调研时间：2026-08-29（Explore 子代理产出）

## 1. 技术栈与依赖

`requirements.txt`：

| 库 | 声明版本 | venv 实装版本 | 用途 |
|---|---|---|---|
| telethon | >=1.36.0 | 1.43.2 | Telegram MTProto 客户端（核心） |
| PyYAML | >=6.0.1 | 6.0.3 | 配置文件解析 |
| openai | >=1.0.0 | 2.38.0 | AI 消息改写（OpenAI 兼容 API） |
| aiohttp | >=3.9.0 | 3.13.5 | WebUI HTTP/WebSocket 服务器 |
| python-dotenv | >=1.0.0 | 1.2.2 | **声明了但代码中从未使用**（无任何 `load_dotenv` 调用） |

- Python 版本：本机 venv 为 **3.13.7**；`Dockerfile` 基于 `python:3.13-slim`；README 宣称 3.8+（git 历史有针对 Python 3.12+ `asyncio.wait()` 协程问题的修复提交）。
- 数据库：Python 标准库 `sqlite3`（同步、每次操作新建连接），无 ORM、无 alembic，自带手写迁移逻辑。
- 无其他框架（WebUI 为手写 aiohttp + 原生 JS，无前端框架）。

## 2. 转发功能全景（核心）

### 2.1 规则模型
- 规则定义于 `config/channels.yaml` 的 `forwarding_rules` 列表，单条规则字段：`source_channel` / `target_channel` / `keywords[]` / `enabled` / `description` / `ai_prompt` / `custom_footer`（`src/config_manager.py:222` `add_forwarding_rule`）。
- **天然多对多**：规则是扁平列表，同一源可配多条规则指向不同目标（不同关键词路由），一条消息命中多条规则即转发到多个目标（`src/client.py:290-317` 对 `get_rules_by_source` 返回的规则循环处理）。
- 规则匹配按源频道标识：先用 `@username` 匹配，失败再用数字 ID 匹配（`src/client.py:265-281`）；匹配前用 `_normalize_channel_id` 去 `@`、转小写（`src/config_manager.py:192`）。
- `keywords` 为空列表 = 不过滤、全量转发（`src/filters.py:58-60`）。实际运行的 `config/channels.yaml` 中两条规则均为空关键词 + AI 润色，即"全转 + AI 改写"用法。

### 2.2 消息过滤（`src/filters.py`）
- **频道类型过滤**：只处理 `is_channel` 且 `broadcast=True` 的广播频道，跳过私聊/群组（`src/client.py:241-247`）。
- **只转原创**：`is_original_message` —— 消息带 `forward_from` / `forward_from_chat` 即判定为二手转发，直接丢弃（`src/filters.py:22-43`；在 `should_forward` 中默认 `check_forwarded=True`）。**注意：这也意味着"源频道里别人转发来的内容"永远不会被转发。**
- **关键词过滤**：`contains_keywords` —— 不区分大小写的子串匹配，命中任一即通过（`src/filters.py:45-90`）。
- **正则**：`use_regex` 参数已实现但**调用处从未传 True，属死代码**（`src/client.py:303` 调用 `should_forward(message, keywords)` 未带该参数）。
- **没有**：媒体类型过滤、黑白名单、用户过滤、长度过滤。

### 2.3 去重 / 防重复
- 有效机制：SQLite `messages` 表，主键 `(message_id, target_channel)`；转发前 `is_message_forwarded` 查重，转发后 `add_message`（`INSERT OR IGNORE`）记录（`src/database.py:44-54,153-219`；`src/client.py:298,862`）。即"源消息 ID + 目标频道"粒度防重。
- **内容哈希去重是死代码**：`content_hash` 列、`is_duplicate_content`、`filters.extract_content_for_hash` 均已定义但从未被调用。

### 2.4 延迟 / 重试 / 防风控
- 转发前固定 `await asyncio.sleep(delay)`（config `forwarding.delay`，当前 0.5s）——**无随机抖动、无分批、无令牌桶**（`src/client.py:764`）。
- 失败重试：`max_retries=3`、间隔 `retry_delay=5s`（`src/client.py:767-882`）；AI 调用独立重试 3 次 × 2s（`src/ai_handler.py:77-117`）。
- 防风控实际手段仅两样：固定延迟 + 优先用 Bot 发送（Bot API 限制更高）。FloodWait 依赖 Telethon 内置处理。
- 已知缺陷：`_forward_message` 内部吞掉异常，`forwarded_count` 在外层无条件 +1，失败时日志仍打印"消息成功转发到 N 个目标频道"（日志中可见该误导，`logs/bot.log`）。

### 2.5 相册（media group）处理（`src/client.py:481-556`）
- 用 `grouped_id` 聴合：首条消息到达后 `sleep(1.0)` 收集同组消息，然后以首条消息做规则匹配/关键词过滤/查重（键为首条消息 ID）。
- 发送：`send_client.send_file(target, files列表, caption)` **重建相册**。
- 缺陷：固定 1 秒窗口，超时到达的后续消息因缓存已 `pop` 而丢失；无兜底 flush。Bot 模式下整个相册所有媒体逐个下载进内存 `BytesIO` 再上传，大视频有内存压力。

### 2.6 消息转换
- **AI 改写**（`src/ai_handler.py`）：规则级 `ai_prompt`，拼接为 `"{prompt}\n\n{text}"` 调 OpenAI 兼容 API；失败自动降级为原文继续转发；全局 `ai.enabled` 开关 + 每规则 prompt 双重控制。
- **来源标注 / 自定义底栏**（`src/client.py:900-981` `_generate_source_info`）：
  - 规则级 `custom_footer` 模板，支持占位符 `{source_link} {source_title} {target_title} {source_channel} {target_channel} {message_id}`（用 `str.replace` 安全替换）；
  - 无自定义底栏时用默认 `[Source](链接) @目标频道`；全局 `forwarding.show_default_footer` 可关；
  - 源链接：公开频道 `https://t.me/{username}/{msg_id}`，私有频道 `https://t.me/c/{abs(chat_id)}/{msg_id}`。
- 文本发送用 `parse_mode='md'`、`link_preview=False`；**不保留原消息 entities**（取 `message.message` 纯文本按 Markdown 重解析）。
- 无水印、无通用模板引擎（模板仅限底栏）。

### 2.7 媒体：下载重传 vs 原样转发
- **全项目从不调用 `messages.forward_messages`**，一律"复制式"重发：
  - User 发送且未 AI 处理：`send_file(file=message.media)` 直接传媒体引用（跨聊天复制，不下载，`src/client.py:843-849`）；
  - **Bot 发送或经 AI 处理**：User 客户端 `download_media` 到内存 `BytesIO` → 推断扩展名（`file.ext` → mime_type → 按类型兜底 .jpg/.mp4/.mp4/.mp3/.bin）→ 设置 `buffer.name` → 发送端 `send_file` 重上传（`src/client.py:806-841`）。
- `MessageMediaWebPage` 不算媒体，走纯文本分支。
- AI 处理过的文本消息即使无图也按 caption 媒体流程或纯文本流程发送。

### 2.8 明确不具备的功能
- **无编辑同步**（无 `events.MessageEdited`）、**无删除同步**（无 `events.MessageDeleted`，全仓 grep 确认）。
- 无历史消息回填（catch-up）逻辑、无定时/延迟队列、无媒体类型路由、无多账号源监听。

### 2.9 辅助行为
- 启动时对全部启用源频道执行 `JoinChannelRequest` 自动加群（`src/client.py:1088-1104`）；添加规则时同样自动加入（`src/commands.py:55-61`、`src/web/app.py:234-239`）。
- 启动/关闭向管理员（Bot）或 Saved Messages（User）发通知。
- 定时清理：`_cleanup_task` 每 `cleanup_interval`（3600s）删除超过 `max_message_age`（86400s）的转发记录（`src/client.py:1149-1163`）。
- 按源频道统计 `stats` 表（总转发数、最后转发时间）。

## 3. 客户端架构

- **库**：Telethon（MTProto），两个 `TelegramClient` 实例：
  - **User 真实账号**：`data/user` 会话，`client.start(phone_number)` 交互式登录。**负责监听全部源频道**：`events.NewMessage(outgoing=False)`，回调内 `get_chat()` 后按 broadcast 频道过滤（`src/client.py:221-322`）。
  - **Bot 账号**：`data/bot_client` 会话，`start(bot_token=...)`。负责：接收私聊 `/命令`（`src/client.py:328`）；可选负责向目标频道发送（`use_bot_to_send`，当前配置为 true）。
- **监听方式**：纯事件回调，无主动历史拉取。断线重连依赖 Telethon 内建机制（日志中可见 `Got difference for channel ... updates`，即 Telethon 恢复时会拉取 update difference，错过不久的消息会以事件形式补发，但项目无显式 catch-up/水位逻辑）。
- **发送路径**：见 2.7 —— 永远是"复制重发"而非 `forward_messages`（无"Forwarded from"头，配合原创过滤不会造成循环）。
- **运行模型**：单进程单事件循环；`asyncio.wait([user.run_until_disconnected(), bot.run_until_disconnected(), webui.start()], FIRST_COMPLETED)`（`src/client.py:1036-1044`）；SIGINT/SIGTERM → `stop()` 发通知后断连。**所有 sqlite3 调用为同步阻塞，直接跑在事件循环线程里**。

## 4. 配置系统与数据

### config/
- `config/config.yaml`（敏感，已 gitignore 但历史上可能入库）：`telegram`（api_id/api_hash/phone_number/bot_token/use_bot_to_send）、`bot`（target_channel 遗留字段、max_message_age、admin_user_ids）、`database`（path、cleanup_interval）、`logging`（level/file/max_bytes/backup_count）、`forwarding`（delay/max_retries/retry_delay/show_default_footer）、`ai`（enabled/api_key/api_base/model/temperature/max_tokens/timeout/max_retries/retry_delay）、`session`（name/workdir）、`webui`（enabled/host/port/password）。
- `config/channels.yaml`：全部转发规则（见 2.1），Bot 命令与 WebUI 的写操作都会直接 `yaml.dump` 回写此文件（`src/config_manager.py:84-101`、`src/web/app.py:374-378`）。
- `config/config.example.yaml`：带注释的模板。
- **全部配置存文件，无任何数据库化配置**。

### data/
- `data/bot.db`（SQLite）：表 `messages(message_id, source_channel, target_channel, timestamp, content_hash, created_at, PK(message_id,target_channel))` + 索引；表 `stats(channel_id PK, total_forwarded, last_forwarded, updated_at)`。含自写的旧表结构迁移（`src/database.py:84-151`）。当前 24 条 messages / 4 条 stats。
- `data/user.session`、`data/bot_client.session`（+ `-journal`）：Telethon SQLite 会话文件（含登录凭据）。

## 5. run.py 入口与 src/ 模块结构与事件流

- `run.py`：把 `src/` 加入 `sys.path`，`from client import main`；异常打印堆栈后 `input()` 等回车退出（容器内依赖 tty）。
- `src/` 模块职责：
  - `client.py`（约 1190 行，**实际核心，全部转发逻辑在此**）：`TelegramForwardBot` 初始化顺序 = ConfigManager → logging → Database → MessageFilter → CommandHandler → MessageHandler（占位）→ AIHandler → WebUI → User/Bot 客户端 → 注册事件 → 信号处理；`start()` 顺序 = User 登录 → Bot 登录 → 缓存源频道实体（`get_entity`）→ 加群+启动通知 → 启动清理协程 → 并发跑三任务。
  - `config_manager.py`：YAML 读写、规则 CRUD、关键词增删、启停切换、admin 判定（**admin 列表为空 = 任何人可用命令**）。
  - `database.py`：SQLite 记录/查重/统计/清理。
  - `filters.py`：原创判定 + 关键词（含未启用的正则与内容哈希提取）。
  - `commands.py`：Bot 管理命令实现（add/remove_rule、关键词、toggle、footer、prompt、stats、reload、cleanup、AI 测试等 14 个命令）。
  - `ai_handler.py`：AsyncOpenAI 封装（process_message / test_connection / 热更新配置）。
  - `handlers.py`：**纯占位**（构造函数存引用，无逻辑，历史遗留）。
  - `web/app.py` + `web/static/`（index.html 381 行、app.js 862 行、style.css）：aiohttp WebUI —— 登录（口令→内存 token 24h）、仪表盘（统计/连接状态）、规则 CRUD/toggle、配置段编辑（api_hash/bot_token/api_key 脱敏回显）、实时日志（环形缓冲 500 条 + WebSocket 推送 + 级别/搜索过滤）、配置 reload。
- **事件全链路**：频道新消息 → User 回调（`client.py:222`）→ channel/broadcast 校验 → album 聚合分支 或 直接流程 → 规则匹配（username→ID）→ 逐规则：`is_message_forwarded` 查重 → `should_forward`（原创+关键词）→ `_forward_message`：sleep(delay) → AI 改写（可选）→ 生成底栏 → 分支发送（媒体下载重传 / 媒体引用复制 / 纯文本）→ `add_message` 落库。Bot 命令与 WebUI API 旁路修改规则/配置（写 YAML）。

## 6. session / 登录

- 会话文件位置由 `session.workdir`（`data/`）+ 固定名 `user` / `bot_client` 决定（`src/client.py:102,115`）；`session.name` 配置项实际未用于拼接（硬编码 `user`）。
- User 登录：`client.start(phone_number)` —— Telethon 交互式在 **stdout 输入验证码**（Docker 下需 `docker attach`，README 有说明）；代码中无 2FA 密码参数（Telethon 会交互提示）。首次登录成功后凭据持久化在 `.session` 文件，之后启动免交互。
- Bot 登录：`start(bot_token=...)`，无需交互。
- 会话文件已入 `.gitignore`/`.dockerignore`；但 `config/config.yaml`（含 api_hash、bot_token、手机号、AI key 明文）虽在 gitignore 中，`config/channels.yaml`（同样被 ignore）仍处于 git 跟踪状态——凭据泄露风险需在新系统中注意。

## 7. 部署

- `Dockerfile`：`python:3.13-slim` + gcc，装依赖，拷贝 `src/ config/ run.py`，VOLUME config/data/logs，`EXPOSE 8080`，`CMD ["python","run.py"]`。
- `docker-compose.yml`：`restart: unless-stopped`；挂载 `./config ./data ./logs`；`TZ=Asia/Shanghai`；`stdin_open+tty`（为 run.py 的 `input()` 和首次登录输码服务）；端口 `8080:8080`。
- **无 healthcheck、无内存/CPU 限制**。
- **配置漂移缺陷**：当前 `config/config.yaml` 中 `webui.port: 8089`，而 compose 只映射 8080 → 容器内 WebUI 宿主机不可达。

## 8. 日志与监控

- 根 logger：`RotatingFileHandler(logs/bot.log, 10MB×5, utf-8)` + stdout；级别来自配置；专门压制 `telethon.network.mtprotosender`(WARNING) 与 `telethon.network.connection`(ERROR) 的连接噪音日志（`src/client.py:179-181`）。
- WebUI：`WebUILogHandler` 内存 deque(500) + WebSocket 实时推送 + `/api/logs` 级别/关键字/条数过滤（`src/web/app.py:17-56,395-429`）。
- 错误处理模式：每层 try/except 全捕获记日志；转发/AI 各有重试；**无重试队列、无死信、无指标（metrics）、无告警**，仅 TG 启停通知。

## 9. 已知问题（代码中无任何 TODO/FIXME 注释）

1. 死代码：`handlers.py` 整个占位模块；`use_regex`、内容哈希去重（`database.is_duplicate_content`、`filters.extract_content_hash`、`content_hash` 列）从未启用；`python-dotenv` 装而未用。
2. 正确性：转发失败后仍计数"成功转发 N 个"；album 固定 1s 聚合窗口有丢消息竞态；AI 生成文本超 4096 字符无分段（发送会失败）；"只转原创"导致源频道内的二手转发全部丢失（可能是无意限制）；admin_user_ids 为空时命令对所有人开放。
3. 部署：WebUI 端口配置与 compose 映射不一致；healthcheck 缺失。
4. 安全：明文凭据写入 config.yaml 且部分被 git 跟踪；WebUI token 仅存内存（重启即失效）、密码明文比较。
5. 架构：同步 sqlite3 阻塞事件循环；Bot 模式下所有媒体先入内存再上传。

## 10. 对新系统的取舍建议

**值得保留的设计**：
- 规则模型：扁平 `source→target + keywords + ai_prompt + custom_footer` 列表，天然支持多对多与按关键词分路由——可直接映射为 MySQL 规则表。
- "复制式转发"（重发而非 `forward_messages`）：无转发头、不暴露源、避免自触发循环；与"Bot 发送"目标完全一致。
- 三态发送策略：User 引用直传（免下载）/ 下载重传（跨账号）/ 纯文本——Bot 发送分支的"下载→BytesIO→扩展名推断→重上传"是跨账号媒体复制的完整参考实现（建议改内存为临时文件流）。
- 原创过滤、`(source_msg_id, target)` 去重键、定期清理过期记录、规则级 AI 改写 + 失败降级原文、底栏占位符模板、添加规则自动 JoinChannel、启动/关闭 TG 通知、日志环形缓冲 + WebSocket 实时推送（WebUI 体验好）。

**与目标系统冲突、需要替换的实现**：
- **配置全在 YAML 文件** → 改为 MySQL 存储；`ConfigManager` 的规则 CRUD 语义可平移成 DAO。
- **SQLite 同步阻塞调用** → 换 MySQL（异步驱动），顺带解决事件循环阻塞。
- **User 账号参与发送** → 新系统统一 Bot 发送，只保留下载重传路径。
- **Bot 私聊命令管理**（commands.py 全部）→ 新系统有 WebUI，可整体丢弃，保留 admin 概念即可。
- `run.py` 的 `input()`/交互式登录 → 改造为无人值守启动（session 预生成、错误直接退出交由 systemd/docker 重启）。
- 固定 delay 无抖动 → 加随机化延迟与 FloodWaitError 捕获。
- 缺失而新系统可能需要的：编辑/删除同步、历史 catch-up 水位、媒体类型过滤、超长消息分段——需新写。
