# ADR-007：范围分级 — P0/P1/P2 切分（方案一修正版）

- 状态：✅ 已拍板
- 日期：2026-08-29
- 前置：目标架构已冻结于 ADR-001~006，本决策只切实施顺序，**不改目标架构**。

## 背景

目标架构已相当完整。若第一期同时实现 hybrid/RRF、vision、自动分类、三级记忆、summary invalidation、完整转发、WebUI、投稿、调度，第一个可运行版本会被拉得过大、故障定位困难。

## 决策

> **P0 = A + B + C；P1 = D + F + 最小 RAG 查询闭环；P2 = E + G**

| 阶段 | 范围 | 目标 |
|---|---|---|
| **P0** | A + B + C | Telegram / 转发 / WebUI 的 **production vertical slice** |
| **P1** | D + F + RAG Query Harness | 总结完整恢复 + 可真正查询的 Dense RAG |
| **P2** | E + G | 完整互动体系 + Hybrid / Multimodal / Agentic RAG |

```text
P0  Telegram Infrastructure + Forwarding + Management UI
        ↓
P1  Summary Pipeline + Knowledge Ingestion + Dense Retrieval
        ↓
P2  Discussion Conversations + Hybrid Retrieval + Memory + Multimodal + Agentic AI
```

### P0：第一个 production-capable vertical slice

专注消灭本次重写最大的两个工程风险：**gotd 双客户端是否稳定**、**Go 单二进制部署模型是否跑得住**。验收是真正长期可运行的系统，而非「骨架能启动」：

```text
User gotd → 真实频道 NewMessage / Album → 规则/filter/dedup →（必要时 AI rewrite）→ Bot gotd → 目标频道
```

同时完成：MySQL session 持久化、断线重连、FloodWait/retry/backoff、相册可靠聚合、WebUI 规则 CRUD、实时日志、systemd、Docker Compose、graceful shutdown、health endpoint。

**P0 不要求能替代完整旧 Sakura-Bot。**

### P1：D + F，F 必须有「查询出口」

**D 总结体系（完整）**：scheduler、增量抓取、水位、手动/自动总结、LLM summary、Bot 发送、summary 入 MySQL、summary 进入 RAG、订阅推送。

**F RAG 基础（第一版只做 Dense）**：

```text
Canonical message → Revision → Dense Embedding → Qdrant
→ Metadata Filter → Top-K → 按时间重新排序 → Context Builder
```

暂不做：BM25、RRF、reranker、Vision、User Memory、Thread Memory、Query Analyzer、Agent tool calling。

**必须附带 RAG Query Harness**（WebUI 内部调试页，开发/管理能力，不是用户聊天系统）：输入问题/频道/时间范围/TopK → 展示 `Score | 时间 | 类型 | 消息` 结果表 → 可选「使用这些 Context 调一次 AI」。没有真实查询出口，F 无法验收——它验证 `Telegram → MySQL → embedding API → Qdrant → retrieval → Context Builder → LLM` 全链条。

### P2：E + G（讨论会话与互动体系统一实现）

E（评论区欢迎、投票、投票重生成、投稿、投稿审核）与 G（BM25 sparse、Dense+Sparse hybrid、RRF、Query Analyzer、LLM reranker、Vision、stale summary、Thread/User Memory、每帖 conversation、全部讨论消息记录、`@Bot`/reply Bot 触发、多模态 conversation、Agentic RAG、用户命令、quota）放同一期：重新定义后的讨论群是「Channel Post → Discussion Thread → 完整会话 + Thread Memory」体系，与互动体系一起实现，**避免 P1 先写一套旧式 discussion handler、P2 为 AI conversation 再重构一次**。

## 强制约束：分期只能裁剪功能，不得裁剪目标架构

接口边界从第一天就存在，业务层不得直接调用具体实现（如 `qdrant.Search(...)`）：

```go
type Retriever interface {
    Retrieve(ctx context.Context, query RetrievalQuery) ([]Candidate, error)
}
```

同理 `Reranker`、`VisionProcessor`、`QueryAnalyzer`、`MemoryStore` 在目标架构中提前留边界；P1 允许 `nil / no-op / basic` 实现，P2 填完整实现。否则「分期」会演变为 P2 大重构。
