// Package botapi 实现 ADR-008 的 Telegram Bot API HTTP 出站通道
// （sendRichMessage / sendRichMessageDraft），net/http 直调、限流/429/重试、
// token 日志脱敏。
// 设计：docs/design/03-telegram-and-forwarding.md §2.8。
package botapi
