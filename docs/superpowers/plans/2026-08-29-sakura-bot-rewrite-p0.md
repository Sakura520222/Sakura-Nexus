# Sakura-Bot 重写整合 实施计划（P0 阶段）

> **For agentic workers:** 本计划由当前会话自主执行（inline 模式）。步骤用 checkbox 跟踪。设计依据：[../specs/2026-08-29-sakura-bot-rewrite-design.md](../specs/2026-08-29-sakura-bot-rewrite-design.md)（schema 见 §6，目录见 §7，约束见 §2）。
> P1/P2 任务在本计划末尾列出为后续批次。

**Goal:** 按 P0 范围交付：单进程单 Bot 的 Sakura-Bot 新实现（MySQL 全量配置/数据、UserBot 抓取 + Bot 发送、整合版转发引擎、WebUI 核心、Docker/systemd 部署）。

**Architecture:** 单 asyncio 事件循环：Telethon UserBot（唯一抓取者）+ Telethon Bot（唯一发送者）+ APScheduler + FastAPI/uvicorn（进程内）。aiomysql 连接池 + 手写 SQL repositories，无 ORM。配置中心 = MySQL settings 表 + 进程内事件总线。

**Tech Stack:** Python 3.11+（目标 3.13）、Telethon、aiomysql、FastAPI、uvicorn、APScheduler 3.x、pydantic v2、PyJWT、numpy、openai SDK；前端 Vue 3 + Vite + naive-ui。

**依赖清单（pyproject）：**
运行：telethon>=1.36, aiomysql>=0.2, pydantic>=2.7, pydantic-settings>=2.2, fastapi>=0.110, uvicorn[standard]>=0.29, PyJWT>=2.8, apscheduler>=3.10,<4, openai>=1.30, numpy>=1.26
开发：pytest, pytest-asyncio, ruff, httpx

**测试约定：** 单测不依赖网络/MySQL；集成测试标记 `mysql`（存在环境变量 `SAKURA_TEST_MYSQL_DSN` 时才跑，DSN 格式 `user:pass@host:port/db`）；Telegram 交互全部通过 `clients/protocols.py` 的 Protocol 抽象注入 Fake。

---

### Task 1: 项目骨架与工具链

**Files:** `pyproject.toml`、`sakura_bot/__init__.py`（`__version__ = "2.0.0"`）、`sakura_bot/main.py`（空入口，仅 `--version` 打印）、`tests/__init__.py`、`pytest.ini`（asyncio_mode=auto, markers: mysql）、`ruff.toml`（target py311, line-length 100）

- [ ] 写 pyproject（依赖如上，`[project.scripts] sakura-bot = "sakura_bot.main:main"`）
- [ ] `python -m pytest` 可运行（0 收集、退出码 5 视为通过）
- [ ] `ruff check .` 通过
- [ ] Commit: `chore: 项目骨架与工具链`

### Task 2: settings.py（.env 唯一入口）

**Files:** `sakura_bot/settings.py`、`tests/test_settings.py`
接口：`class Settings(BaseSettings)` 字段=设计 §5 全部变量（MYSQL_*、TELEGRAM_BOT_TOKEN/API_ID/API_HASH、USERBOT_PHONE_NUMBER/USERBOT_SESSION_STRING、WEBUI_ENABLED/HOST/PORT/USERNAME/PASSWORD、LOG_LEVEL、SHUTDOWN_TIMEOUT_TOTAL）；`validate_required()` 校验 BOT_TOKEN/API_ID/API_HASH/WEBUI_PASSWORD 非空，缺失抛 `SettingsError` 并列出全部缺失项。
测试：必填缺失 → 异常信息含缺失变量名；完整 → 对象可构造。

- [ ] 失败测试 → 实现 → 通过 → Commit: `feat: .env 配置模型与必填校验`

### Task 3: db/pool.py + db/schema.py

**Files:** `sakura_bot/db/pool.py`、`sakura_bot/db/schema.py`、`tests/integration/test_schema.py`
接口：
```python
class Database:
    def __init__(self, settings: Settings): ...
    async def connect(self) -> None      # 创建池（pool_size=5, max_overflow=10…），自动重试 3 次
    async def close(self) -> None
    @asynccontextmanager
    async def acquire(self) -> aiomysql.Connection   # dict 游标, autocommit=False, with 事务
    async def fetch_all / fetch_one / execute(返回 lastrowid/rowcount)
SCHEMA_VERSION = 1
async def ensure_schema(db: Database) -> int   # 幂等建库表（设计 §6 全部 20 张表）+ 写 db_version
```
SQL 要点：全部 `utf8mb4`/`InnoDB`；`embeddings.vector BLOB`；`settings.value JSON`；`forwarded_messages` PK(source_chat_id, source_message_id, target_chat_id)；`forward_rules` 列见设计 §6。
集成测试（mysql 标记）：建表幂等（跑两次同版本）；插入/查询 settings 往返 JSON 正确。
单测：无 MySQL 时 `ensure_schema` 报错信息可读（用 Fake 池断言 SQL 包含 CREATE TABLE 数量=20）。

- [ ] 单测 → 实现 → mysql 集成测试（本机无 MySQL 则跳过）→ Commit: `feat: aiomysql 连接池与 schema v1`

### Task 4: events.py 进程内事件总线

**Files:** `sakura_bot/events.py`、`tests/test_events.py`
接口：`class EventBus: subscribe(topic, handler) / unsubscribe / async publish(topic, payload)`；固定 topic 常量 `Topics.CONFIG_CHANGED = "config.changed"`（payload: scope）。handler 异常捕获记日志不中断其他订阅者。
测试：多订阅者收发；异常订阅者不影响其他；unsubscribe 后不再收。

- [ ] TDD 三步 → Commit: `feat: 进程内事件总线`

### Task 5: config_center.py 配置中心

**Files:** `sakura_bot/config_center.py`、`tests/test_config_center.py`
接口：
```python
class ConfigCenter:
    def __init__(self, db, bus: EventBus): ...
    async def load(self) -> None                 # settings 表 → 快照（缺失 scope 用 DEFAULTS）
    def get[T](scope: str, model: type[T]) -> T  # pydantic 模型快照（内存）
    async def update(self, scope: str, values: dict) -> None  # 合并写库→重建快照→publish(CONFIG_CHANGED)
    async def on_config_changed(...)             # 订阅回调，供模块刷新
```
scope 模型定义在 `sakura_bot/config_models.py`：`AISettings(llm_api_key,llm_base_url,llm_model,embedding_*,reranker_*)`、`SystemSettings(language,admins 更新走 admins 表, poll 默认, welcome 默认, qa quota, forwarding_global(show_default_footer,dedup_days=30,delay 默认区间), logging(level))`——全部带安全默认值。
测试（Fake db）：load 空→默认；update→快照变更+事件发布；部分字段更新保留其余。

- [ ] TDD → Commit: `feat: MySQL 配置中心与快照`

### Task 6: utils/logging.py

**Files:** `sakura_bot/utils/logging.py`、`tests/test_logging.py`
要点：`setup_logging(level, log_dir="logs")`：stdout + RotatingFile(10MB×5, utf-8)；`RingBufferHandler(deque 500)` 挂根 logger 供 WebUI；降噪 telethon.network.*；`get_component_logger(name)`；`WebLogBuffer.get(entries, level, keyword)` 过滤。
测试：RingBufferHandler 收集与过滤。

- [ ] TDD → Commit: `feat: 组件化日志与环形缓冲`

### Task 7: clients/mysql_session.py（Telethon session 入库）

**Files:** `sakura_bot/clients/mysql_session.py`、`tests/test_mysql_session.py`
核心实现（继承 MemorySession）：
```python
class MySQLSession(MemorySession):
    def __init__(self, name: str, db: Database):
        super().__init__(); self._name, self._db = name, db; self._dirty = False; self._last_flush = 0.0
    @classmethod
    async def load(cls, name, db) -> "MySQLSession":
        s = cls(name, db); row = await db.fetch_one("SELECT session_data FROM sessions WHERE name=%s", name)
        if row and row["session_data"]:
            parsed = StringSession(row["session_data"]); s._dc_id, s._server_address, s._port = parsed._dc_id, parsed._server_address, parsed._port
            s._auth_key = parsed._auth_key; s.takeout = parsed.takeout
        return s
    def save(self):                       # Telethon 在实体/状态更新时调用（同步）
        self._dirty = True
    async def flush(self, force=False):   # 防抖 ≥30s 或 force；序列化 StringSession 写 sessions 表（REPLACE）
        if not self._dirty and not force: return
        now = time.monotonic()
        if not force and now - self._last_flush < 30: return
        ss = StringSession(); ss._dc_id, ss._server_address, ss._port = self._dc_id, self._server_address, self._port
        ss._auth_key = self._auth_key; ss.set_dc = None  # 见实现：直接复制定义于 StringSession 的私有字段
        await self._db.execute("REPLACE INTO sessions(name, session_data, updated_at) VALUES(%s,%s,NOW())", self._name, ss.save())
        self._dirty = False; self._last_flush = now
    async def close(self): await self.flush(force=True)
```
后台任务 `flush_loop(session)` 每 30s 调 flush（在 bootstrap 挂载）。
测试（Fake db）：load 空→空 session；save→flush(force) 写入；写入串可被 StringSession 解析回（auth_key None 时跳过 auth 字段断言）。

- [ ] TDD → Commit: `feat: Telethon MySQLSession`

### Task 8: clients/protocols.py + userbot.py + bot.py + client_manager.py

**Files:** 上述四文件、`tests/test_clients.py`
protocols.py 定义注入边界（业务只依赖它）：
```python
class Fetcher(Protocol):   # 唯一抓取者=UserBot
    async def iter_messages(chat, limit, offset_id, reverse) -> list[FetchedMessage]: ...
    async def get_entity(chat) -> Entity: ...
class Sender(Protocol):    # 唯一发送者=Bot
    async def send_message(chat, text, entities=None, reply_to=None, link_preview=False) -> SentRef: ...
    async def send_file(chat, files: list[LocalFile|MediaRef], caption=None, entities=None) -> SentRef: ...
    async def forward_messages(dest, from_chat, ids) -> list[SentRef]: ...
class Downloader(Protocol):  # UserBot
    async def download_to_tempfile(message) -> Path: ...
```
（以轻量 dataclass `FetchedMessage/SentRef/LocalFile` 封装 Telethon 类型，测试用 Fake 构造。）
userbot.py：`UserBotClient`：load session→start（`USERBOT_SESSION_STRING` 优先，否则 WebUI 登录向导写入的 sessions 行；都没有→`available=False`）；`is_authorized`；事件注册辅助 `on_new_message(handler, chats)`；断线由 Telethon 自愈，暴露 `connected` 属性。
bot.py：`BotClient`：MySQLSession('bot')→start(bot_token)；`me`；发送方法实现 Sender（含 entities 透传、`parse_mode=None`）。
client_manager.py：持有两者，`ensure_started()`，`status()` 返回两客户端连接态（供 /api/health、dashboard）。
测试：Fake TelegramClient 注入（monkeypatch TelegramClient 构造），验证 start 参数分流（bot_token vs 无）、available=False 路径、status 字段。

- [ ] TDD → Commit: `feat: UserBot/Bot 双客户端与抓取发送协议边界`

### Task 9: forwarding/filters.py 过滤链（纯函数，重点 TDD）

**Files:** `sakura_bot/forwarding/filters.py`、`tests/test_filters.py`
接口：
```python
@dataclass
class RuleView:  # 从 DB 行映射的内存视图
    id: int; source_chat_id: int | None; source_username: str | None
    target_chat_id: int; target_username: str | None; enabled: bool
    keywords: list[str]; blacklist: list[str]; patterns: list[str]; blacklist_patterns: list[str]
    media_types: list[str]; forward_original_only: bool; copy_mode: str  # "copy"|"forward"
    ai_enabled: bool; ai_prompt: str | None; custom_footer: str | None
    delay_min: float; delay_max: float
def match_source(rule: RuleView, chat_id: int | None, username: str | None) -> bool   # id 优先，username 归一化(@去/小写)
def check_keywords(text: str, kws: list[str]) -> bool      # 空=过；子串不分大小写，任一命中
def check_patterns(text: str, pats: list[str]) -> bool     # 空=过；re.search 命中任一；坏正则记日志按不匹配
def check_blacklists(text, words, pats) -> bool            # 任一命中→False
def check_media_types(msg: FetchedMessage, types: list[str]) -> bool  # 空=过；text/photo/video/audio/document/animation/sticker/voice/video_note/any
def is_original(msg) -> bool                                # 无 forward_from/forward_from_chat
def should_forward(rule: RuleView, msg: FetchedMessage) -> tuple[bool, str]  # 组合上述，str=拒绝原因（日志/统计用）
```
测试覆盖：每条函数正反例；媒体相册首条带图+纯文本消息；`should_forward` 全链各拒绝原因文案；username 带 @ 与大小写归一。

- [ ] TDD（先写全部用例）→ 实现 → Commit: `feat: 转发过滤链`

### Task 10: forwarding/footer.py 底栏模板

**Files:** `sakura_bot/forwarding/footer.py`、`tests/test_footer.py`
接口：`def build_footer(template: str | None, *, show_default: bool, source_link, source_title, target_title, message_id, bot_username) -> str`
占位符：`{source_link} {source_title} {target_title} {source_channel} {target_channel} {message_id} {assistant_bot}`；无模板且 show_default → `[Source]({source_link})`；源链接规则：公开频道 `t.me/{username}/{id}`，私有 `t.me/c/{abs(id)}/{id}`。
测试：模板替换、默认底栏、关闭开关、私有频道链接。

- [ ] TDD → Commit: `feat: 转发底栏模板`

### Task 11: forwarding/sender.py 发送队列（三态发送/限流/FloodWait/重试）

**Files:** `sakura_bot/forwarding/sender.py`、`tests/test_sender.py`
接口：
```python
class SendJob: chat_id; text=None; entities=None; files: list[Path]=[]; caption=None; forward=None(tuple from_chat,ids)|None; footer_text=None; rule_delay=(min,max)
class ForwardSender:
    def __init__(self, sender: Sender, downloader: Downloader | None): ...
    def submit(self, job: SendJob) -> asyncio.Future[SentRef]   # 入全局 asyncio.Queue
    async def run(self) -> None                                  # 唯一消费者协程（bootstrap 启动）
    async def _send_one(self, job) -> SentRef                    # 见下
```
`_send_one` 逻辑：① 等待随机延迟 `uniform(min,max)`；② job.forward → sender.forward_messages；③ job.files → sender.send_file（多文件自动相册，caption+footer 拼接、entities 透传）；④ 纯文本 → footer 追加（换行拼接）→ `>4096` 分段循环 send；⑤ `FloodWaitError`：`await sleep(e.seconds+1)` 后重试（计入次数）；其他异常指数退避 1/2/4s 重试 3 次，仍失败→标记 Future 异常完成（记 error 日志）。发送串行化在 run() 消费者实现。
测试（Fake Sender 抛 FloodWaitError(3)一次→重试成功；抛普通错误 3 次→Future 异常；分段长度边界 4096/4097；footer 拼接格式；随机延迟用注入 randfunc 断言区间）。

- [ ] TDD → Commit: `feat: Bot 发送队列（限流/FloodWait/重试/分段）`

### Task 12: forwarding/forwarding.py 规则仓库 + 缓存

**Files:** `sakura_bot/forwarding/repository.py`、`tests/test_forwarding_repo.py`
接口：`class ForwardRuleRepo: async list_enabled() -> list[RuleView]（含缓存，CONFIG_CHANGED 失效）/ create/update/delete/set_enabled/stats(rule_id)/record_forward(...)/is_forwarded(src_chat,src_msg,target)/cleanup_before(days)`；CRUD 参数校验（source 至少有 id 或 username 之一；delay_min<=delay_max）。
测试（Fake db）：缓存失效事件；is_forwarded/record 幂等；cleanup SQL 条件。

- [ ] TDD → Commit: `feat: 转发规则仓库与缓存`

### Task 13: forwarding/engine.py 引擎（事件入口+相册聚合+编排）

**Files:** `sakura_bot/forwarding/engine.py`、`tests/test_engine.py`
接口：
```python
class ForwardingEngine:
    def __init__(self, repo, filters_mod, footer_mod, sender, ai_client, fetcher, bus): ...
    async def start(self) -> None    # fetcher.on_new_message(self._on_message, chats=规则源集合)
    async def _on_message(self, msg: FetchedMessage) -> None
```
`_on_message` 编排：① chat 匹配 `repo.list_enabled()` 多规则循环；② `grouped_id` 非空→`AlbumAggregator`（dict[grouped_id]=(msgs, task)；首条创建 `asyncio.create_task(self._flush_album(gid))` 延迟 2.0s，集满 10 条提前触发；`_flush_album` 从 dict pop 后处理）→ 相册以首条做过滤判断；③ `should_forward`；④ `is_forwarded` 去重；⑤ ai_enabled→`ai.rewrite(prompt, text)` 失败降级原文；⑥ 组装 SendJob（媒体：逐条 `downloader.download_to_tempfile` 收集 Path 列表，finally 删除临时文件；caption=改写后文本）；⑦ submit 并 `await future`；成功→`record_forward`（含 target_message_id）与 stats 计数，失败→error 日志（不计成功）。
测试（全 Fake）：单文本消息命中 1 规则→1 次 send_message+record；命中 2 规则→2 目标；相册 3 条（第 3 条延迟到 2s 后到达仍在窗口→1 次 send_file 3 文件；窗口过后到达→按新消息独立处理）；去重跳过；AI 异常降级原文；temporary file 清理（FakeDownloader 生成真临时文件断言删除）。

- [ ] TDD（本任务用例最多，先全写）→ Commit: `feat: 转发引擎（相册聚合/AI改写/去重/编排）`

### Task 14: ai/client.py

**Files:** `sakura_bot/ai/client.py`、`tests/test_ai_client.py`
接口：`class AIClient: __init__(cfg: AISettings)；async chat(messages, **kw) -> str（指数退避 1/2/4s×3）；async rewrite(prompt, text) -> str（"{prompt}\n\n{text}"，重试后仍失败抛 AIError）；def rebuild(cfg)`（CONFIG_CHANGED 订阅触发）。
测试：Fake AsyncOpenAI（monkeypatch 构造）——成功、失败 2 次后成功、3 次失败抛 AIError；rewrite 拼接格式。

- [ ] TDD → Commit: `feat: OpenAI 兼容客户端与热重建`

### Task 15: commands/ 精简命令集

**Files:** `sakura_bot/commands/router.py`、`sakura_bot/commands/admin_cmds.py`、`sakura_bot/commands/user_cmds.py`、`tests/test_commands.py`
要点：`CommandRouter.register(bot_client)`——`events.NewMessage(pattern=r'^/[a-z_]+')` 统一入口→解析命令名→查表；管理命令要求 `admins` 表命中（空表=仅 WebUI）；命令实现为纯 async 函数 `(ctx) -> None`（ctx 含 msg、sender、config、repos、功能开关接口）。命令集：管理 `/status /pause /resume /channels /summarize <url> /userbot join|leave|status /stats /language /loglevel`；用户 `/start /help`（P1 其余用户命令后补）。暂停机制：`FeatureSwitch`（内存 flag，engine/scheduler 启动前检查）。
测试：路由正则；无权限回复拒绝文案；/pause /resume 翻转 FeatureSwitch。

- [ ] TDD → Commit: `feat: Bot 命令路由与精简管理命令`

### Task 16: webapi/auth.py（用户名密码→JWT）

**Files:** `sakura_bot/webapi/auth.py`、`tests/test_auth.py`
要点：`POST /api/auth/login {username,password}`（body）比对 settings（恒定时间比较 `secrets.compare_digest`）→ 签发 JWT（HS256，exp=7d，sub=username, scope=webui），密钥=`sha256(sha256(password)+"webui:"+username)`；`AuthMiddleware`/依赖 `require_auth`；token 刷新 `POST /api/auth/refresh`。失败统一 401 `{"detail":"用户名或密码错误"}`，登录失败计数（内存，5 次/10 分钟锁定，日志审计）。
测试：正确凭据得 JWT 且受保护端点通过；错误 401；过期 token 401；锁定。

- [ ] TDD → Commit: `feat: WebUI 用户名密码鉴权`

### Task 17: webapi/app.py + routes（核心 API）

**Files:** `sakura_bot/webapi/app.py`、`sakura_bot/webapi/routes/{health,auth,dashboard,forwarding,channels,system,logs}.py`、`tests/test_webapi.py`
端点：`GET /api/health`（db ping、两客户端状态、调度器/队列运行态）；`GET /api/dashboard`（统计汇总：转发今日/累计、规则数、频道数、连接态）；`/api/forwarding/rules` GET/POST/PUT/DELETE + `POST /{id}/backfill {limit}`（回溯补发，调 engine.backfill）；`/api/channels` CRUD；`/api/system/{status,pause,resume,restart,log-level,audit-logs}`；`WS /api/logs/stream`（RingBuffer 增量推送）；`POST /api/userbot/login {phone}`+`{code}`+`{password}` 登录向导（写 sessions 表，成功后热启动 UserBot）。重启=写 restart flag→exit(0)（systemd/compose 拉起）。全部写操作落 `system_audit_logs`。
测试：httpx AsyncClient + Fake 依赖注入 app.dependency_overrides——鉴权 401、CRUD 往返、health 各分支、WS（httpx 不支持 WS 则改用 starlette TestClient 同步路径）。

- [ ] TDD → Commit: `feat: WebUI 核心 API`

### Task 18: web/ 前端（Vue 3 SPA）

**Files:** `web/`（vite+vue-tsc+naive-ui；`package.json`、`src/main.ts`、`src/router`、`src/api`（axios 拦截器 JWT/401）、`src/stores/auth.ts`、`src/views/{Login,Dashboard,Forwarding,Channels,System,Logs}.vue`、`src/App.vue` 布局侧栏）
要点：深/浅色；Forwarding 页=规则表格 CRUD 抽屉 + 统计列 + 回溯按钮；System 页=状态卡片+暂停/恢复/重启+日志级别+审计表；Logs 页=WebSocket 实时流+级别过滤；UserBot 登录向导（Dashboard 内嵌：手机号→验证码→2FA）。
验收：`npm run build` 通过，产物 `web/dist/`（gitignore，镜像构建时生成）；`vue-tsc --noEmit` 无错误。手验路径记录在 README。

- [ ] 搭建 → 页面逐个实现 → build 通过 → Commit: `feat: WebUI 前端（Vue3+naive-ui）`

### Task 19: bootstrap.py + shutdown.py 接线

**Files:** `sakura_bot/bootstrap.py`、`sakura_bot/shutdown.py`、`sakura_bot/main.py`
要点：`AppContext` dataclass 注入容器；启动序=设计 §7（settings→日志→db→schema→config load→Bot start（strict 失败退出）→UserBot（lenient，失败仅告警+WebUI 向导可用）→sender.run 后台→engine.start→FeatureSwitch→CommandRouter→APScheduler（P1 任务挂载点，预留）→uvicorn serve→flush_loops→启动通知）；信号处理→`ShutdownManager` 逆序（停接收→drain 发送队列（超时 SHUTDOWN_TIMEOUT_TOTAL）→flush sessions→关调度→关 db→关 uvicorn）→退出码 0。单组件 init 异常按 strict/lenient 策略（Bot/DB=strict，其余 lenient 记 CRITICAL）。
测试：Fake 组件的启动/关闭顺序断言（记录调用序列）；lenient 组件抛错不中断。

- [ ] TDD → Commit: `feat: 启动编排与优雅关闭`

### Task 20: 部署物与文档

**Files:** `Dockerfile`（多阶段 node:20→python:3.13-slim、非 root、HEALTHCHECK /api/health）、`docker-compose.yml`（bot + mysql profile=local、mem_limit 512m、restart unless-stopped、TZ）、`deploy/sakura-bot.service`（systemd Restart=always EnvironmentFile=.env）、`.env.example`（设计 §5）、`README.md`（特性/快速开始 Docker 与 systemd/首次登录 UserBot 向导/迁移说明：旧 config.json→WebUI 手动迁移指引）

- [ ] 全部文件 → Commit: `chore: Docker/systemd 部署与 README`

### Task 21: 全量验证收尾

- [ ] `python -m pytest`（全绿，mysql 标记按环境）+ `ruff check .` + `npm run build`
- [ ] 冒烟：本地无凭据启动应给出清晰缺失项错误退出（exit 2）
- [ ] 更新 task_plan/progress/findings + 记忆文件 → Commit: `chore: P0 收尾验证`

---

## P1 批次（后续计划，此处仅列任务名）

定时总结全链（fetcher/report/jobs/水位）、订阅与推送、投稿全流程（用户侧状态机+管理审核）、评论区欢迎、投票与重生成、QA 用户命令其余项（/ask /subscribe /submit /request_summary）、Agentic RAG（embedder/store/rerank/qa_engine/conversation）、实时向量入库 worker、频道帖子聊天、en-US i18n、WebUI 对应页面（Schedules/Submissions/QA/Interaction/Stats）。

## P2 批次

编辑/删除同步（利用 target_message_id）、回溯补发增强、统计导出、备份脚本。
