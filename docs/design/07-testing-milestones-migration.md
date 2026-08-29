# 07 测试、里程碑与迁移

- 状态：⏳ 待成文
- 受约束 ADR：[007](../decisions/007-scope-phases.md)

## 覆盖内容

- 测试策略：单元（过滤链 / 渲染器 / 配置 / 纯函数）、集成（MySQL/Qdrant 容器）、gotd 的测试策略（接口注入 fake）、RAG Query Harness 作为验收工具
- CI：lint（gofmt/staticcheck）→ 单测 → 集成 → 前端 build（vue-tsc）→ 镜像构建
- 里程碑对照：P0/P1/P2 ↔ 设计章节映射表，每阶段验收标准（P0：真实频道转发跑通 + WebUI 管理 + systemd/compose 部署；P1：总结恢复 + Dense RAG 全链路可查询；P2：完整互动 + Hybrid/Multimodal/Agentic RAG）
- 附录：术语表、旧项目（Sakura-Bot v1.8.9 / TG-Forwarder）→ v2 功能迁移对照表、ADR 索引
