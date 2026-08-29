# Sakura-Nexus（原 Sakura-Bot 重写）— 任务计划

> 创建：2026-08-29 · 当前状态：**P0 实施中（T1.1 代码完成，S 冒烟待 TELEGRAM_API_ID/HASH）**

## 目标

将 `E:\项目\Sakura-Bot`（主项目）重写至本仓库，并整合 `E:\项目\TG-Forwarder` 的转发功能。

## 硬性约束（用户指定，优先级最高）

1. 在 Linux 服务器上稳定、长期、低占用运行
2. 所有数据、配置均写入 MySQL 数据库（用户于 2026-08-29 放宽：RAG 检索索引可由 Qdrant 承担，但 Qdrant 仅为可重建的派生索引，真相源仍是 MySQL）
3. `.env` 仅配置：Telegram Bot Token、WebUI 用户名/密码、真实 Telegram 用户账号相关配置
4. 所有消息抓取/获取由真实账号执行；所有发送由 Bot 账号执行
5. 原项目的两个 Bot 收敛为 **1 个 Bot**（2026-08-29 追加）
6. 技术栈：**Go**（2026-08-29 拍板，替换原 Python 方向）

## 已拍板决策（详见 [docs/decisions/README.md](docs/decisions/README.md)）

| # | 决策 |
|---|---|
| 001 | Go + gotd/td（User 抓取 / Bot 发送，无降级路径；gotd/botapi 不用） |
| 002 | 单二进制 + 单进程 + 多 goroutine；channel/context 协作；MySQL 禁止当 IPC；核心组件 fatal → 优雅退出 → systemd 兜底 |
| 003 | WebUI = SPA + go:embed；配置真相源唯一（全部走 Go service 层）；WebSocket 只做实时流 |
| 004 | 前端 = Vue 3 + TS + Vite + Naive UI；无 Nuxt；UI 库不泄漏业务层 |
| 005 | Go 基础库：go-sql-driver/mysql + sqlx + 标准库 ServeMux + slog + gocron v2 + goose + validator/v10 + openai-go + coder/websocket + godotenv |
| 006 | RAG：MySQL SoT + Qdrant 双 collection；事件驱动 ingest（revision/stale summary）；六阶段检索；LLM rerank 起步；多模态双层；每帖一 conversation，记录/触发分离 |

## 阶段

- [x] 阶段0 调研：两个源项目深挖完成（docs/research/）
- [x] 阶段0.5 核心架构决策：七项 ADR 拍板并分类落盘（001–007，目标架构冻结）+ ADR-008
- [x] 阶段1 范围分级（ADR-007）
- [x] 阶段2 总体设计：overview + 01–07（R3.1.1）——**2026-08-29 用户正式批准，全部冻结**
- [ ] 阶段3 Go P0 实施计划：已编写（docs/plans/p0-implementation.md），**待用户批准**——批准前零实现代码
- [ ] 阶段4 P0 实施（Phase 0–6、GATE-1~4 硬门禁；风险前置：第 4 个任务即首次连接真实 Telegram）

## 事件记录

- 2026-08-29：未经批准即实施被用户叫停，Python 实现代码已全部撤回（见 git 历史）。
- 2026-08-29：用户重新定向技术栈为 Go，六项决策逐问拍板，废弃 Python 版设计草稿与 P0 计划（git 历史可查），决策分类落盘 docs/decisions/。
