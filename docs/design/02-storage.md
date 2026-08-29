# 02 存储

- 状态：⏳ 待成文
- 受约束 ADR：[006](../decisions/006-rag-architecture.md) · [007](../decisions/007-scope-phases.md)

## 覆盖内容

- MySQL schema v2（goose 版本化迁移，全部表）：gotd sessions、settings 配置中心、channels、channel_settings、messages + revisions（canonical + 事件模型）、conversations、forward_rules / forwarded_messages / forwarding_stats、summaries（含 source_revision_hash / is_stale）、subscriptions、users / usage_quota、submissions、poll_regenerations / poll_voters、system_audit_logs；旧 v1 表映射与迁移说明
- Qdrant 设计：`sakura_knowledge` / `sakura_conversations` 双 collection 的 payload schema、point ID 规则、alias 命名（blue/green 版本化）、reindex worker 状态存储与 API
- 数据生命周期：forwarded_messages 保留期、消息向量保留策略、备份边界（MySQL 备份 + Qdrant 重建）
