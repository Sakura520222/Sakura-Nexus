# 02 存储

- 状态：📝 修订版（R2），待复核
- 受约束 ADR：[006](../decisions/006-rag-architecture.md) · [007](../decisions/007-scope-phases.md)
- 本文只定**数据模型与边界**；DDL 以 goose 迁移文件为准（字段表 + 关键约束在此评审）。

## 1. 总原则

### 1.1 Telegram ID 规范（全库统一，一次定死）

| 规则 | 内容 |
|---|---|
| 存储形态 | **一律存 MTProto 裸 ID（正数）**：`channel_id` = `tg.Channel.ID`、`user_id` = `tg.User.ID`、`message_id` = `tg.Message.ID` |
| SQL 类型 | 全部 `BIGINT UNSIGNED`（Telegram ID 空间远小于 2^63；驱动层统一按有符号扫描，应用层保证非负） |
| `-100` 转换边界 | Bot API 通道（ADR-008）出站时由 **platform/botapi 唯一负责**加 `-100` mark；库内任何表不出现带 mark 的 ID |
| `@username` | 永不作主键/外键；仅 `channels.username`、`forward_rules.source_username` 作为**解析辅助列**（可变，随时可能被改名/回收） |

### 1.2 时间字段语义（全库统一四件套，禁止裸 `timestamp` 命名）

```text
created_at   = 本系统记录写入时间
published_at = Telegram 原消息发布时间（tg.Message.Date）
edited_at    = Telegram 最近编辑时间（tg.Message.EditDate）
deleted_at   = 逻辑删除时间（本系统判定消息已删除）
```

存储：`DATETIME(6)`，**统一 UTC**（连接串 `loc=UTC` + parseTime）；展示层转时区。

### 1.3 基础

MySQL 8+（或 MariaDB 10.5+），`utf8mb4` / `InnoDB`；迁移由 goose 管理（`migrations/` embed，启动即 Up，见 01 §1.1）。

## 2. MySQL schema v2

### 2.1 Telegram 持久化状态（认证 session 与恢复状态分离）

**`gotd_sessions`** — 仅认证 session

| 字段 | 类型 | 说明 |
|---|---|---|
| account | `VARCHAR(8)` PK | `user` / `bot` |
| data | `MEDIUMBLOB` | gotd 序列化 session（DC、auth key 等**认证材料**） |
| session_version | `SMALLINT` | gotd session 结构版本（防降级写坏） |
| updated_at | `DATETIME(6)` | |

**`telegram_update_states`** — 全局 update 恢复状态（gaps recovery 的 pts/qts/date/seq/date_seq）

| 字段 | 类型 |
|---|---|
| account `VARCHAR(8)` PK | user / bot |
| pts / qts / seq `BIGINT UNSIGNED`，date `BIGINT` | |
| updated_at `DATETIME(6)` | |

**`telegram_channel_states`** — per-channel PTS（频道级增量恢复）

| 字段 | 类型 |
|---|---|
| PK(account, channel_id) | |
| pts `BIGINT UNSIGNED`、updated_at | |

**`telegram_peers`** — peer/access_hash 缓存（实体解析）

| 字段 | 说明 |
|---|---|
| peer_id BIGINT UNSIGNED PK | 裸 ID |
| peer_type `VARCHAR(8)` | user / chat / channel |
| access_hash `BIGINT` | API 访问凭据（可刷新） |
| username / title VARCHAR | 快照（辅助解析与展示） |
| updated_at | |

职责边界：**session blob 只含认证材料**；update 恢复状态与 peer 缓存独立持久化（gotd 各自的 storage 接口分别映射到上述表）。写入均为独立小事务、原子 REPLACE。

### 2.2 频道与转发（P0）

**`channels`**

| 字段 | 说明 |
|---|---|
| id BIGINT UNSIGNED PK AI | 内部主键 |
| tg_id BIGINT UNSIGNED UNIQUE | 频道裸 ID（唯一稳定标识） |
| username VARCHAR(64) NULL | 解析辅助（可变） |
| title VARCHAR(255) | 展示用快照 |
| discussion_chat_id BIGINT UNSIGNED NULL | 关联讨论群裸 ID（Telegram 提供，非猜测） |
| is_verified / added_at / updated_at | |

**`channel_settings`** — 频道级**配置**（运行水位不在此表，见 `summary_cursors`）

| 字段 | 说明 |
|---|---|
| channel_id BIGINT UNSIGNED PK → channels.tg_id | |
| summary_config JSON | 调度（frequency/days/hour/minute）、是否回源频道（P1，struct 校验） |
| poll_config / welcome_config JSON | P2 |

**`forward_rules`**

| 字段 | 说明 |
|---|---|
| id INT UNSIGNED PK AI | |
| name VARCHAR(128) NULL | |
| source_chat_id BIGINT UNSIGNED NULL + source_username VARCHAR(64) NULL | 二者至少一个（匹配：id 优先，username 辅助） |
| target_chat_id / target_username | 同上 |
| enabled TINYINT(1) | |
| keywords / blacklist / patterns / blacklist_patterns / media_types JSON | 过滤链（struct 校验：正则可编译） |
| forward_original_only TINYINT(1) | 只转原创 |
| copy_mode VARCHAR(16) | `copy`（默认，复制重发）/ `forward`（原样转发，需 Bot 在源频道） |
| ai_enabled TINYINT(1) + ai_prompt TEXT | 转发时 AI 改写（失败降级原文） |
| custom_footer TEXT | 底栏模板 |
| delay_min_sec / delay_max_sec FLOAT | 随机延迟区间 |
| last_message_id BIGINT UNSIGNED NULL | 规则级水位（回溯补发；属运行数据，但与规则生命周期一致，随规则删除） |
| created_at / updated_at | |

**`forwarded_messages`** — 去重 + 目标消息映射

| 字段 | 说明 |
|---|---|
| PK(source_chat_id, source_message_id, target_chat_id) | 去重键 |
| rule_id / target_message_id BIGINT UNSIGNED NULL | 映射（未来编辑/删除同步钩子） |
| content_hash CHAR(64) NULL | 可选内容哈希去重（settings.forwarding.content_dedup 开启时启用） |
| created_at | 保留期清理（dedup_days） |

**`forwarding_stats`** — PK(rule_id, stat_date)，forwarded_count / failed_count（真实成败计数）。

### 2.3 消息与修订（P0 建表，P1 开始使用——canonical 与 immutable 历史分离）

**`messages`** — 当前 canonical 状态（每 (chat_id, message_id) 一行）

| 字段 | 说明 |
|---|---|
| id BIGINT UNSIGNED PK AI | 内部主键（revisions 与 Qdrant 关联用它） |
| chat_id + message_id BIGINT UNSIGNED | UNIQUE(chat_id, message_id) |
| source_type VARCHAR(24) | `channel_message` / `discussion_message` / `bot_reply`（与 Qdrant kind 对齐） |
| conversation_id BIGINT UNSIGNED NULL | discussion/bot 消息所属会话（→ conversations.id；频道消息为 NULL） |
| thread_top_id BIGINT UNSIGNED NULL | 讨论线程顶层消息 ID（Telegram `reply_to_top_id`；非线程消息 = 自身 message_id） |
| sender_user_id BIGINT UNSIGNED NULL | 发送者（频道消息可 NULL） |
| sender_username / sender_display_name VARCHAR | 发送时快照（改名不影响身份判定） |
| text MEDIUMTEXT | 当前文本 |
| media JSON | 媒体**元数据**（mime、尺寸、file_reference 等）。注意：`file_reference` 是**可刷新的临时缓存引用**，不是永久媒体凭据——失效时须经 User 客户端重新解析刷新，过期即不可用 |
| ai_meta JSON NULL | AI 增强（categories/tags/keywords/entities/importance，reindex 可复用） |
| published_at / edited_at / deleted_at DATETIME(6) | 语义见 §1.2 |
| current_revision INT UNSIGNED | 当前修订号 |
| embedding_state TINYINT | 0=pending / 1=indexed / 2=excluded（P1 用） |
| created_at / updated_at | |

**`message_revisions`** — immutable 事件流（只 INSERT，永不 UPDATE/DELETE）

| 字段 | 说明 |
|---|---|
| id BIGINT UNSIGNED PK AI | |
| message_id BIGINT UNSIGNED → messages.id | FK，INDEX(message_id, revision) |
| revision INT UNSIGNED | UNIQUE(message_id, revision)；0 = 原始版本 |
| **event_type VARCHAR(8)** | `create` / `edit` / **`delete`**（Delete 也 append 不可变事件） |
| text MEDIUMTEXT NULL / media JSON NULL / ai_meta JSON NULL | 该修订快照（delete 事件为 NULL——事件本身即内容） |
| edited_at DATETIME(6) NULL | 该修订的 Telegram 编辑时间 |
| created_at | 本系统记录时间 |

**写入协议**（引擎内单一入口执行）：
- New：messages INSERT（revision 0）+ revisions INSERT(rev 0, create)。
- Edit：messages UPDATE（text/media/edited_at/current_revision+1）+ revisions INSERT(新 rev, edit)。
- Delete：messages UPDATE `deleted_at = now()`（canonical 软删）+ **revisions INSERT(新 rev, delete)**（不可变删除事件）+ Qdrant 删除 active point。删除内容退出普通知识检索；delete revision 本身可支撑时间线/审计需求（ADR-006）。

### 2.4 会话（P1 建表，P2 使用——三方身份显式建模）

**`conversations`** — 每篇频道帖 = 一个 AI 会话

| 字段 | 说明 |
|---|---|
| id BIGINT UNSIGNED PK AI | 内部主键 |
| channel_id + channel_post_message_id BIGINT UNSIGNED | UNIQUE(channel_id, channel_post_message_id)＝conversation_key |
| discussion_chat_id BIGINT UNSIGNED NULL | 该帖讨论所在群 |
| discussion_top_message_id BIGINT UNSIGNED NULL | 该帖在讨论群中的 auto-forward 顶层消息 |
| status VARCHAR(16) | `active` / `closed` / `orphan`（无讨论群/评论关闭） |
| created_at / last_active_at | |

### 2.5 总结（P1）

**`summary_cursors`** — 总结运行水位（与配置分离）

| 字段 | 说明 |
|---|---|
| channel_id BIGINT UNSIGNED PK | |
| last_summary_message_id BIGINT UNSIGNED | 已报告到的消息 ID（增量抓取排除） |
| last_summary_at DATETIME(6) | 上次总结完成时间 |
| updated_at | |

**`summaries`**

| 字段 | 说明 |
|---|---|
| id BIGINT UNSIGNED PK AI | |
| channel_id BIGINT UNSIGNED | |
| summary_type VARCHAR(8) | `auto` / `manual` |
| period_start / period_end DATETIME(6) | |
| text MEDIUMTEXT | |
| ai_model VARCHAR(64) | |
| source_revision_hash CHAR(64) | 生成时各源消息 revision 的聚合哈希（stale 判定） |
| is_stale TINYINT(1) | 底层消息编辑/删除后置位 |
| report_message_id BIGINT UNSIGNED NULL | 报告消息（水位衔接 + 深链） |
| created_at | |

**`summary_sources`** — 总结 ↔ 源消息明细（取代 JSON 列，支撑 stale 追踪）

| 字段 | 说明 |
|---|---|
| summary_id BIGINT UNSIGNED → summaries.id | PK(summary_id, message_id) |
| message_id BIGINT UNSIGNED | INDEX(message_id)：消息 Edit/Delete 时反查受影响总结 |
| revision INT UNSIGNED | 生成总结时的源消息修订号——`messages.current_revision > revision` 即可判定 stale |

### 2.6 用户 / 订阅 / 配额（P1/P2）

- **`users`**：tg_user_id PK、first_name、username（快照）、language、is_blocked、created_at、last_seen。
- **`subscriptions`**：PK(user_id, channel_id)。
- **`usage_quota`**：PK(user_id, quota_date)、used（P2 QA 配额）。

### 2.7 投稿 / 投票（P2 建表）

- **`submissions`**：id PK、user_id、title/content/media JSON、anonymous、signature、ai_polished_content、status(pending/approved/rejected/published)、reviewer_id、review_message_id、published_message_id、created_at/reviewed_at。
- **`poll_regenerations`** PK(channel_id, summary_message_id)、regen_count；**`poll_voters`** PK(channel_id, summary_message_id, user_id)。

### 2.8 运维

- **`system_audit_logs`**：id、actor（webui:username / tg:user_id / system）、action、detail JSON、created_at。
- **`reindex_state`** — reindex worker 的 checkpoint：

| 字段 | 说明 |
|---|---|
| id BIGINT PK AI | |
| collection VARCHAR(64) | `sakura_knowledge` / `sakura_conversations` |
| target_version INT | 正在构建的物理 collection 版本号 |
| status VARCHAR(16) | running / paused / done / failed |
| last_message_id BIGINT | 断点：已处理到的 messages.id |
| total / done INT | 进度 |
| error TEXT NULL | 最近错误 |
| started_at / finished_at | |

## 3. Qdrant 设计

### 3.1 collection 与 alias（blue/green）

```text
alias: sakura_knowledge    → 物理 sakura_knowledge_v3   ← 线上检索入口（永远经 alias）
构建中: sakura_knowledge_v4 ← reindex worker 写入 → 校验 → alias 原子切换 → 观察 → 删 v3
alias: sakura_conversations → sakura_conversations_v{N}（同机制）
```

### 3.2 point ID 规则（确定性，Edit 覆盖）

```text
UUIDv5(namespace=SakuraBot, name):
  message:{messages.id}          # 不含 revision —— Edit 后同 ID upsert，检索永远命中最新版
  summary:{summaries.id}
  vision:{media_ref_key}         # P2
```

### 3.3 vector 与 payload schema

- **named vectors 从第一天声明双槽**（P1 只填 dense；P2 启用 sparse 无需重建 collection）：
  - `dense`：vector size = embedding 维度（settings.ai，默认 1024），HNSW cosine
  - `sparse`：P2 由 BM25 路径填（Qdrant 原生 server-side sparse inference 优先，见 ADR-006；接口不绑定生成方式）
- payload（固定字段；**全文不进 payload**，检索命中后回 MySQL 取）：

| 字段 | 用途 |
|---|---|
| kind | channel_message / summary / discussion_message / bot_reply（与 messages.source_type 一致） |
| mysql_ref | {messages.id} 或 {summaries.id}（回表主键） |
| channel_id / message_id | 展示与回链 |
| source_type | 同 messages |
| published_at | datetime range filter |
| categories / tags / keywords / entities | filter（来自 ai_meta） |
| text_hash | 校验 |
| is_stale | summary 降权/排除 filter |

### 3.4 Source of Truth 边界（Invariant 1 的落地）

- MySQL 保存**完全重建 Qdrant 所需的一切**：canonical 文本与 media 元数据、可复用的 ai_meta、reindex_state checkpoint。
- **Qdrant vector 不回写 MySQL**；Qdrant 任何时刻可 DROP + reindex 重建。
- 回表失败（payload 命中但 MySQL 行已清理）→ 检索层跳过该 point 并记 metric。

### 3.5 保留策略（索引与真相源独立清理）

- **MySQL `messages`（含 discussion / bot_reply）：默认永久保留**——符合「所有聊天记录都记录」的目标；空间压力出现时再评估归档策略。
- `sakura_knowledge`（频道消息 + summary 向量）：默认**永久**。
- `sakura_conversations`（讨论/回复向量）：默认 **180 天**（settings.rag 可配），由维护任务按 `published_at` 清理 **Qdrant point**——不动 MySQL 行。

## 4. 配置数据边界汇总

| 数据 | 位置 | 变更入口 | WebUI 回显 |
|---|---|---|---|
| 凭据（bot token / api hash / mysql / qdrant / webui 密码） | `.env` only | 手工编辑文件 | 永不 |
| AI provider key | settings.ai（secret 字段） | WebUI / 命令 | `•••`+尾4 |
| 全局业务配置 | settings.*（scope→struct） | WebUI / 命令 / scheduler → **只经 config 中心** | 正常 |
| 频道级配置 | channel_settings | 同上 | 正常 |
| 转发规则 | forward_rules | 同上 | 正常 |
| 运行水位 | summary_cursors / forward_rules.last_message_id | 仅引擎写入 | 只读 |
| Telegram 确定元数据 | messages 等表 | 仅引擎写入 | 只读 |

## 5. 初始化与重建

- Sakura-Bot v2 从空 MySQL / 空 Qdrant **全新初始化**。
- MySQL schema 仅由 embed goose migrations 管理。
- **不支持导入旧 Sakura-Bot / TG-Forwarder 的配置、消息、session、向量或运行数据**。
- User 首次部署通过 v2 登录流程（WebUI 向导）建立新的 gotd session。
- Qdrant 始终由当前 MySQL Source of Truth 构建 / 重建（reindex worker，见 §2.8 与 ADR-006）。
