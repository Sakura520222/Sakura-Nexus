# 架构决策记录（ADR）索引

Sakura-Bot v2 重写的已拍板决策。每项一个文件，自包含（背景 / 决策 / 理由 / 备选与否决原因）。修改已拍板决策须修订对应文件并在状态中注明，不得静默变更。

| # | 主题 | 文件 | 状态 | 拍板日期 |
|---|---|---|---|---|
| 001 | 技术栈与 Telegram 库（Go + gotd/td） | [001-telegram-stack.md](001-telegram-stack.md) | ✅ 已拍板 | 2026-08-29 |
| 002 | 运行模型（单二进制 + 单进程 + 多 goroutine） | [002-runtime-model.md](002-runtime-model.md) | ✅ 已拍板 | 2026-08-29 |
| 003 | WebUI 形态（SPA + go:embed） | [003-webui-form.md](003-webui-form.md) | ✅ 已拍板 | 2026-08-29 |
| 004 | 前端技术栈（Vue 3 + TS + Vite + Naive UI） | [004-frontend-stack.md](004-frontend-stack.md) | ✅ 已拍板 | 2026-08-29 |
| 005 | Go 基础库选型包 | [005-go-libraries.md](005-go-libraries.md) | ✅ 已拍板 | 2026-08-29 |
| 006 | RAG / AI 架构（MySQL SoT + Qdrant） | [006-rag-architecture.md](006-rag-architecture.md) | ✅ 已拍板（含措辞澄清修订） | 2026-08-29 |
| 007 | 范围分级 P0/P1/P2（方案一修正版） | [007-scope-phases.md](007-scope-phases.md) | ✅ 已拍板 | 2026-08-29 |

**六项目标架构决策（001–006）已冻结**，后续只做范围切分，不再改变目标架构。006 内含三条 Architecture Invariants（MySQL=SoT / Qdrant=派生索引；记录≠触发；相关度检索→重排→选择→时间序组装），绕过即架构违规。

## 待定事项

- 总体设计文档（架构图 / MySQL schema / .env v2 / 部署 / 接口边界）——**结构待用户审阅**，然后成文
- Go 版实施计划——总体设计批准后编写，计划确认后才动代码
- 旧 Python 版设计草稿与 P0 计划已于 2026-08-29 废弃删除（技术栈变更，git 历史可查）
