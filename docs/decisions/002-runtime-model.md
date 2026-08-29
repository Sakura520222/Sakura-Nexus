# ADR-002：运行模型 — 单二进制 + 单 OS 进程 + 多 goroutine

- 状态：✅ 已拍板
- 日期：2026-08-29

## 背景

旧 Sakura-Bot 为双 Bot 双进程 + MySQL 队列表每 30 秒轮询通信，是主要架构债之一。现在 Bot 已合并为 1 个（用户硬性约束），没有理由重新制造 IPC。项目约束：Linux 长期运行、低占用、单机单人运维。

## 决策

**单二进制 + 单 OS 进程 + 多 goroutine。**

```text
main
 └─ App
     ├─ TelegramSupervisor
     │   ├─ UserClient   （真实账号，抓取/监听）
     │   └─ BotClient    （唯一 Bot，发送/命令）
     ├─ ForwardingService
     ├─ SummaryService
     ├─ RAGService
     ├─ Scheduler
     └─ WebServer
```

一个 `sakura-bot` 进程内运行：User/Bot MTProto client、Telegram 事件分发、转发引擎、AI 总结 / RAG、scheduler、Web API、WebSocket 实时日志、后台清理与维护任务。

### 协作方式

- 组件通过 **Go channel、context、明确的 service interface** 协作。
- MySQL 是持久层，**不是内部组件之间的消息总线**（禁止用 MySQL 表轮询充当内部 IPC）。
- 顶层统一持有一个 `context.Context`。

### 错误处理（两层，不把 recover 当主恢复机制）

1. **正常运行时错误**：返回 `error`，局部 retry / backoff，组件自处理可恢复故障（Telegram 断线、FloodWait、LLM 5xx、MySQL 临时断连）。
2. **panic / invariant violation**：仅在明确的 goroutine boundary recover 并记录 stack trace；不无脑 recover 后假装无事发生。**核心组件不可恢复退出 → 全局优雅退出：**

```text
核心组件 fatal → cancel root context → 停止接收新任务
→ 等待在执行任务完成/超时 → 关闭 Bot/User → 关闭 HTTP Server
→ 关闭 MySQL pool → 进程退出 → systemd Restart=on-failure 兜底
```

理由：「每个 goroutine 都 recover 然后继续跑」会造成进程活着但核心 goroutine 已死的半瘫痪状态，比崩溃重启更危险。

### 部署形态约束

- **不按功能拆 Docker 容器**。未来需要 Docker 时：`sakura-bot container → /app/sakura-bot`，单应用服务。
- MySQL（与 Qdrant，见 ADR-006）为外部服务或独立容器；Sakura-Bot 自身永远只有一个应用服务。
- 不引入 MQ / Redis / 服务发现 / k8s。

## 备选与否决理由

- **多进程多服务**：故障隔离好，但进程间通信要么回 MySQL 轮询老路要么引入 MQ，占用与运维 ×N，单人单机过度设计。
- **多容器编排**：同上 + 容器开销；单容器包装本方案随时可叠加，不构成独立选项。
