# 05 AI 与 RAG

- 状态：📝 R3.1.1，待用户核对修改点
- 受约束 ADR：[006](../decisions/006-rag-architecture.md) · [007](../decisions/007-scope-phases.md) · [008](../decisions/008-rich-message-transport.md)

## 1. AIProvider（platform/ai，openai-go + `option.WithBaseURL`）

| 能力 | 方法 | 用途 | 期 |
|---|---|---|---|
| Chat（rewrite） | `Rewrite(prompt, text)` | 转发改写（失败降级原文） | P0 |
| Chat（answer/generate） | `Answer(query, context)` | RAG Harness 回答 / 会话 AI 回复（R3.1 补入能力表——此前 Harness 与 conversation 调用了未列出的能力） | P1 |
| Chat（summary） | `Summarize(prompt, messages)` | 频道总结 | P1 |
| Chat（classification） | `Classify(taxonomy, text)` | 消息分类/标签/实体增强 | P2 |
| Chat（query analysis） | `AnalyzeQuery(q)` | 检索管线第 ① 阶段 | P2 |
| Chat（listwise rerank） | `Rerank(q, candidates)` | 检索管线第 ⑤ 阶段 | P2 |
| Embedding | `Embed(texts)` | 向量化 | P1 |
| Vision | `Describe(image)` | 图片结构化描述（结果持久化于 `media_analyses`，02 §2.3） | P2 |

- 配置：`settings.ai`（P0 起存在，字段按期启用，见 01 §6.2）；变更回调热重建客户端。
- 超时/重试/降级按 03 §1.4 矩阵；**所有能力走同一 OpenAI-compatible provider**（ADR-006）。

## 2. AIResponse 输出契约（无 Telegram 类型）

```go
type AIResponse struct {
    Text      string            // 主体内容（Rich Markdown 优先输出，见 §3）
    MediaRefs []domain.MediaRef // 可选：生成过程引用的媒体
    Metadata  map[string]any    // model、usage、finish_reason 等
}
```

渲染与发送由 Telegram presentation 层（03 §2）负责——AIResponse 可原样复用于 WebUI（RAG Harness 的 answer 展示即直接渲染此结构）。

## 3. 输出格式策略

- system prompt 指令：输出 Telegram Rich Markdown（标题/列表/表格/引用/代码块/公式/链接；禁止 HTML）。
- **deterministic renderer 保证可发送**（03 §2.2–2.3）：LLM 尽量生成正确，程序保证一定能发出；第一版仅 `rich_message.markdown` 一种表达。

## 4. RAG ingest（P1 起；数据流边界见 01 §5.2）

**索引生命周期状态机（R3.1：MySQL durable state → repairable derived job → Qdrant 的 eventual consistency）**

```text
Telegram New/Edit/Delete
  → MySQL canonical + revision（单一写入协议，02 §2.3；不可丢）

消息（messages.embedding_state）：
  New    → pending   ──embedding+upsert──▶ indexed
  Edit   → pending（revision+1，重嵌入同 UUID 覆盖）
  Delete → delete_pending（与 deleted_at 同一事务内置！）──Qdrant delete──▶ excluded
  失败   → error（记原因，repair 重试）
Summary（summaries.index_state）：
  创建   → pending ──upsert──▶ indexed；失败 → error
  （R3.1：修复「INSERT commit 后、入队前崩溃 → 永久漏索引」的 crash window）
media_analyses / user_memories（P2）：同一状态机
```

- **repair 任务**（周期维护）统一扫描全部 SoT 表的 `pending` / `error` / `delete_pending`：补做嵌入/删除 → 收敛到 indexed/excluded。队满丢弃与崩溃窗口都由它兜底——**Qdrant 相对 MySQL 最终一致**。
- Delete 在**同一 MySQL 事务**内写 `deleted_at + current_revision+1 + embedding_state=delete_pending`，不存在「commit 完成但无任何持久痕迹」的窗口；队列中的 invalidation 只是加速器，repair 才是保证。
- P1 不做 AI 增强（ai_meta 留空）；P2 增加分类/标签/实体（写入 ai_meta，payload 同步）+ vision（media_analyses）+ user memory 抽取（user_memories）。
- 消息选取白名单：哪些频道进入 knowledge 索引由频道设置控制（channel_settings.rag_config，P1）。

## 5. 检索管线（P1 dense 子集 → P2 完整六阶段）

```text
P1：query → metadata filter（channels[]/time range/kinds[]）→ dense Top-K
    → 回表（MySQL 取全文）→ 按时间排序 → Context Builder
P2：+ Query Analyzer（①）→ filter（②）→ dense+BM25 双路（③）→ RRF（④）
    → Reranker（⑤ LLM listwise；预留 DedicatedReranker 适配器）→ Context Builder（⑥）
```

- 接口即 01 §3.2 的 `Retriever`；P1 实现只有 dense 分支，业务层无感。
- 回表失败（point 命中但 MySQL 行已清理）→ 跳过并记 metric（02 §3.4）。

### Context Builder 规格（R3.1：顺序修正）

严格遵循 Invariant 3 的完整顺序——**时间排序之后不得再做任何裁剪**：

```text
relevance ranking → rerank → token-budget selection（按相关度从高到低装满预算，得到 selected set）
→ chronological sort（selected set 按 published_at 升序）→ context
```

- 每条目格式：`{时间, 作者(user_id/username), 频道, 文本, t.me 深链}`；token 预算可配（默认 ~8k）。
- 输出：时间线式上下文块 + 检索引用列表（供最终回答标注来源）。

## 6. stale summary（P1 基础 / P2 策略化）

- 触发：消息 Edit/Delete 事件 → 反查 `summary_sources(message_id)` → `messages.current_revision != summary_sources.revision` → **同一事务**内置 `summaries.is_stale = 1` **并重置 `index_state = pending`**（R3.1.1：payload 同步同样走 durable state，消灭「MySQL 已 stale、crash 后 Qdrant 仍 is_stale=false」窗口）→ derived worker/repair 重新 upsert（payload.is_stale=true）→ `index_state = indexed`。Delete 依赖 02 §2.3 的 `current_revision += 1`。
- 策略（settings.rag.stale_policy）：`downgrade`（检索默认排除/降权 stale summary，payload `is_stale` filter）| `regenerate`（入队重生成）。P1 只实现 downgrade；P2 增 regenerate。

## 7. reindex worker（blue/green，02 §2.8 状态机）

```text
触发（WebUI / 修复） → 创建物理 collection v_{N+1}
（named vectors 双槽：dense + sparse，P1 sparse 空置）
→ 按 SoT 表逐类推进（R3.1：per-kind 游标，02 §2.8 checkpoint JSON）
   messages（pending+indexed 均重建）/ summaries / media_analyses(P2) / user_memories(P2)
   每类按主键升序分批（batch=50，embedding 限速可配），批后写 checkpoint
→ 完成 → 校验（count 与源对比）→ alias 原子切换 → 观察期（可配）
→ 删除旧版本物理 collection
失败/暂停：status=failed/paused 保留 checkpoint，可续跑
```

进度经 WebSocket `task` 帧推送 + `GET /rag/reindex/status` 查询。

## 8. Query Harness 后端（P1）

- `POST /rag/query`：表单参数 → `Retriever.Retrieve`（P1 dense 分支）→ 装配 `{score, publishedAt, kind, channel, messageRef, text}` 列表（直调检索层，不经过 conversation/AI）。
- `POST /rag/answer`（R3.1）：**前端只提交 candidate IDs/refs**，后端从 MySQL canonical 重新取内容并跑真正的 Context Builder → `AIProvider.Answer`。否则 Harness 验证的是「浏览器拼 context → LLM」而非真实 RAG 管线。返回 `AIResponse` + 所用引用列表。

## 9. 分类体系（P2）

- closed taxonomy：`settings.taxonomy` 管理固定大类（如 game_leak / official_news / discussion …），WebUI 可维护。
- open tags/entities：`Classify` 输出自由标签，写入 `ai_meta`，Qdrant payload 同步（filter 用）。
- 分类**只作用于增强**；频道/时间/kind 等确定 metadata 永不交给 AI（ADR-006）。

## 10. 讨论会话与触发（P2，conversation 包）

- conversation_key 解析：频道帖 → `getDiscussionMessage`（user-only）→ discussion top message；`reply_to_top_id` 归并同帖；无讨论群记 `orphan`（02 §2.4）。
- **记录/触发分离**（Invariant 2）：讨论群全部消息无条件入 `messages`（source_type=discussion_message）+ 会话；仅 `@Bot` / reply-Bot 触发 `Answer`。
- 触发时上下文 = 该帖近期完整对话（含未 @ 的消息，按时间序）+ **Thread Memory** + **User Memory**（R3.1 补第三层：`user_memories` SoT → `sakura_conversations.extracted_memory`，ADR-006 三级记忆完整落地）+ channel RAG（Retriever 按频道过滤）。
- **多模态回答链（R3.1 补全）**：

```text
当前用户图片 + 检索命中的 vision result
    ↓ 经 User 客户端重新获取 Telegram 原图（file_reference 刷新，03 §1.5）
    ↓ 原图 + text context → 多模态 answer 模型（Vision）
若原图已无法重新获取 → fallback：使用 media_analyses 持久化描述
```

- Bot 回复：`messages`（source_type=bot_reply，conversation_id 关联）+ Qdrant upsert（sakura_conversations）。
- 用户消息渲染在 prompt 层：`用户 {tg_user_id}/@{username} 说：{raw_text}`（存储保持原始字段，ADR-006）。
- User Memory 抽取（P2）：会话结束后周期性从对话中抽取长期记忆写入 `user_memories`（kind/confidence/溯源），走与消息相同的索引状态机。
