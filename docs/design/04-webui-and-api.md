# 04 WebUI 与 API

- 状态：⏳ 待成文
- 受约束 ADR：[003](../decisions/003-webui-form.md) · [004](../decisions/004-frontend-stack.md)

## 覆盖内容

- 页面清单与路由（P0：登录 / 仪表盘 / 转发规则 / 频道 / 系统 / 日志流；P1：+ 调度 / 总结 / RAG Query Harness；P2：+ 互动 / 投稿 / 会话）
- REST API 清单（`/api/*`）与 DTO 约定（前端不成为配置真相源：SPA / TG 命令 / Scheduler 全走 Go service 层）
- JWT 鉴权细节（.env 用户名/密码 → JWT；登录失败防护）
- WebSocket 协议（实时日志 / 连接状态 / 任务 / 系统事件；仅实时流，CRUD 走 REST）
- RAG Query Harness 页面规格（P1）：问题 / 频道 / 时间 / TopK → `Score|时间|类型|消息` 结果表 → 「使用这些 Context 调一次 AI」
- 前端架构：DTO → frontend model → composable/store → Naive UI 组件（UI 库不泄漏业务层）
