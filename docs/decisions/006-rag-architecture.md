# ADR-006：RAG / AI 架构 — MySQL Source of Truth + Qdrant Derived Index

- 状态：✅ 已拍板
- 日期：2026-08-29

## 背景

旧方案（Python 版草稿）「把 ChromaDB 换成 MySQL 向量存储」已彻底废弃。本项目的 RAG 需求已升级为**事件驱动知识库 + 多层会话记忆 + 多模态 Agentic RAG**。借鉴对象（2026-08 核实）：RAGFlow（SQL 与检索引擎分层、AI 自动元数据）、Dify（filter→召回→重排、多模态 rerank）、Open WebUI（BM25+vector hybrid + CrossEncoder rerank、知识与聊天记忆分离）、Mem0（SQL 事实存储 + vector 语义层、user/session 分 scope）、Qdrant（dense+sparse/BM25、RRF、multi-stage、metadata/datetime filter，v1.18 线、官方 Go client 活跃）。

## 决策

> **RAG / AI 架构：MySQL Source of Truth + Qdrant Derived Index**

```text
                Sakura-Bot（单进程，见 ADR-002）
                        │
        ┌───────────────┴───────────────┐
        MySQL                          Qdrant
   Source of Truth               Derived Search Index
   配置/用户/频道                  Dense Vector
   原始消息 + revision             BM25 Sparse
   会话/summary                   Metadata Index
   调度/投稿/状态                  RRF / Hybrid
   媒体元数据                     RAG Payload
```

**MySQL 是事实真相源，Qdrant 是可以随时重建的索引。** Qdrant 丢失不造成数据丢失（清空 → 遍历 MySQL active messages → 复用已有分类 → 重新 embedding → 重建）。组件实时协作仍走 Go channel（ADR-002 的「MySQL 禁止当 IPC」不冲突）。

### Architecture Invariants（不可破坏；为省代码绕过即视为架构违规）

1. `MySQL = Source of Truth`；`Qdrant = Derived / Disposable Index`
2. `Conversation Recording ≠ AI Reply Triggering`（记录与触发分离）
3. 检索组装管线：`Retrieve by Relevance → Rerank → Select → Order by Time → Build Context`

### 存储分层与双 collection

| 层 | 内容 | Scope |
|---|---|---|
| Channel Knowledge | 频道帖子、编辑、总结 | `channel_id` |
| Thread Memory | 帖子下全部用户聊天 + Bot 回复 | `channel_id + post_id` |
| User Memory | 可选的长期用户信息/偏好 | `telegram_user_id` |

物理上 Qdrant 分两个 collection，隔离群聊噪声与正式知识：

```text
sakura_knowledge
├── channel_message
├── summary
└── vision_description

sakura_conversations
├── discussion_message
├── bot_reply
└── extracted_memory
```

（群友的「笑死」「这角色老婆」不能与频道正式爆料获得同级知识可信度。）

### 消息数据模型（MySQL canonical）

至少保留：`channel_id`、`message_id`、`revision`、`event_type(create/edit/delete)`、`published_at`、`edited_at`、`deleted_at`、`source_type(channel_message/summary/discussion/memory)`、`text`、`media[]`、`thread_id`、`telegram_user_id`、`username`、`categories[]`、`tags[]`、`keywords[]`、`entities[]`。

- **频道、时间、消息类型是 Telegram 提供的确定 metadata，绝不交给 AI 猜。**
- AI 只负责：topic、subtopic、tags、keywords、entities、importance、image_description。
- 分类采用**固定大类（closed taxonomy，WebUI 可管理）+ 开放标签**，避免「游戏新闻/游戏资讯/game_news」语义漂移。

### 事件驱动 ingest

- **NewMessage**：MySQL 保存 canonical → AI 分类/关键词/实体 → 图片走 Vision 解析 → embedding → Qdrant upsert。
- **MessageEdited**：`revision += 1`，保留旧 revision，更新 current，重新分类与 embedding，**相同 logical ID upsert**——搜索默认只命中最新版，历史修订仍在 MySQL。
- **MessageDeleted**：MySQL `deleted_at = now`；Qdrant 删除 active point——**删除前内容不再进入普通知识检索**（用户删了内容 AI 还在答，不可接受）；可选记录「#xxx 于某时被删除」的时间线事件。

### Summary 为一级知识文档 + stale 机制

自动/手动总结均作为 `source_type = summary` 入索引（channel_id、period、summary_type、source_message_ids[]）。每份 summary 保存 `source_revision_hash`；底层消息被大幅编辑或删除后 `is_stale = true` → 按策略**自动重新生成**或**检索时降权/排除**。（旧项目「总结写进去永远有效」的问题修正。）

### 六阶段检索管线

```text
用户问题
  ↓ ① Query Analyzer（channel scope / 时间范围 / category / entity / source 偏好）
  ↓ ② Metadata Filter
  ↓ ③ Dense 语义召回 + BM25 词法召回
  ↓ ④ RRF Fusion
  ↓ ⑤ Reranker
  ↓ ⑥ Context Builder（按 published_at 时间序重排）
  → LLM
```

关键原则：**检索阶段按相关度排序，喂给 AI 前再按时间排序**（50 candidates → rerank → 8~15 chunks → 按 published_at ASC）。不要直接 ORDER BY time 做 RAG。

### Rerank 的 API 现实

OpenAI 标准 `/v1` 目前**没有通用 `/v1/rerank`**。第一版：Hybrid 召回 → 20 candidates → Chat/Responses **LLM Listwise Rerank**（结构化返回 `[{"id":..,"score":..}]`）→ 8~12 结果。接口预留：

```text
Reranker interface
├── LLMReranker          （第一版）
└── DedicatedReranker    （未来接 Jina/Cohere/SiliconFlow 的 /rerank）
```

**边界澄清**：核心 AI Provider 矩阵为 Embedding / Chat+Agent / Vision / Classification / Summary / Query Analyzer / LLM Reranker，**全部经 OpenAI-compatible API**（已拍板）。`DedicatedReranker` 属于**可选检索扩展适配器**，不是核心 `AIProvider` 的组成部分（Jina/Cohere/SiliconFlow 专用 rerank API 未必遵循 OpenAI 协议）。第一期只用 OpenAI-compatible Chat/Responses 做 listwise rerank，不引入第二套 AI API 协议。

### AI Provider 统一

全部 AI 能力走一套 OpenAI-compatible provider（openai-go + WithBaseURL）：embedding（`/v1/embeddings`）、classification / summary / query analysis / rerank（Chat/Responses）、vision（`input_image`）。

### 多模态双层表示

```text
Telegram 图片
  ├─ 原始图片/媒体引用（保留）
  └─ Vision Model → caption / OCR / objects / entities / tags
        ↓ 文本 embedding → Qdrant
```

检索用描述层；**回答时把真正的原图 + 文字上下文一起交给多模态模型**（能搜「立绘里那个蓝色东西」，回答时模型重新看原图，而非只信旧 caption）。

### 频道讨论群：每篇帖子 = 一个 AI 会话

Telegram 官方机制：频道关联 discussion group 后，每篇 post 自动转发到群，其评论区就是该 auto-forwarded message 的 thread（`reply_to_top_id`）；真实账号可调 `messages.getDiscussionMessage`（user-only，符合「User 抓取」原则）。

```text
conversation_key = channel_id + channel_message_id
channel post 233 → discussion top message 18527 → reply_to_top_id = 18527 → 同一个 AI Conversation
```

- **记录与触发完全分离**：讨论群**所有消息永远记录**并加入该帖子 conversation（不管有没有 @Bot）；仅 @Bot / reply Bot 触发 AI 回复。触发时上下文 = 该帖子近期完整对话（含未 @Bot 的消息）+ thread memory + channel RAG。
- **所有会话都具有调用 RAG 的能力。**
- 边缘情况容忍：帖子未关联讨论群/评论关闭时 `getDiscussionMessage` 失败；非 thread 消息无 `reply_to_top_id`，fallback 到 message_id 本身。

### 用户消息：存储原始，渲染在 prompt 层

数据库保存 `telegram_user_id / username / display_name / raw_text / timestamp`；进入模型上下文时才渲染为「用户 123456789/@alice 说：……」。用户改名后仍能靠 Telegram ID 区分同人。

### 实施补充约束（拍板时追加）

1. **Qdrant 是部署新增外部服务**（docker-compose 服务或裸机二进制 + systemd），部署文档须覆盖两种形态。
2. **BM25 sparse 路径在实施期实测决定**：优先评估 Qdrant 原生 BM25 / server-side sparse inference（Qdrant 全文排名以 sparse vector 为基础，1.15.2 起可直接在服务端从输入文本生成 BM25 sparse embedding）；fallback 为应用侧生成 sparse vectors；若 OpenAI-compatible provider 后续提供高质量 lexical/sparse embedding，也可作为第三种实现。**检索层接口不得绑定具体 sparse 生成方式。**
3. **reindex 是正式 worker + blue/green 版本化重建**（非一次性脚本、非原地清空）：

```text
sakura_knowledge_v3    ← 当前 alias
        │ 后台构建（checkpoint / rate limit / retry / progress / validation）
        ▼
sakura_knowledge_v4 ── atomic alias switch ──▶ sakura_knowledge → v4
        │ 验证稳定
        ▼
清理 v3
```

   embedding API 限流、服务器重启、某批消息失败均不影响线上现有检索。
4. 架构拍板 ≠ 全量进第一期实施；分期切分在范围分级（第七问）确定。

## 备选与否决理由

- **向量塞 MySQL（旧方案）**：废弃。需求已远超「简单向量化」，SQL 数据库不应同时承担全文+向量+过滤的检索引擎角色。
- **PostgreSQL + pgvector**：能做 HNSW + FTS + hybrid，但复杂 metadata filter 配合 HNSW 需要 iterative scan/partial index/partition 调优；适合「只想维护一个数据库」的场景，非本项目首选。
- **Elasticsearch/OpenSearch**：BM25/vector/RRF/rerank 最成熟（RAGFlow 默认），但与「长期、低占用、单服务器」明显偏重。
- **为 RAG 换掉 MySQL**：不必要——MySQL 继续做业务真相源，RAG 走 Qdrant，分层清晰。

## 来源

[Qdrant Full-Text Search（BM25/sparse）](https://qdrant.tech/documentation/search/text-search/full-text-search/) · [Qdrant Sparse Retrieval Demo](https://qdrant.tech/course/essentials/day-3/sparse-retrieval-demo/) · [RAGFlow](https://github.com/infiniflow/ragflow) · [RAGFlow ingestion extractor](https://github.com/infiniflow/ragflow/blob/main/internal/ingestion/component/schema/extractor.go) · [RAGFlow AutoKeyword/AutoQuestion](https://github.com/infiniflow/ragflow/blob/main/docs/guides/dataset/advanced/autokeyword_autoquestion.mdx) · [Dify Knowledge Retrieval](https://github.com/langgenius/dify-docs/blob/main/en/cloud/use-dify/nodes/knowledge-retrieval.mdx) · [Open WebUI RAG](https://docs.openwebui.com/features/chat-conversations/rag/) · [Qdrant Hybrid Queries](https://qdrant.tech/documentation/search/hybrid-queries/) · [pgvector](https://github.com/pgvector/pgvector) · [Elastic hybrid search](https://www.elastic.co/docs/solutions/search/hybrid-search) · [OpenAI 平台文档](https://platform.openai.com/docs/models/default-usage-policies-by-endpoint) · [Telegram: Discussion groups](https://core.telegram.org/api/discussion) · [Telegram: Threads](https://core.telegram.org/api/threads) · [messages.getDiscussionMessage](https://core.telegram.org/method/messages.getDiscussionMessage) · [Mem0 架构](https://github.com/mem0ai/mem0/blob/main/skills/mem0/references/architecture.md) · [qdrant/go-client releases](https://github.com/qdrant/go-client/releases)
