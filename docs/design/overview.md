# Sakura-Bot v2 总体设计 — 总览（overview）

- 状态：总览已成文；领域设计文件成文中
- 定位：**本文档系只做技术细化、不做新决策**；凡与 ADR 冲突，以 [ADR（docs/decisions/）](../decisions/README.md) 为准。
- 目标架构基线：ADR 001–008（已冻结，001–006 为目标架构，007 为分期，008 为唯一专项例外）。

## 1. 架构总图

```text
┌────────────────────────── sakura-bot（单二进制 / 单进程 / 多 goroutine）──────────────────────────┐
│                                                                                                  │
│  App（root context 统一生命周期；核心组件 fatal → 全局优雅退出 → systemd Restart=on-failure）        │
│   ├─ TelegramSupervisor                                                                          │
│   │   ├─ UserClient（gotd/td MTProto，真实账号）   ← 唯一抓取者：监听 / 历史抓取 / Edit·Delete 事件    │
│   │   │                                             / 媒体下载 / 加频道 / getDiscussionMessage       │
│   │   └─ BotClient（gotd/td MTProto，唯一 Bot）    ← 默认发送通道：文本 / 媒体 / 转发 / 命令 / 回调    │
│   │                                   ┌──────────────────────────────────────┐                    │
│   │                                   │ 例外通道（ADR-008）：Rich Message      │                    │
│   │                                   │ AI 回复 / 总结 → MessageRenderer       │                    │
│   │                                   │ → Bot API HTTP sendRichMessage        │                    │
│   │                                   └──────────────────────────────────────┘                    │
│   ├─ ForwardingService（规则 / 过滤链 / 相册聚合 / 去重 / 限流 / 统计）                                │
│   ├─ SummaryService（P1：调度 / 增量抓取 / 水位 / LLM 总结 / 订阅推送）                                 │
│   ├─ RAGService（P1 起：ingest / retrieval / reindex；P2：hybrid / memory / agentic）                  │
│   ├─ Scheduler（gocron：总结任务 / 清理维护 / reindex worker）                                        │
│   └─ WebServer（标准库 ServeMux：/api/* REST + WebSocket 实时流 + SPA 静态 + history fallback）        │
│                                                                                                  │
│   组件协作：channel + context + service interface；MySQL 禁止充当内部 IPC（ADR-002）                  │
└──────────────────────────────────────────────────────────────────────────────────────────────────┘
        │                                              │                              │
        ▼                                              ▼                              ▼
  MySQL（Source of Truth）                     Qdrant（Derived / Disposable）   Telegram Bot API
  配置 / 频道 / 规则 / canonical message         sakura_knowledge                （HTTP，仅 Rich Message，
  + revisions / conversations / summaries        sakura_conversations            复用同一 Bot token）
  / 调度 / 投稿 / 订阅 / 审计 / settings 配置中心   dense（P2 +sparse/RRF）         ADR-008 专项例外）
  / Telegram persistent state：                   alias + blue/green reindex
    session + update state + peer cache
```

## 2. 核心数据流

1. **转发（P0）**：User gotd 事件 → 规则匹配 → 过滤链（原创/关键词/正则/黑名单/媒体类型）→ 去重 → （可选 AI rewrite）→ 底栏 → Bot gotd 发送（文本 entities 透传 / 媒体下载重传 / 相册重建）→ 落库统计。
2. **RAG ingest（P1）**：Telegram New/Edit/Delete → MySQL canonical message + revision →（AI 增强：分类/标签/关键词/实体）→（P2：图片 Vision 描述）→ dense embedding → Qdrant upsert（Edit 同 logical ID 重 upsert；Delete 删 point）。
3. **检索管线（P1 dense 版，P2 完整）**：Query →（P2 Query Analyzer）→ metadata filter → dense 召回（P2 +BM25 sparse）→（P2 RRF）→（P2 rerank）→ 按时间排序 → Context Builder → LLM。
4. **Rich Message 出站（ADR-008）**：AI raw output → RichMarkdownNormalizer → validate（限制数字 / block 边界切分）→ sendRichMessage → 失败 retry / safe fallback；私聊可 Draft 流式预览。

## 3. Architecture Invariants（绕过即架构违规）

```text
1. MySQL = Source of Truth；Qdrant = Derived / Disposable Index
2. Conversation Recording ≠ AI Reply Triggering
3. Retrieve by Relevance → Rerank → Select → Order by Time → Build Context
4. Business Logic Depends on Interfaces, Not Transport / Storage Implementations
```

第 4 条的具体禁止项：

```text
SummaryService / RAGService / ForwardingService / ConversationService
    ✗ net/http Telegram Bot API 直调
    ✗ gotd 具体 client
    ✗ qdrant 具体 client
    ✗ sqlx 裸 DB 调用
        ↓ 必须依赖
Sender / Fetcher / Retriever / Repository / AIProvider 等接口
```

ADR-008 的「`AIProvider` 只产出通用 `AIResponse`、Rich Markdown 由 presentation 层负责」是第 4 条的一个具体实例。接口定义与依赖方向细化见 [01-runtime-and-components.md](01-runtime-and-components.md) 第 2–3 节。

## 4. 文档地图（overview + 7 个领域文件）

| 文件 | 覆盖（原 16 章映射） | 受约束 ADR | 状态 |
|---|---|---|---|
| [01-runtime-and-components.md](01-runtime-and-components.md) | 3 进程生命周期 · 4 代码组织与接口边界（含 Sender 出站抽象）· 7 配置体系（.env v2、settings scope、加载与热更） | 001 002 003 005 008 | ✅ 已冻结（R3.1） |
| [02-storage.md](02-storage.md) | 5 MySQL schema v2 · 6 Qdrant 设计 | 006 007 | ✅ 已冻结（R3.1） |
| [03-telegram-and-forwarding.md](03-telegram-and-forwarding.md) | 8 Telegram 集成（含 8.x Bot 出站传输与 Rich Message Rendering）· 9 转发引擎 | 001 002 008 | 📝 R3.1.1 待核对 |
| [04-webui-and-api.md](04-webui-and-api.md) | 10 WebUI 与 API（页面/路由/DTO/server-side 会话鉴权/WebSocket/RAG Query Harness） | 003 004 | 📝 R3.1.1 待核对 |
| [05-ai-and-rag.md](05-ai-and-rag.md) | 11 AI Provider（含 AI 输出契约与 Answer 能力）· RAG 管线细化（索引状态机） | 006 007 008 | 📝 R3.1.1 待核对 |
| [06-deployment-security-observability.md](06-deployment-security-observability.md) | 12 部署 · 13 可观测性与安全 | 002 005 006 | 📝 R3.1.1 待核对 |
| [07-testing-milestones-reference.md](07-testing-milestones-reference.md) | 14 测试与 CI · 15 里程碑对照 · 16 附录（术语/功能对照参考） | 007 | 📝 R3.1.1 待核对 |

原 16 章中的第 1 章（文档定位）与第 2 章（架构总图）由本 overview 承担。

## 5. 分期与验收（摘要，全文见 [ADR-007](../decisions/007-scope-phases.md)）

- **P0 = A+B+C**：Telegram 基础设施 + 转发全量 + WebUI 管理面的 production vertical slice（gotd 双客户端稳定性与单二进制部署是本阶段要消灭的两大风险；不要求替代完整旧 Sakura-Bot）。
- **P1 = D+F+RAG Query Harness**：总结完整恢复 + Dense RAG 全链路（无查询出口即无法验收）。
- **P2 = E+G**：互动体系 + Hybrid / Multimodal / Agentic RAG / 讨论会话。
- 分期只裁功能不裁架构：`Retriever / Reranker / VisionProcessor / QueryAnalyzer / MemoryStore` 接口第一天留边界，P1 允许 no-op/basic 实现。
