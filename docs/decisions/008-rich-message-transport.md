# ADR-008：Telegram Rich Message 出站传输（Bot API HTTP 专项例外）

- 状态：✅ 已拍板
- 日期：2026-08-29
- 关系：ADR-001「Bot 发送全部走 gotd/td」的**唯一专项例外**；目标架构其余部分不受影响。

## 背景

Telegram Bot API 10.2 引入 Rich Markdown / Rich Message（`sendRichMessage` / `sendRichMessageDraft`），支持标题、表格、任务列表、公式、脚注、媒体块——非常适合 AI 回复与总结的呈现。而 MTProto 的 `messages.sendMessage` 只有 `message + MessageEntity[]`，**没有 `sendRichMessage` 等价物**。若在总体设计里直接写「支持 Rich Message」而不动 ADR-001，会造成设计与冻结决策自相矛盾——故以专项例外 ADR 处理，不静默绕过。

## 决策

> **gotd/td 仍是 User/Bot 的核心 Telegram 客户端、唯一 Update 来源和默认发送通道；需要 Rich Markdown 的消息经 Telegram HTTP Bot API `sendRichMessage` 发送——这是 ADR-001 的唯一专项例外。**

```text
一个 Bot、一个 Go 进程、一个 Bot Token

gotd/td MTProto（主基础设施）
├─ updates（事件/命令/回调）
├─ 普通文本/媒体/转发发送
└─ User 抓取

Bot API HTTP（Rich Message 专用 outbound transport）
└─ sendRichMessage / sendRichMessageDraft（net/http 直调，复用同一 token）
```

- **不采用 `gotd/botapi`**（experimental，ADR-001 已否决）；直接用 Go `net/http` 调 Bot API，**不新增运行时、常驻服务、Telegram SDK**。
- 普通文本、媒体、转发等原有业务仍默认走 gotd/td。
- 它不是第二个 Bot，也不是第二个运行时。
- **`AIProvider` 不感知 Telegram Rich Markdown**：AI 产生内容（`AIResponse`，不含 Telegram 特定类型），Telegram presentation/rendering 层负责规范化与发送——AI 输出未来可复用于 WebUI。

## 两条硬限制（实现必须遵守）

1. **`sendRichMessageDraft` 仅限私聊**（官方参数将 chat_id 限定为 private chat）：
   - Bot 私聊 AI：可用 Draft 流式预览 → 最后 `sendRichMessage` 固化。
   - 频道关联讨论群：**不能依赖 Draft 流式输出**；先做 typing / 处理中状态，最终一次发送 Rich Message。群内逐字流式效果若未来要做，需单独设计，不得把私聊 Draft API 硬套过去。
2. **discussion thread ≠ forum topic**：`message_thread_id` 主要针对 forum topic；频道评论线程保留原消息关系，用 `reply_parameters` 回复对应讨论消息。`sendRichMessage` 支持 `reply_parameters` 与 `reply_markup`，Rich Markdown 不妨碍回复指定消息或携带按钮。

## Renderer 约束（细化进总体设计 8.x）

- 协议限制成为 renderer 的 validation/splitting 规则：**32,768 UTF-8 字符、500 blocks、16 层嵌套、50 个媒体附件、表格 ≤20 列**。
- **LLM formatting instruction ≠ Telegram protocol validation**：模型「尽量生成正确」，程序「保证一定能发送」——AI 输出以 Rich Markdown 为首选格式，但必须经 deterministic renderer/validator。
- 长内容**按 block 边界分段**，不按字符硬切（防止切断 fenced code block / table / list / formula / `<details>` / link）。
- 失败路径：网络 / 429 / 5xx → retry（含 FloodWait 服从）；formating reject → **safe fallback** → 普通 Telegram formatting → 纯文本。
- 第一版**统一 `rich_message.markdown` 一种表达**，不同时维护 Markdown / HTML / blocks 三套 renderer。
- 业务层（SummaryService、RAGService、conversation handler）不得直接调用 `sendRichMessage`，一律经出站抽象（`Sender` / `MessageRenderer`，见总体设计第 4 章）。

## 备选与否决理由

- **等 MTProto 提供 Rich Message 等价物**：无时间表，不可控。
- **gotd/botapi**：experimental（ADR-001 已否决）。
- **放弃 Rich Markdown**：AI 总结/回复呈现能力显著受损，且 Bot API 能力已官方稳定提供。

## 来源

Telegram Bot API 10.2 Rich Markdown（本地资料：`docs/telegram-bot-api-10.2-rich-markdown-zh.md`）· [Telegram Bot API](https://core.telegram.org/bots/api/) · [messages.sendMessage（MTProto）](https://core.telegram.org/method/messages.sendMessage)
