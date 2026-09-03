# Sakura-Nexus 交接文档（会话恢复点）

> 更新：2026-09-03（会话 2 收工时）
> 用途：下一会话从本文件 + progress.md 尾部恢复，无需重读全部历史。

## 1. 项目总览

- **仓库**：`github.com/Sakura520222/Sakura-Nexus`（本地 `/home/firefly/Projects/Sakura-Nexus`，module path `github.com/Sakura520222/Sakura-Nexus`）
- **目标**：重写旧 Sakura-Bot + 整合 TG-Forwarder → 单二进制 Go 应用（User 抓取 / Bot 发送 / MySQL 全量数据与配置 / Linux 低占用）
- **铁律**：**不从旧项目迁移任何数据或代码**（全新实现，旧项目仅调研报告作业务参考）
- **冻结文档**：`docs/decisions/`（ADR 001–008）、`docs/design/`（overview + 01–07，R3.1.1）、`docs/plans/p0-implementation.md`（P0 计划 R1.1——**实施唯一依据，偏离需停止回报**）
- **规划文件**：`task_plan.md` / `findings.md` / `progress.md`（根目录）

## 2. 实施进度（P0 计划）

| 阶段 | 状态 |
|---|---|
| Phase 1 Telegram 风险验证 | ✅ **GATE-1 PASS + SEALED**（T1.0–T1.3） |
| Phase 2 生命周期+存储/配置 | ✅ T2.0–T2.4 全部 CI 绿 |
| Phase 3 转发引擎 | ✅ T3.1–T3.9 **U 部分全部完成**（T3.5 引擎/§6 cursor、T3.6 媒体、T3.7 AI、T3.8 底栏、T3.9 backfill）；**GATE-2 阻塞：等 .env 凭据** |
| Phase 4 | Rich 出站（T4.1 botapi → T4.2 renderer golden → T4.3 路由）——下一任务 |
| Phase 5 | 服务接线 + WebUI（T5.1–T5.4）→ GATE-3 |
| Phase 6 | 部署与验收（T6.1–T6.4）→ GATE-4 = P0 Done |

## 3. 下一步

1. **GATE-2（smoke-forward）被阻塞**：需真实 `.env`（TELEGRAM_BOT_TOKEN / TELEGRAM_API_ID / TELEGRAM_API_HASH）——本机从 Windows 迁移后 gitignored 凭据文件未带过来。**等用户提供 .env 后执行**（内容：cmd/smoke/smoke-forward 文本/单媒体/相册三 case + 相册全成员 dedup 查库验证）。T3.4/T3.6 的 S 冒烟同批补跑。
2. GATE-2 不可先行（硬门禁）→ 若用户暂不提供 .env，可先做 **T4.1（platform/botapi，fake server 单测）**——但注意计划依赖：T4.1 依赖 T3.4 ✅，与 GATE-2 无前置关系（Phase 4 在 GATE-2 之后才应开始——**严格说须先 GATE-2**，若用户同意可例外先行 T4.1 的 U 部分，需回报确认）。
3. 引擎接线（T5.1）时需要补的适配点已在代码注释标明：FailureClassifier（gotd tgerr→permanent 映射）、Rewriter（ai.Provider 适配 rule.AIPrompt）、AssistantBot（Bot username）、settings 订阅→ApplySettings、规则 CRUD→RefreshRules。

## 4. 环境与工作流（Linux 开发机）

- **PATH**：golangci-lint 2.13.2 在 `~/go/bin`（本机 go1.27 + CGO=1，本地可跑 `-race`）
- **GOPROXY** 已设 goproxy.cn（`go env -w`，默认代理被墙；可 `go env -u GOPROXY` 还原）
- **集成库**：本机 docker `mysql:8.4` 容器（容器名 mysql，127.0.0.1:3306）；已建 `test_db` + `test_user`/`test_user_pw`，凭据在 `.env.test.local`（gitignored）。集成测试：`export $(grep -v '^#' .env.test.local | xargs) && go test -race -tags integration ./...`
- **生产库 sakura_bot 与 .env**：本机缺失（见 §3.1 阻塞项）
- **测试门禁**：`gofmt -l .` 空 → `golangci-lint run` 0 issues → **`go test -race ./...` 全量**（教训：只跑单包把红测试推上 CI 过）→ 集成同上
- **提交纪律**：每任务 1–3 commit、Conventional Commits、每 commit 可编译测试绿；**验证绿后自行 `git push origin main`**（用户持久授权）；红 CI 先修再继续（gh CLI 查终态）
- **GATE 硬门禁**：GATE-1/2/3/4 未过不得进入后续任务（T3.5 之前无需再停，计划已批）
- **CodeGraph**：本机已建索引（.codegraph/，gitignored）——查代码优先 codegraph_explore

## 5. 本会话尾部状态

- 提交链（全部 CI 绿）：d85ee3d+14a8895（T3.5）/ 7e2568a（T3.6）/ 27f5c4d（T3.7）/ b3a97e3（T3.8）/ ee721b1（T3.9，CI success）
- 工作区：progress.md 已记 T3.6–T3.9 详情（含两处设计偏差备案：§1.5 fileref 不写回、默认底栏文案 {source_link}——详见 progress.md）
- 记忆已持久化（本机 ~/.claude/projects/-home-firefly-Projects-Sakura-Nexus/memory/）：push-self-authorized / design-approval-required / dev-env-linux
- 引擎测试基线：forwarding 包 45+ 测试全绿（-race），fake 全链（含 §6 cursor 恢复用例）
