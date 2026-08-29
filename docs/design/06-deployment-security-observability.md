# 06 部署、安全与可观测性

- 状态：⏳ 待成文
- 受约束 ADR：[002](../decisions/002-runtime-model.md) · [005](../decisions/005-go-libraries.md) · [006](../decisions/006-rag-architecture.md)

## 覆盖内容

- 部署：systemd unit（Restart=on-failure）、Dockerfile（多阶段：前端构建 → Go 构建 → 运行，非 root）、docker-compose（sakura-bot + mysql + qdrant）、裸机部署（Qdrant 二进制 + systemd）
- 升级流程（goose 启动即迁移、二进制替换、回滚）
- 备份恢复：MySQL 备份策略 + Qdrant 重建（reindex worker 恢复索引）
- 可观测性：slog 约定（组件 logger、JSON、级别）、WebUI 日志流（环形缓冲 + WebSocket）、状态自检、审计（system_audit_logs）
- 安全：凭据管理（.env 权限 600）、WebUI 登录防护（失败锁定）、Qdrant API key、MySQL 最小权限账户、Bot token 复用边界（ADR-008）
