# Sakura-Nexus 交接文档（会话恢复点）

> 更新：2026-09-05（会话 3 收工时——GATE-2 PASS）
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
| Phase 3 转发引擎 | ✅ T3.1–T3.9 + **GATE-2 PASS**（smoke-forward 三 case 端到端 + 相册全成员 dedup 查库，2026-09-05）；S 冒烟实证修复 6 个真实缺陷（random_id/tmpRoot/扩展名/相册 uploadMedia 注册/file_reference——详见 progress.md 会话 3） |
| Phase 4 Rich 出站 | **下一任务**：T4.1（platform/botapi，fake server 单测）→ T4.2（normalizer/validator/splitter golden）→ T4.3（路由+lazy capability + Rich smoke checkpoint 非 Gate） |
| Phase 5 | 服务接线 + WebUI（T5.1–T5.4）→ GATE-3 |
| Phase 6 | 部署与验收（T6.1–T6.4）→ GATE-4 = P0 Done |

## 3. 下一步

1. **T4.1 platform/botapi**（依赖 T3.4 ✅；GATE-2 已过，无门禁阻塞）：net/http 客户端、PeerKind 三态编码、429/Retry-After、5xx 退避、token 脱敏日志；U=fake server 单测。
2. 引擎接线（T5.1）备忘（代码注释 + 会话记录）：FailureClassifier 完整 tgerr→permanent 映射（冒烟侧已有最小版）、Rewriter（ai.Provider 适配 rule.AIPrompt）、AssistantBot（Bot username）、settings 订阅→ApplySettings、规则 CRUD→RefreshRules、**Bot 侧 peer 查询表**（冒烟用静态表 + getChannels 验证可行）、相册已生产化走 uploadMedia 注册路径（551ac00）。
3. 已知运行事实（GATE-2 实证，写代码时参考）：Bot 账号发送必须带非零 random_id；photo 再上传文件名必须带照片扩展；sendMultiMedia 成员须先 messages.uploadMedia 注册且引用带 file_reference；小尺寸纯色图相册必拒（MEDIA_INVALID）。

## 4. 环境与工作流（Linux 开发机）

- **生产 .env 已就位**（本机 docker mysql：库 `Sakura-Nexus` utf8mb4、用户 `sakura_nexus` 全权；TELEGRAM_BOT_TOKEN/API_ID/HASH、USERBOT_PHONE_NUMBER 齐备）。userbot 已登录：**@Let_MoonLet** id=6826794184（会话 3 中途换号；session 在 gotd_sessions(user)，重登走 `go run ./cmd/sakura-nexus login-user`）；bot session（gotd_sessions(bot)，@sakura_bot_test_bot）。
- **PATH**：golangci-lint 2.13.2 在 `~/go/bin`（本机 go1.27 + CGO=1，本地可跑 `-race`）
- **GOPROXY** 已设 goproxy.cn（`go env -w`；可 `go env -u GOPROXY` 还原）
- **集成库**：`test_db` + `test_user`/`test_user_pw`（`.env.test.local`，gitignored）。集成测试：`export $(grep -v '^#' .env.test.local | xargs) && go test -race -tags integration ./...`
- **S 冒烟**：`go run ./cmd/smoke/smoke-forward`（自建临时频道全自动，结束清理；`-keep` 保留现场）；smoke-bot/smoke-user/smoke-recovery 备用复验
- **测试门禁**：`gofmt -l .` 空 → `golangci-lint run` 0 issues → **`go test -race ./...` 全量**（教训：只跑单包把红测试推上 CI 过）→ 集成同上
- **提交纪律**：每任务 1–3 commit、Conventional Commits、每 commit 可编译测试绿；**验证绿后自行 `git push origin main`**（用户持久授权）；红 CI 先修再继续（gh CLI 查终态）
- **GATE 硬门禁**：GATE-1 ✅ / GATE-2 ✅ / GATE-3、GATE-4 未到；Rich smoke checkpoint（T4.3 后）非 Gate
- **CodeGraph**：本机已建索引（.codegraph/，gitignored）——查代码优先 codegraph_explore

## 5. 本会话尾部状态

- 提交链（GATE-2 相关，CI 全绿）：1e04c83（smoke-forward 交付）→ fb1e187（缺陷①②③）→ 4fde68e（缺陷④⑤前半）→ 551ac00（相册根因·uploadMedia 注册）→ 76ce5f8（file_reference）→ 本 docs commit
- GATE-2 冒烟记录：progress.md 会话 3（源/目标临时频道已删除清理；forwarding_stats 3/0；相册 grouped_id 组内成员 3 全员 dedup 落库）
- 记忆已持久化：push-self-authorized / design-approval-required / dev-env-linux
- 引擎测试基线：forwarding 包全绿（-race），新增 tmpRoot/扩展名回归测试
