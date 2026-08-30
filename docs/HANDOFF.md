# Sakura-Nexus 交接文档（会话恢复点）

> 更新：2026-08-29（本会话收工时）
> 用途：下一会话从本文件 + progress.md 尾部恢复，无需重读全部历史。

## 1. 项目总览

- **仓库**：`github.com/Sakura520222/Sakura-Nexus`（本地 `d:\Project\Sakura-Bot`，module path `github.com/Sakura520222/Sakura-Nexus`）
- **目标**：重写旧 Sakura-Bot + 整合 TG-Forwarder → 单二进制 Go 应用（User 抓取 / Bot 发送 / MySQL 全量数据与配置 / Linux 低占用）
- **铁律**：**不从旧项目迁移任何数据或代码**（全新实现，旧项目仅调研报告作业务参考）
- **冻结文档**：`docs/decisions/`（ADR 001–008）、`docs/design/`（overview + 01–07，R3.1.1）、`docs/plans/p0-implementation.md`（P0 计划 R1.1——**实施唯一依据，偏离需停止回报**）
- **规划文件**：`task_plan.md` / `findings.md` / `progress.md`（根目录）

## 2. 实施进度（P0 计划）

| 阶段 | 状态 |
|---|---|
| Phase 1 Telegram 风险验证 | ✅ **GATE-1 PASS + SEALED**（T1.0–T1.3；16 实时 + 346 重启补齐冒烟实证） |
| Phase 2 生命周期+存储/配置 | ✅ T2.0 lifecycle / T2.1 0002 迁移 / T2.2 Database / T2.3 settings 中心 / T2.4 repositories（全部 CI 绿） |
| Phase 3 转发引擎 | 🔄 T3.1 outbound domain ✅ / T3.2 过滤链 ✅ / T3.3 AlbumAggregator ✅ / **T3.4 Outbound MTProto ✅（CI 已绿）** |
| Phase 3 剩余 | **T3.5 engine 编排**（下一任务）→ T3.6 媒体临时文件 → T3.7 AI rewrite → T3.8 底栏 → T3.9 backfill → **GATE-2** |
| Phase 4 | Rich 出站（T4.1 botapi → T4.2 renderer golden → T4.3 路由） |
| Phase 5 | 服务接线 + WebUI（T5.1–T5.4）→ GATE-3 |
| Phase 6 | 部署与验收（T6.1–T6.4）→ GATE-4 = P0 Done |

## 3. 下一步（T3.5 要点）

`internal/forwarding/engine.go`：事件入口（挂 User updates dispatcher）→ 相册聚合分支（AlbumAggregator）或单条 → 规则匹配（MatchSource）→ 过滤链（ShouldForward）→ 去重查（ForwardedRepo.Exists，五列 ChatRef 键）→ 发送队列（容量 100 阻塞背压、单消费者、随机延迟、FloodWait 矩阵）→ **contiguous cursor 语义**（P0 Plan §6：transient failure 不得越过推进；terminal=filtered/dedup/success）→ 真实成败统计（IncrStats）。前置依赖已齐：T2.4 repos + T3.1-T3.4 + config.SettingsCenter（转发参数）。

## 4. 环境与工作流（重要）

- **PATH**（bash 会话每次需补）：`export PATH="/c/Program Files/Go/bin:/c/Users/Firefly/go/bin:$PATH"`（golangci-lint 2.13.2 以 go1.27 编译；本机 GOTOOLCHAIN=auto 已是 1.27）
- **测试门禁**：`gofmt -l .` 空 → `golangci-lint run` 0 issues → **`go test -count=1 ./...` 全量**（教训：T3.2 因只跑单包把红测试推上 CI）→ 集成 `export $(grep -v '^#' .env.test.local | xargs) && go test -tags integration ./...`
- **本机无 cgo**：本地 `go test` 不带 `-race`（`make test-local`）；`-race` 由 CI（锁 go1.26）执行
- **集成库**：本机 MySQL 8.0.45 的 `test_db`（test_user/密码在 `.env.test.local`，gitignored）；生产库 `sakura_bot`（凭据在 `.env`，gitignored）
- **提交纪律**：每任务 1–3 commit、Conventional Commits、每个 commit 可编译测试绿；**提交验证绿后自行 `git push origin main`**（用户持久授权）；红 CI 先修再继续（gh CLI：`gh run list --branch main --limit 5 --json databaseId,headSha,status,conclusion --jq ...` 查终态）
- **GATE 硬门禁**：GATE-1/2/3/4 未过不得进入后续任务（T3.5 之前无需再停，计划已批）
- **环境凭据**（`.env`）：TELEGRAM_BOT_TOKEN（测试 bot @sakura_bot_test_bot）/ TELEGRAM_API_ID / TELEGRAM_API_HASH；User 真实账号已登录（@CherrySakura321，session 存 gotd_sessions 表）；login-user 重登需用户终端操作

## 5. 本会话尾部状态

- 提交至 `634416b`（迁移竞态修复，CI 监控中）；T3.4 `36da5fa` CI 已确认 success；T3.3 `9eae8d8` success
- 工作区干净（无未提交改动）
- `8179d5f` 红已定位并修复：**迁移跨包并发竞态**（mysql 包 TestMigrateFullCycle 的 DownTo 与 config 包 MigrateUp 并行踩版本表 → missing zero version migration；同代码两次 run 一绿一红证实是时序竞态，非 transient）。修复：MigrateUp/DownTo 经 MySQL 命名锁（GET_LOCK 'sakura_migration_lock'）全局串行 + TestMigrateFullCycle 改用独立临时库（CI root 走完整 Down→Up 验证；本地 test_user 无 CREATE 权限自动退化幂等验证）。下一会话开跑先确认 634416b CI 绿
- 记忆已持久化：`design-approval-required` / `push-self-authorized` / `sakura-bot-rewrite-goal`（C:\Users\Firefly\.claude\projects\d--Project-Sakura-Bot\memory\）
