# 07 测试、里程碑与参考

- 状态：📝 R3.1 修订版，待快速一致性复核
- 受约束 ADR：[007](../decisions/007-scope-phases.md)
- 更名说明（R3.1）：原「…-migration」更名以免误解——本项目**完全不迁移旧数据**（02 §5）；§4.2 是功能对照参考。

## 1. 测试策略

### 1.1 单元测试（不依赖网络/容器，表驱动为主）

| 对象 | 覆盖点 |
|---|---|
| 过滤链（forwarding/filters） | keywords/patterns/黑名单/媒体类型/原创限定 全正反例；坏正则行为；**相册聚合过滤（聚合文本 + 媒体类型并集，03 §1.6）** |
| Rich renderer（platform/telegram，R3.1 路径修正） | normalizer + validator + block 切分：**golden 样例**（输入 markdown → 期望 block 序列/切分结果），含边界：超限表格、超长代码块、16 层嵌套、50 媒体、32768 字符 |
| 相册聚合（forwarding/engine） | 假时钟：quiet 400ms 重置、hard deadline 2s、集满 10 提前 flush、窗口后迟到消息独立处理、全成员 dedup 写入 |
| Availability 状态机（platform） | 连接→断线→重连循环的状态翻转与订阅通知（01 §1.3 接口） |
| 配置中心 | scope 校验拒绝非法值、热更新回调 |
| 底栏模板 | 占位符全量、私有频道链接 |
| 会话鉴权（webapi） | Cookie 会话、失败锁定、Origin 校验（04 §4–5） |
| 索引状态机（rag） | pending/indexed/delete_pending/excluded/error 状态转移；repair 扫描与收敛（05 §4） |
| DTO/权限 | WebAPI handler（httptest + fake service）鉴权豁免表 |

### 1.2 集成测试（R3.1：**必跑**，不依赖任何 Telegram 凭据）

- **P0：MySQL 集成必跑**（testcontainers-go 或 CI service 容器）。
- **P1 起：MySQL + Qdrant 集成必跑**。
- 覆盖：goose 迁移幂等；repositories CRUD 与事务语义；update state / peer storage / peer aliases 往返（gotd storage 接口实现）；P1：Qdrant upsert/search/alias 切换/reindex checkpoint（per-kind）续跑；索引状态机 + repair 的崩溃恢复模拟（kill -9 后重启收敛）。
- 真实 Telegram：仍仅手动 smoke（`examples/smoke/`），不进 CI。

### 1.3 gotd 测试策略

- 领域只依赖消费者接口（01 §2.3），测试注入 fake Fetcher/Sender。
- 真实 Telegram 冒烟：登录、收一条消息、发一条 Rich（验证 lazy capability detection），手动执行。

## 2. CI（GitHub Actions）

```sh
# 1. lint
test -z "$(gofmt -l .)"        # R3.1：gofmt 无 --check，用 -l 判空
golangci-lint run              # depguard 强制依赖方向（01 §2.2）
pnpm --dir web exec vue-tsc --noEmit

# 2. test
go test -race ./...            # R3.1：加 -race
go test -race -tags integration ./...   # MySQL（P0 起）+ Qdrant（P1 起）容器

# 3. build
go build ./...
pnpm --dir web build
docker build -t sakura-bot .

# 4. 校验与安全
docker compose -f compose.yaml config -q
docker compose -f compose.yaml -f compose.full.yaml config -q   # R3.1：两个组合都验
govulncheck ./...
pnpm --dir web audit
```

## 3. 里程碑验收标准（与 ADR-007 对齐）

### P0（production vertical slice）

- [ ] 真实频道转发全链路跑通：文本 / 媒体 / 相册 / 带编辑历史源不受影响
- [ ] 规则 CRUD、启停、回溯补发经 WebUI 完成
- [ ] MySQL session/update state/peer（含 aliases）持久化：重启不重登、catch-up 正常
- [ ] **强制构造 gap-too-long（ChannelDifferenceTooLong）→ targeted history recovery 不漏消息**（R3.1 新增）
- [ ] **相册转发后全部成员均完成 dedup/mapping（逐成员可查 forwarded_messages）**（R3.1 新增）
- [ ] 断线重连（User/Bot）自动恢复，依赖服务 DEPENDENCY_UNAVAILABLE 转换正确
- [ ] FloodWait/429 服从（超限 = failed + 可补发，无静默丢失）
- [ ] graceful shutdown（SIGTERM → drain → 落库 → exit 0）；WebUI 重启 → exit 75 → 被拉起
- [ ] systemd 与 docker compose（双文件）部署可用；`healthcheck` 子命令通过
- [ ] 24h 稳定性观察：内存平稳（06 §6 预算）、无 goroutine 泄漏、日志无未处理异常风暴

### P1（总结 + Dense RAG）

- [ ] 定时/手动总结、水位增量、报告发送、订阅推送
- [ ] RAG ingest：New/Edit/Delete 同步索引（revision 覆盖与 point 删除；delete_pending 事务化）
- [ ] repair 任务：模拟 kill -9 后 pending/delete_pending 收敛（最终一致验证）
- [ ] RAG Query Harness 全链路可查询并可调 AI（answer 只传 IDs，后端重建 context）
- [ ] reindex worker：blue/green + alias 切换 + per-kind checkpoint 断点续跑
- [ ] stale downgrade 生效

### P2（互动 + 完整 AI）

- [ ] 讨论会话（每帖一 conversation、记录/触发分离、@Bot 问答）
- [ ] hybrid（dense+sparse/RRF）、LLM rerank、Query Analyzer
- [ ] Vision 双层（media_analyses SoT + 描述入索引 + 原图入回答；原图不可得 → persisted description fallback）
- [ ] **User Memory：抽取入 user_memories + 检索注入**（R3.1 新增）
- [ ] **讨论群图片多模态问答端到端**（R3.1 新增）
- [ ] 投稿/投票/欢迎互动体系
- [ ] 用户命令与配额

## 4. 附录

### 4.1 术语表

| 术语 | 含义 |
|---|---|
| SoT / Derived Index | MySQL 唯一事实源 / Qdrant 可重建派生索引（Invariant 1） |
| ChatRef | `domain.ChatRef{PeerKind, rawID}`——裸 ID 空间重叠，必须携带类型（01 §4.1、02 §1.1） |
| canonical / revision | 消息当前态 / 不可变历史修订流 |
| 索引状态机 | pending/indexed/delete_pending/excluded/error——Qdrant 相对 MySQL 最终一致的保证（05 §4） |
| conversation_key | (channel_id, channel_post_message_id)——每帖一 AI 会话 |
| orphan conversation | 频道帖无关联讨论群/评论关闭时的会话状态 |
| DEPENDENCY_UNAVAILABLE / OWN_FATAL | 服务等待依赖 / 服务自身致命错（01 §1.3） |
| derived-index queue | 仅承载索引派生任务、允许按规则丢弃的队列（Delete invalidation 除外，状态机兜底） |
| blue/green reindex | 新版本物理 collection 后台构建 → alias 原子切换 |

### 4.2 功能对照参考（v1 → v2 归属，**非数据迁移**——本项目不迁移任何旧数据）

| v1 功能 | v2 处置 |
|---|---|
| 双 Bot + 队列轮询 | 合并单 Bot，进程内直调（删除全部轮询） |
| config.json / channels.yaml / watchdog 热重载 | settings 配置中心 + 回调（删除文件配置） |
| Telethon session 文件 / ChromaDB | gotd MySQL session / Qdrant（无迁移，全新初始化，02 §5） |
| 转发（两套实现） | 单引擎（03 §3，两项目功能并集） |
| 定时总结/投票/欢迎/投稿/订阅/QA 问答 | P1/P2 分期恢复（ADR-007） |
| Bot 抓取降级 / UserBot 发送回退 | 删除（ADR-001 无降级） |
| AI 消息纯文本+MarkdownV2 | Rich Message（ADR-008） |

### 4.3 ADR 索引

见 [docs/decisions/README.md](../decisions/README.md)（001–008 已冻结）。
