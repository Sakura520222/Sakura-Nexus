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
