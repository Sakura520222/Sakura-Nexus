# ADR-004：前端技术栈 — Vue 3 + TypeScript + Vite + Naive UI

- 状态：✅ 已拍板
- 日期：2026-08-29

## 背景

2026-08 生态核查（用户核实）：Vue 3 稳定线 3.5.41（2026-08-05 发布，3.6 处于 RC）；Vite 8.x（8.2.2，2026-08-20）；Naive UI 2.45.3（**2026-08-27**，维护非常活跃，90+ 组件、tree-shaking、TS、主题覆盖、数据组件 virtual list）；Vue Router 5.x；React 19.2.8 与 Svelte 5.56 生态同样健康但无迁移收益。

## 决策

> **前端技术栈：Vue 3 + TypeScript + Vite + Naive UI**

- SPA 使用 Vue 3 Composition API；全部业务代码 TypeScript；Vite 作开发/构建工具；Naive UI 为主组件库；Vue Router 负责 SPA 路由。
- **不引入 Nuxt / SSR / Node 服务端**（已有 Go HTTP Server，Nuxt 会重新引入 Node 服务端，无收益）。
- 构建产物经 `go:embed` 嵌入（ADR-003）；运行时无 Node.js。
- 继承旧 Sakura-Bot 的**信息架构与交互经验**（Dashboard / Channels / Schedules / Forwarding / Stats / System 等页面结构），不直接搬旧 Vue 实现。
- Naive UI 为默认 UI 基础，**不混用** Element Plus 等第二套大型组件库（两者都健康，无迁移收益）。
- 实施时取当时最新**稳定版**，不追 RC/beta（不上 Vue 3.6 RC）。

### 架构约束：避免 UI 库泄漏到业务层

```text
API DTO → frontend model → Vue composable/store → Naive UI component
```

页面不得把 Naive UI 对象当业务模型传递（如围着 `NDataTable` 类型设计数据层）。未来即使更换 UI 库，业务层不被绑死。

## 备选与否决理由

- **React 19**：CRUD/表单/路由/WebSocket/图表能力 ≈ Vue，但丢失旧 UI 信息架构连续性、Naive UI 连续性、Vue 开发经验，无迁移理由。
- **Svelte 5**：产物小、代码省，适合小工具/小型仪表盘；本项目是长期增长的重管理后台，成熟组件库的现成能力比框架代码量更重要。
