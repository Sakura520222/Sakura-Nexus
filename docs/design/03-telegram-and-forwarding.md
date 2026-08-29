# 03 Telegram 集成与转发

- 状态：📝 R3.1.1，待用户核对修改点
- 受约束 ADR：[001](../decisions/001-telegram-stack.md) · [002](../decisions/002-runtime-model.md) · [008](../decisions/008-rich-message-transport.md)

## 1. gotd 客户端集成

### 1.1 客户端构造

- `telegram.NewClient(apiID, apiHash, Options{…})` ×2（user / bot slot），共享同一 API 凭据（`.env` 的 `TELEGRAM_API_ID/HASH`），各自独立 session。
- Device 参数**固定常量**（设备型号/系统版本不变）——避免每次启动随机指纹触发风控。
- 中间件：gotd 内建 flood waiter（自动 sleep）、重试包装、日志适配（降噪网络层）。
- 连接保活与重连交给 gotd；可用性状态经 `Availability` 接口上报（01 §1.3）。

### 1.2 持久状态映射（表结构见 02 §2.1）

| gotd 抽象 | MySQL 表 | 说明 |
|---|---|---|
| `session.Storage`（LoadSession/StoreSession） | `gotd_sessions` | opaque blob，不解析 |
| `updates.StateStorage`（Get/SetState、Get/SetChannelPts，按 userID 分区） | `telegram_update_states` / `telegram_channel_states` | account+user_id 身份；换号清旧行 |
| contrib `storage.PeerStorage`（Add/Find/Assign/Resolve/Iterate） | `telegram_peers`（PK(account, peer_type, peer_id)） | `storage.Peer` 序列化 + username/title 快照；内存索引由 contrib 层维护、MySQL 持久 |

### 1.3 updates 分发与 gap recovery

- **User 客户端**：`updates.Manager`（gotd 当前对外 API；持久化 StateStorage 见 §1.2）→ dispatcher → 领域 handler（收到 `domain.ChannelMessage`，已剥离 gotd 类型）：forwarding 订阅 NewMessage（P0）；rag 订阅 New/Edit/Delete（P1）；conversation 订阅讨论群消息（P2）。
- 常规 gap：Manager 的 difference recovery 自动补齐（含离线期间），channel PTS 走 per-channel 表。
- **异常边界（R3.1.1 按 gotd callback 签名区分 scope，不再宣称「无显式补拉」）**：
  - `OnLoadChannelStateFailed(channelID)`（含 `ChannelDifferenceTooLong`）→ **channel 级**：标记该 channel `recovery_required` → User `GetHistory` 定向补抓 → 补抓消息走**同一条 canonical/dedup 管线**（重复被 UNIQUE 键与去重吸收）→ 恢复并写入新 state。
  - `OnTooLong()` / `OnLoadUserStateFailed()`（无 channelID，**account/global 级**）→ 对当前全部受管源频道做定向 history reconciliation（同管线）。
  - `OnChannelInaccessible(channelID)` → **不是补抓**：标记该源频道 `unavailable`（被踢出/不可访问）、停止其 recovery 循环、WebUI 显示 degraded/权限错误——防止被踢出频道后无意义反复补抓。
- **Bot 客户端**：P0 仅连接保活与发送；私聊命令/回调在 P1/P2 接入 dispatcher。Bot **不参与任何频道抓取**（ADR-001 无降级）。
- handler panic 由 dispatcher 边界 recover 记日志，不影响连接与其他 handler（01 §5.3）。

### 1.4 FloodWait / 重试矩阵（全项目统一）

| 层 | 错误 | 策略 |
|---|---|---|
| MTProto（gotd） | FloodWaitError | 统一由 `gotd/contrib/middleware/floodwait`（`Run` / `WithMaxWait` / `WithMaxRetries`）控制，`MaxWait=1h`；**超限语义 = 本次发送失败**：计入 failed、保持未转发状态、**可由回溯补发恢复**——不是「丢弃」，不存在静默消息丢失 |
| Bot API HTTP | 429 + retry_after | 服从 `retry_after + 1s`，重试上限 3 次；超限同上（failed + 可补发） |
| Bot API HTTP | 5xx / 网络错误 | 指数退避 1/2/4s，上限 3 次 |
| AI API | 429 / 5xx | 指数退避 + jitter，3 次；转发改写失败→降级原文；总结失败→任务失败记日志（不降级） |
| MySQL | 闪断 | 连接池负责重连；**仅 repository 明确判定幂等的读/写可 retry 一次**；事务提交状态未知时**不得自动重放**（防重复 revision/统计），交由上层状态机（如 index_state）收敛 |

### 1.5 实体解析与 file_reference

- 解析顺序：`telegram_peers`（contrib 内存索引，启动时从 MySQL 预热）→ 未命中则按 alias（username/phone）查 `telegram_peer_aliases`（R3.1 落地，重启后 `Assign/Resolve` 语义完整）→ 仍未命中经 User 客户端 resolve 并回存 peers + aliases → 失败报错。username 改绑时 `Assign` upsert 替换旧绑定。
- `file_reference` 是**可刷新的缓存引用**（02 §2.3）：下载遇 `FILEREF_INVALID` → 经 User 重新 `get_messages` 刷新 media 元数据（写回 messages.media）→ 重试一次。

### 1.6 相册聚合算法（R3.1：真动态窗口 + 聚合过滤 + 全成员去重）

```text
状态：map[grouped_id] → {msgs, quietTimer, hardDeadline}
首条消息：入 state，启动 quiet timer（默认 400–500ms）与 hard deadline（默认 2.0s）
后续同组：append 并**重置 quiet timer**；满 10 条（Telegram 相册上限）→ 立即 flush
触发 flush 的三条件（任一）：
  quiet timeout（组内消息流静默）OR hard deadline OR 集满 10 条
窗口结束后才到达的同组消息：视为独立新消息走常规流程（记 metric warn，不静默丢弃）
窗口参数可调（settings.forwarding.album_quiet_ms / album_hard_deadline_ms）
```

- **过滤对象（R3.1 修正：不只看首条）**：规则匹配/关键词过滤基于**聚合文本**（首条 caption + 各成员 caption/文本拼接）；media_types 过滤基于**全体成员媒体类型并集**。
- **全成员去重（R3.1）**：发送成功后把**相册全部成员的 source message ID** 写入 `forwarded_messages`（逐条记录，target 相同）——否则未被记录的成员后续可能被当作独立消息再次转发。
- flush 幂等：以「首条 (chat_type, chat_id, message_id)」为聚合键。

## 2. Bot 出站传输与 Rich Message Rendering（8.x）

### 2.1 能力矩阵

见 01 §4.3（reply / keyboard / media / entities / 长消息 / 流式预览，两通道对照）。

### 2.2 RichMarkdownNormalizer（deterministic，禁止信任模型输出）

- 输入：`AIResponse.Text`（system prompt 已要求输出 Telegram Rich Markdown：标题、列表、表格、引用、代码块、公式、链接；禁止 HTML）。
- 规范化步骤：剥离不支持的 HTML/裸标签 → 统一标题层级 → 链接规范化 `[text](url)` → 代码块补语言标注 → 空白规整。
- **LLM formatting instruction ≠ protocol validation**：normalizer + validator 保证「一定能发送」。

### 2.3 Validator 与 block-aware 切分

- 解析为 block 流（heading / paragraph / list / table / code / quote / formula / footnote / media）。
- 校验规则（协议硬限制，超限即切分或报错）：**32,768 UTF-8 字符、500 blocks、16 层嵌套、50 媒体附件/条、表格 ≤20 列**。
- 切分策略：按 block 边界贪心组装成多条合法消息；代码块/表格/公式整体优先不切；单个 block 自身超限 → 行级二次切分；仍超限 → validation error → 走 fallback 链。**禁止按字符数硬切**。

### 2.4 sendRichMessage / sendRichMessageDraft

- `sendRichMessage`：`POST /bot{token}/sendRichMessage`，body 含 `chat_id`（由本层按 PeerKind 编码：user→`+ID`、chat→`-ID`、channel→`-(1000000000000+ID)`，02 §1.1 边界）、`rich_message.markdown`、`reply_parameters`、`reply_markup`。
- `sendRichMessageDraft`：**仅私聊**（P2 Bot 私聊 AI 场景）：Draft 流式更新预览 → 最终 `sendRichMessage` 固化。群/讨论群一律：`sendChatAction(typing)` / 处理中状态 → 一次发送。

### 2.5 reply / thread 映射

- 讨论线程内回复：`reply_parameters.message_id` = 目标讨论消息 ID。**不使用 `message_thread_id`**（forum topic 专用，ADR-008 硬限制）。

### 2.6 inline keyboard

`domain.Keyboard` → `reply_markup.inline_keyboard`（Rich 通道）与 MTProto reply markup 双映射，业务层只见 `domain.Keyboard`。

### 2.7 fallback 链

`Rich reject（400 formatting/unsupported）→ 普通 Telegram formatting（entities）→ 纯文本`。每次降级记 metric + warn 日志（WebUI 可观测）。

### 2.8 Bot API HTTP 客户端（platform/botapi）

- `net/http` 复用连接；超时 30s；429/5xx 按 §1.4 矩阵；同一 `TELEGRAM_BOT_TOKEN`；**日志脱敏**：不得打印含 token 的 URL（06 §5）。
- 无 SDK、无常驻连接池之外的状态。

### 2.9 能力版本兼容（R3.1：lazy first-use detection）

不做「启动探测」——不存在无副作用的完整 `sendRichMessage` capability probe。改为**首次真实使用时探测**：第一次真实 Rich 发送返回 method-not-supported 语义（按 Telegram API 错误语义判定，**不写死为 HTTP 400**）→ 置 capability flag（禁用 Rich，全部走 fallback），WebUI 系统页显示该限制；flag 缓存至进程重启。

## 3. 转发引擎（P0 核心）

### 3.1 事件入口与规则匹配

- User NewMessage → engine 入口 → 相册聚合分支（§1.6）或直接流程。
- 规则匹配（R3.1.1：全程 ChatRef）：`ChatRef{kind, id}` 精确匹配优先；未命中再以归一化 username（去 `@`、小写）匹配辅助列；**命中多条规则 → 逐规则独立处理**（各自过滤/去重/发送，单规则失败不影响其他）。

### 3.2 过滤链（顺序固定，任一拒绝即终止该规则并记原因）

```text
频道校验（is_channel/broadcast）
→ 相册聚合（整组判定：聚合文本 + 媒体类型并集，§1.6）
→ 去重查（forwarded_messages 完整 ChatRef 键，§3.5）
→ forward_original_only（带 forward 头的消息拒绝）
→ keywords（子串、大小写不敏感、任一命中；空=过）
→ patterns（正则 re.search 任一命中；坏正则记日志按不匹配）
→ blacklist words / blacklist patterns（任一命中即拒）
→ media_types（空=全部；text/photo/video/audio/document/animation/sticker/voice/video_note/any）
→ AI 改写（ai_enabled；失败降级原文）
```

### 3.3 三态发送

1. **纯文本**：`SendRequest{Text, Entities 透传}` 保留原格式；>4096 按 entity 边界分段发送。
2. **媒体/相册**：User `DownloadMedia` 流式写**临时文件**（非内存 buffer；单文件大小上限可配，默认 2GB）→ Bot `send_file`（保留 attributes/spoiler 等）→ `defer` 删除临时文件；相册整体 `send_file(files[])` 重建，caption = 改写后文本 + 底栏。
3. **copy_mode=forward**：Bot `forward_messages` 原样转发——**前置条件**：Bot 可读源频道（规则保存时预检，运行期失败记 error 跳过该规则）。

### 3.4 发送队列与限流

- 全局发送队列（容量 100，阻塞背压，单消费者串行——01 §5.2）。
- 每规则随机延迟 `uniform(delay_min_sec, delay_max_sec)`（默认 0.5–2.0s）。
- 队列内任务按 §1.4 矩阵处理 FloodWait/重试。

### 3.5 去重与统计

- 去重键 `(source_chat_type, source_chat_id, source_message_id, target_chat_type, target_chat_id)`（R3.1.1：源与目标均为完整 ChatRef）；`content_dedup` 开启时附加内容哈希比对（防删帖重发）。
- **发送成功才写 forwarded_messages**；stats 按**真实成败**计数（修复源项目假成功问题）。

### 3.6 底栏模板

占位符：`{source_link} {source_title} {target_title} {source_channel} {target_channel} {message_id} {assistant_bot}`。源链接：公开频道 `https://t.me/{username}/{msg_id}`；私有频道 `https://t.me/c/{stripped_id}/{msg_id}`（stripped = 裸 ID，无 -100 mark）。

### 3.7 回溯补发（手动）

WebUI 触发：`POST /api/forwarding/rules/{id}/backfill {limit}` → `GetHistory(minID=rule.last_message_id, limit)` → 逐条走完整过滤链（已转发的被去重自然跳过）→ 完成后更新水位。单次上限（默认 200 条）防风暴。

### 3.8 加入频道与权限预检

- 新建/修改规则的源频道：User `JoinChannel`（公开频道自动加；私有频道无法自动加，WebUI 明确提示需手动）。
- 目标频道：预检 Bot 具备发帖权限（无权限则规则保存时报错，避免运行期反复失败）。

### 3.9 临时文件管理

- 下载目录：系统临时目录下 `sakura-bot/` 子目录；文件名带 (chat_id, message_id) 便于排查。
- 生命周期：发送完成/失败即删；启动时清理残留（崩溃保护）；目录可配。
