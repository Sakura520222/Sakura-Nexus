# 03 Telegram 集成与转发

- 状态：⏳ 待成文
- 受约束 ADR：[001](../decisions/001-telegram-stack.md) · [002](../decisions/002-runtime-model.md) · [008](../decisions/008-rich-message-transport.md)

## 覆盖内容

- gotd 集成：session 的 MySQL storage 实现、updates 分发与 gap 恢复、FloodWait/重试矩阵、实体解析与缓存、相册聚合窗口算法
- `8.x Bot 出站传输与 Rich Message Rendering`（用户指定小节）：
  - MTProto / Bot API capability matrix
  - Rich Markdown normalization 与 deterministic validation
  - `sendRichMessage` / `sendRichMessageDraft`（Draft 仅私聊）
  - reply / thread 映射（discussion thread ≠ forum topic，用 `reply_parameters`）
  - inline keyboard、fallback、block 边界长消息切分（32,768 字符 / 500 blocks / 16 层嵌套 / 50 媒体 / 表格 ≤20 列）
  - Bot API HTTP retry / FloodWait / 429
  - Rich Message feature / version 兼容性
- 发送链（AI raw output → RichMarkdownNormalizer → Validate → sendRichMessage → retry / safe fallback）
- 转发引擎：规则模型、过滤链顺序、发送队列与限流（随机延迟）、去重键、底栏模板、统计、回溯补发
