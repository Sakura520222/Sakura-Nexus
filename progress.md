# 进度日志

## 2026-08-29 · 会话 1

- 确认任务：重写 E:\项目\Sakura-Bot + 整合 E:\项目\TG-Forwarder → 本仓库。
- 记录硬性约束（见 task_plan.md），含用户追加要求：原两个 Bot 收敛为 1 个。
- 建立规划文件 task_plan.md / findings.md / progress.md。
- 派出 2 个并行 Explore 子代理分别调研两个源项目（后台运行中）。
- 已知环境问题：claude-mem observer 持续失败（OpenRouter 503），本会话记忆由文件式记忆承担。
- TG-Forwarder 调研报告返回并落盘 `docs/research/tg-forwarder-report.md`；findings.md 已更新摘要。
- 等待 Sakura-Bot 源项目调研子代理返回。

## 2026-08-29 · 会话 1（续）：叫停与撤回

- 用户叫停：设计未经批准就开始写实现代码（违反 brainstorming 审批门禁），自主模式的判断错误。
- `git reset --hard a412f9f`：撤回全部 6 个实现 commit（骨架/settings/db/事件总线/配置中心），清理全部未跟踪代码残留与缓存。
- 保留：docs/（调研报告 ×2、设计文档草稿、P0 计划草稿）、规划文件、.gitignore。
- .venv 保留在磁盘（未跟踪，依赖已装好，批准后可直接复用）。
- 当前状态：**等待用户审阅设计文档**。

## 2026-08-29 · 会话 1（续）：技术栈重定向与决策拍板

- 用户重新定向：技术栈弃 Python 改 **Go**，逐问拍板制推进。
- 依次拍板六项（001 Go+gotd/td；002 单二进制单进程多goroutine；003 SPA+go:embed；004 Vue3+TS+Vite+Naive UI；005 Go 基础库包；006 RAG=MySQL SoT+Qdrant），期间实时核实了 2026-08 生态维护状态（GramJS/Telethon 归档、robfig/cron 失维护、coder/websocket、goose、openai-go 等）。
- 六项决策分类写入 docs/decisions/（ADR 风格）；废弃并删除 Python 版设计草稿与 P0 计划。
- 下一步：第七问范围分级 → 总体设计文档（含 gotd session 入 MySQL 方案、.env v2）→ Go 版实施计划。
- 注意：docs/telegram-bot-api-10.2-rich-markdown-zh.md 为用户自行放入的文件，未纳入提交。

## 2026-08-29 · 会话 1（续）：第六项澄清 + 第七项拍板

- ADR-006 两处措辞级修订：BM25 sparse 实施期实测三路径（Qdrant 原生 server-side sparse inference 优先 / 应用侧 fallback / provider sparse 第三路），接口不绑定生成方式；DedicatedReranker 明确为可选检索扩展适配器（非核心 AIProvider）。
- 三条 Architecture Invariants 正式写入 ADR-006；blue/green + alias reindex 正式接受。
- ADR-007 拍板：P0=A+B+C（production vertical slice）；P1=D+F+RAG Query Harness；P2=E+G；强制「分期只裁功能不裁架构」（Retriever/Reranker/VisionProcessor/QueryAnalyzer/MemoryStore 第一天留接口边界）。
- 目标架构（001-006）冻结。下一步：总体设计文档结构给用户审 → 成文 → Go 版 P0 实施计划 → 确认后才动代码。

## 2026-08-29 · 会话 1（续）：ADR-008 与总体设计结构落盘

- 新增 ADR-008（Rich Message 经 Bot API HTTP 出站，ADR-001 唯一专项例外；net/http 直调、不新增 SDK；Draft 仅私聊、thread≠forum topic 两条硬限制；renderer 确定性校验与 block 边界切分）。001 与索引已同步标注例外关系。
- 事实澄清：16 章结构原提案中并无 Rich Markdown 项（源自用户放入的 Bot API 10.2 资料）。
- 总体设计结构定稿：overview + 7 领域文件（用户折中方案）。overview.md 已成文（架构总图 / 数据流 / invariants / 文档地图 / 分期）；01–07 骨架就位（含受约束 ADR 与章节映射，用户三处补充落在 01/03/05）。
- 下一步：按 01→07 顺序逐文件成文 → 整包交用户审 → 批准后写 Go 版 P0 实施计划 → 计划确认后才动代码。

## 2026-08-29 · 会话 1（续）：overview invariant 定稿 + 01/02 成文

- overview 四条 invariant 定稿（第 4 条提升为通用「业务逻辑依赖抽象而非基础设施实现」+ 具体禁止项清单）。
- 01-runtime-and-components.md 成文：composition root/service 抽象、CORE/DEGRADED 两类服务与 fatal 判定、包结构与依赖方向（depguard CI 强制、接口消费者定义、反上帝对象）、P0 接口与 P1/P2 预留边界（只留签名不做假实现）、SendRequest 统一出站模型（Rich 不长平行业务 API）、channel 三规则（owner/容量/关闭责任）、.env v2 清单、settings scope 表、secrets 不回显。
- 02-storage.md 成文：Telegram ID 规范（裸 ID + -100 转换唯一边界在 platform/botapi）、时间四件套语义、gotd_sessions（version+原子写）、messages canonical / message_revisions immutable 分离与写入协议（New/Edit/Delete）、conversations 三方身份建模、summaries+stale、Qdrant 双 named vector（dense+sparse 预留，P2 不重建 collection）、point UUIDv5 不含 revision（Edit 覆盖）、SoT 边界（vector 不回写 MySQL）、reindex_state checkpoint、v1→v2 迁移映射（含 Telethon session→gotd 转换命令）。
- 状态：01+02 待用户审；03-07 未动；无实现代码。

## 2026-08-29 · 会话 1（续）：01/02 集中修订（R2）

用户复核指出必改项（注：按本会话记录为首次收到的具体修订要求，已全部执行）：
- 01：+platform/ai（openai-go 入 depguard 禁令）；Sender 改各消费者最小接口、MessageRenderer 下沉 platform/telegram 内部；服务状态拆 DEPENDENCY_UNAVAILABLE（存活等依赖、不重启）/OWN_FATAL（退避重启）；生命周期统一「构造→注册→supervisors 启动→readiness barrier」，WebServer 为普通 CORE service；公开 /api/health 仅 status/version/uptime，细项移鉴权后 /api/system/status；删除 systemd watchdog 表述（仅 Restart=on-failure）。
- 02：Telegram 持久化拆四表（gotd_sessions 仅认证 / telegram_update_states / telegram_channel_states / telegram_peers）；删除全部 v1 迁移内容，改「初始化与重建」（全新初始化、无导入）；+summary_cursors（水位与配置分离）；+summary_sources(summary_id,message_id,revision)（stale 追踪）；message_revisions 增 event_type（create/edit/delete），Delete 也 append 不可变事件；messages.source_type 补 bot_reply；media.file_reference 标注为可刷新缓存引用；保留策略改为 MySQL messages 永久 / Qdrant conversations 默认 180 天（只清索引不动 MySQL）。
- overview：MySQL 列表述更新为 Telegram persistent state（session + update state + peer cache）；架构图误删的 settings 配置中心已恢复。
- 状态：01/02 R2 版待用户快速复核；通过即冻结，再继续 03–07。

## 2026-08-29 · 会话 1（续）：01/02 冻结（R3）+ 03–07 成文

R3 修订（用户复核的 5+2 项，全部落实）：
- 01：Ready channel 改 Availability 接口（IsReady/WaitReady/SubscribeState，可重复连接循环）；settings.ai 从 P0 起存在（AISettings 字段按期启用：P0 base_url/api_key/rewrite_model/temperature/timeout）；RAG 队列改名 derived-index queue（canonical 先落库不可丢；Delete invalidation 禁止静默丢弃）。
- 02：telegram_update_states/telegram_channel_states 增 user_id 身份分区（gotd StateStorage 语义）+ 换号清旧；telegram_peers 改 PK(account, peer_type, peer_id) + storage.Peer 序列化（对齐 contrib PeerStorage，ID 空间重叠/access_hash 不跨 session）；gotd_sessions 删 session_version（opaque blob 不解析）；写语义 REPLACE→INSERT…ON DUPLICATE KEY UPDATE；Delete 明确 current_revision += 1。
- 01/02 标记冻结。
按用户预授权继续成文 03–07（首次成文，待审）：03 gotd 集成/状态映射/相册算法/8.x Rich Rendering/转发引擎；04 页面/API/JWT/WS/Harness；05 AIProvider 矩阵/AIResponse/ingest/检索管线/reindex/分类/会话；06 systemd+docker+compose(profile full)/备份/可观测/安全/资源预算；07 测试策略(golden)/CI/里程碑验收/术语与功能对照。
