# 架构决策记录（ADR）索引

Sakura-Bot v2 重写的已拍板决策。每项一个文件，自包含（背景 / 决策 / 理由 / 备选与否决原因）。修改已拍板决策须修订对应文件并在状态中注明，不得静默变更。

| # | 主题 | 文件 | 状态 | 拍板日期 |
|---|---|---|---|---|
| 001 | 技术栈与 Telegram 库（Go + gotd/td） | [001-telegram-stack.md](001-telegram-stack.md) | ✅ 已拍板 | 2026-08-29 |
| 002 | 运行模型（单二进制 + 单进程 + 多 goroutine） | [002-runtime-model.md](002-runtime-model.md) | ✅ 已拍板 | 2026-08-29 |
| 003 | WebUI 形态（SPA + go:embed） | [003-webui-form.md](003-webui-form.md) | ✅ 已拍板 | 2026-08-29 |
| 004 | 前端技术栈（Vue 3 + TS + Vite + Naive UI） | [004-frontend-stack.md](004-frontend-stack.md) | ✅ 已拍板 | 2026-08-29 |
| 005 | Go 基础库选型包 | [005-go-libraries.md](005-go-libraries.md) | ✅ 已拍板 | 2026-08-29 |
| 006 | RAG / AI 架构（MySQL SoT + Qdrant） | [006-rag-architecture.md](006-rag-architecture.md) | ✅ 已拍板 | 2026-08-29 |

## 待定事项

- 范围分级 / P0 切分（第七问，待用户拍板）
- 总体设计文档（架构图 / MySQL schema / .env v2 清单 / 部署）——依赖范围分级完成后编写
- 旧 Python 版设计草稿与 P0 计划已于 2026-08-29 废弃删除（技术栈变更，git 历史可查）
