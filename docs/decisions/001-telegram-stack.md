# ADR-001：技术栈与 Telegram 库 — Go + gotd/td

- 状态：✅ 已拍板
- 日期：2026-08-29

## 背景

两个源项目（Sakura-Bot v1.8.9、TG-Forwarder）均为 Python + Telethon。本次是重写整合：真正继承的是业务逻辑（真实账号抓取、Bot 发送、转发规则、RAG、总结、WebUI），不是 Python 代码本身。项目首要约束是 Linux 服务器长期运行、稳定、低占用，而非少改旧代码。

2026-08 生态核查（用户提供并核实）：

- **gotd/td**：活跃维护（2026-08 仍有提交与 issue 活动），纯 Go 的 MTProto 2.0 / Telegram API 实现。
- **GramJS（Node/TS）**：已于 **2026-07-14 归档**，官方提示迁移到 teleproto。不作为新项目基石。
- **Telethon（Python）**：原 GitHub 仓库已于 **2026-02-21 归档**。
- **grammers（Rust）**：可用，但仓库已迁 Codeberg，且业务型 Bot 上开发成本明显高于 Go。

## 决策

**语言与 Telegram 栈：Go + gotd/td。** 候选排序：C（Go）> B（TS/teleproto）> D（Rust）> A（Python）。

### 客户端分工（语义铁律，全项目强制）

| 客户端 | 库 | 职责 |
|---|---|---|
| **真实账号** | gotd/td，MTProto | 监听源频道、历史抓取、编辑/删除事件、媒体下载、加入频道；`messages.getDiscussionMessage` 等 user-only 方法 |
| **Bot 账号** | 同一 gotd/td 基础设施，**不引入独立运行时** | 所有发送、用户命令、回调按钮、订阅推送、投稿交互 |

- `gotd/botapi`（experimental）**不作为核心依赖**。
- 删除旧项目的兼容路径：**Bot 抓取降级、UserBot 回退发送，一律不存在**。User 抓取 / Bot 发送，无降级。
- **ADR-008 例外（2026-08-29 补充）**：需要 Rich Markdown 的消息（AI 回复/总结等）允许经 HTTP Bot API `sendRichMessage` 发送，是「Bot 发送走 gotd/td」的唯一专项例外，详见 [008-rich-message-transport.md](008-rich-message-transport.md)。

### 内存预期

不预设数字（此前「Go 常驻 20–50MB」的说法已撤回）。Go 大概率显著低于原 Python 方案，但最终 RSS 以实际功能与负载压测为准；媒体下载、相册聚合、RAG、WebUI 都可能制造峰值。

## 备选与否决理由

- **Python + Telethon**：继承代码最多，但 Telethon 仓库归档、占用高、旧架构债多。
- **Node/TS**：GramJS 归档，teleproto 尚新。
- **Rust + grammers**：占用最低，但开发速度与业务功能交付不匹配。

## 影响

- 旧 Python 代码不可复用，仅作业务逻辑参考（调研报告：`docs/research/`）。
- 所有依赖、schema、文档按 Go 生态重新确立（见 ADR-005、ADR-006）。
