# 调研发现

## 已确认（2026-08-29）

- 当前仓库 `d:\Project\Sakura-Bot` 除 `.git` 外为空，是全新起点。
- `E:\项目\Sakura-Bot`：Python 项目。顶层有 `main.py`、`core/`、`web/`、`data/`、`tests/`、`qa_bot.py`、`query/`、`.sakura/`、Docker 全套（Dockerfile / docker-compose.yml / docker-entrypoint.sh / start.bat / start.sh）、`TG-Forwarder/` 内嵌子目录、`wiki/`。
- `E:\项目\TG-Forwarder`：独立 Python 项目。顶层有 `run.py`、`src/`、`config/`、`data/`、Docker 支持、`备份/` 目录。
- 疑点已解：Sakura-Bot 内嵌 `TG-Forwarder/` 子目录是未跟踪的残留拷贝（.gitignore 忽略、零 import），与独立仓库无代码关系，重写时忽略。

## 子代理调研报告

（待两个 Explore 子代理返回后填入/引用）

### Sakura-Bot 源项目 — 已完成 ✅（全文见 [docs/research/sakura-bot-report.md](docs/research/sakura-bot-report.md)）

要点：
- **双进程**：主进程（Telethon 主 Bot + UserBot + APScheduler + FastAPI/uvicorn + ChromaDB 共存一个事件循环）+ QA Bot 子进程（python-telegram-bot）。
- **两个 Bot**：主 Bot（`TELEGRAM_BOT_TOKEN`，Telethon，管理命令 + 全部发送）；QA Bot（`QA_BOT_TOKEN`，PTB，用户侧 RAG 问答/订阅推送/投稿）。跨 Bot 通信 = MySQL 三张队列表 30s 轮询。**收敛为单 Bot 后：删除 PTB 依赖、子进程管理、全部队列轮询，QA 用户功能移植为同一 Bot 的用户命令。**
- 分工现状已符合目标方向（UserBot 抓取、Bot 发送），但存在「UserBot 未启用时降级为 Bot 监听/抓取」路径，需按新约束收紧为仅真实账号抓取。
- MySQL 已在用（aiomysql，15 表，db_version=6）；**与目标冲突的三处**：① 配置主体在 `data/config.json`（watchdog 热重载）→ 迁 MySQL 配置表；② ChromaDB 向量落盘 `data/vectors/` → 需替换为 MySQL 存储；③ Telethon session 是文件 → 需自定义 MySQLSession 或 StringSession 入库。
- WebUI：Vue 3 + naive-ui SPA + FastAPI（14 router），鉴权现为 token/Telegram Widget，**无用户名密码体系** → 改为 .env 用户名/密码 + JWT。
- 内嵌 `TG-Forwarder/` 子目录是残留拷贝，主项目零引用，**重写时忽略**。
- 架构债（来自 .sakura/SAKURA.md）：配置双通道、JSON 无锁直写、全局单例无依赖注入（测试困难）、延迟导入滥用——新架构针对性解决（单一配置源 MySQL、显式依赖注入容器）。
- 技术栈：Python 3.11+（推荐 3.13）、Telethon、FastAPI、APScheduler、aiomysql、pydantic、Vue3/Vite。

## 设计决策记录

（设计文档撰写中，见 docs/superpowers/specs/）

### TG-Forwarder 源项目 — 已完成 ✅（全文见 [docs/research/tg-forwarder-report.md](docs/research/tg-forwarder-report.md)）

要点：
- Telethon 双客户端：**User 真实账号监听源频道，Bot 账号发送**——与目标架构一致，架构可直接沿用。
- 转发一律「复制式重发」（从不 `forward_messages`）：媒体走「User 下载（BytesIO）→ Bot 重上传」；规则模型为扁平 `source→target + keywords + ai_prompt + custom_footer` 多对多列表。
- 已有：原创过滤、关键词子串过滤、`(source_msg_id, target)` 去重、固定延迟+重试、相册 grouped_id 聚合（固定 1s 窗口有丢消息竞态）、AI 改写（OpenAI 兼容）、底栏模板（占位符替换）、自动加群、启动/停止 TG 通知、统计与定期清理。
- WebUI：手写 aiohttp + 原生 JS（登录/仪表盘/规则 CRUD/配置编辑/环形缓冲日志 + WebSocket 实时推送）。
- 数据：SQLite（**同步阻塞事件循环**）；配置全在 YAML 文件（config.yaml + channels.yaml 回写式）。
- 明确缺失：编辑/删除同步、catch-up 水位、媒体类型过滤、消息分段、FloodWait 显式处理、随机抖动。
- 已知坑：转发失败仍计成功数；明文凭据曾被 git 跟踪；webui 端口与 compose 映射漂移。
- 取舍：规则模型/复制式转发/三态发送策略/去重键/日志环形缓冲保留；YAML 配置、Bot 私聊管理命令、交互式登录、User 发送分支全部替换或删除。

## 设计决策记录

（待补充）
