# 05 AI 与 RAG

- 状态：⏳ 待成文
- 受约束 ADR：[006](../decisions/006-rag-architecture.md) · [007](../decisions/007-scope-phases.md) · [008](../decisions/008-rich-message-transport.md)

## 覆盖内容

- AI Provider（openai-go + WithBaseURL）：能力矩阵（Embedding / Chat+Agent / Vision / Classification / Summary / Query Analyzer / LLM Reranker，全部 OpenAI-compatible）、配置模型、降级策略、热重建
- **AI 输出契约（用户指定补充）**：

```text
AIProvider → AIResponse
             ├─ text/content
             ├─ optional media references
             └─ metadata
             ↓
Telegram Presentation Layer → Rich Markdown（渲染发生在 Telegram 层，AIResponse 无 Telegram 特定类型）
```

- AI 输出格式策略：LLM formatting instruction ≠ protocol validation；首选 Rich Markdown 输出 + deterministic renderer/validator；第一版仅 `rich_message.markdown` 一种表达
- RAG 管线细化：ingest 事件模型（canonical → revision → classify → embed → upsert；delete 退出检索）、六阶段检索（P1 dense 子集 / P2 完整）、stale summary、三级记忆（P2）、reindex worker（blue/green + alias）
- 分类体系：closed taxonomy（WebUI 可管理）+ open tags/entities
