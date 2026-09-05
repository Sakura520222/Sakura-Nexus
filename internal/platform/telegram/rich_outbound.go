package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/botapi"
)

// Rich 出站路由（T4.3，01 §4.2：Outbound 内部路由 MTProto / Bot API Rich，
// 领域不可见；07 §1.1 R3.1 路径修正：renderer 与路由均在 platform/telegram）。

// richCaller 是 botapi.Client 的最小调用面（测试注入 fake）。
type richCaller interface {
	Call(ctx context.Context, method string, params, result any) error
}

// plainSender 是 MTProto 兜底发送面（Outbound.sendTextMTProto）。
type plainSender func(ctx context.Context, req domain.SendRequest) (domain.SentMessage, error)

// richRouter 实现 Content→Renderer→Rich 路由、fallback 链（03 §2.7）与
// lazy capability detection（03 §2.9：首次真实使用探测，flag 缓存至进程重启）。
type richRouter struct {
	rich  richCaller
	plain plainSender
	log   *slog.Logger

	mu       sync.Mutex
	disabled bool
	reason   string
}

// OutboundOption 是 Outbound 的可选装配项。
type OutboundOption func(*Outbound)

// WithLog 注入出站侧结构化日志（Rich 路由降级/capability 告警走此 logger）。
func WithLog(lg *slog.Logger) OutboundOption {
	return func(o *Outbound) {
		if lg != nil && o.rich != nil {
			o.rich.log = lg
		}
	}
}

func newRichRouter(rich richCaller, plain plainSender, log *slog.Logger) *richRouter {
	if log == nil {
		log = slog.Default()
	}
	return &richRouter{rich: rich, plain: plain, log: log}
}

// capability 返回 Rich 通道可用性与不可用原因（WebUI 系统页展示 03 §2.9）。
func (r *richRouter) capability() (enabled bool, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disabled {
		return false, r.reason
	}
	if r.rich == nil {
		return false, "rich 未接线"
	}
	return true, ""
}

// disable 置 capability flag（§2.9：禁用 Rich 全部走 fallback，缓存至进程重启）。
func (r *richRouter) disable(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disabled {
		return
	}
	r.disabled, r.reason = true, reason
	r.log.Warn("rich capability 已禁用", "reason", reason)
}

// richSentResult 是 sendRichMessage 返回的 Message 对象中最小字段。
type richSentResult struct {
	MessageID int64 `json:"message_id"`
}

// send 按 §2.7 fallback 链发送 Content：
// Rich reject（400 formatting/unsupported）→ MTProto；ErrRichUnsendable → MTProto；
// 404 method-not-supported → 置 capability flag 并 MTProto；
// 429/5xx/网络耗尽 → 原样返回瞬态错误（§1.4：failed + 可补发，不降级）。
// StyleRich 语义：强制 Rich 通道——通道不可用/已禁用时报错（硬需求不静默换道），
// 但内容性失败仍走 safe fallback（ADR-008 无条件适用）。
func (r *richRouter) send(ctx context.Context, req domain.SendRequest) (domain.SentMessage, error) {
	enabled, reason := r.capability()
	if !enabled {
		if req.Style == domain.StyleRich {
			return domain.SentMessage{}, fmt.Errorf("rich 通道不可用（StyleRich 强制 Rich）: %s", reason)
		}
		return r.fallback(ctx, req, reason)
	}

	msgs, err := RichMessages(NormalizeRichMarkdown(req.Content.Text))
	if err != nil {
		return r.fallback(ctx, req, "内容超限: "+err.Error())
	}

	var first domain.SentMessage
	for i, m := range msgs {
		params, err := buildRichParams(m, req, i == 0)
		if err != nil {
			return first, err // ChatID 编码等调用方契约错误，不降级
		}
		var out richSentResult
		callErr := r.rich.Call(ctx, "sendRichMessage", params, &out)
		if callErr != nil {
			return r.classifyAndRecover(ctx, req, callErr, first)
		}
		if i == 0 {
			first = domain.SentMessage{Chat: req.Chat, MessageID: out.MessageID}
		}
	}
	return first, nil
}

// classifyAndRecover 按 Telegram API 错误语义处置失败（§2.9：不写死 HTTP 码，
// 404=method-not-supported、400=formatting/unsupported reject、429/5xx=瞬态）。
func (r *richRouter) classifyAndRecover(ctx context.Context, req domain.SendRequest, err error, first domain.SentMessage) (domain.SentMessage, error) {
	var apiErr *botapi.APIError
	if !errors.As(err, &apiErr) {
		return first, err // 网络错误（重试耗尽）= 瞬态
	}
	switch apiErr.Code {
	case http.StatusNotFound:
		r.disable(fmt.Sprintf("sendRichMessage 不被服务端支持 (404 %s)", apiErr.Description))
		return r.fallback(ctx, req, "method-not-supported (404)")
	case http.StatusBadRequest:
		return r.fallback(ctx, req, "formatting reject (400): "+apiErr.Description)
	default:
		return first, err // 429/5xx 耗尽 = 瞬态（可补发）
	}
}

// fallback 降级到 MTProto（§2.7：每次降级记 metric + warn——P0 以结构化日志
// 充当 metric，WebUI 日志流可观测；独立 metrics 属 T5.x 观测面）。
// 注：AI 内容无 entities，fallback 链的「普通 formatting」层与纯文本层在 P0
// 重合（renderer 实体再生成属后续增强）。
func (r *richRouter) fallback(ctx context.Context, req domain.SendRequest, reason string) (domain.SentMessage, error) {
	r.log.Warn("rich 发送降级 MTProto", "reason", reason, "chat", req.Chat.String())
	if r.plain == nil {
		return domain.SentMessage{}, fmt.Errorf("rich 降级失败: MTProto 兜底未接线")
	}
	return r.plain(ctx, plainOf(req))
}

// plainOf 构造 MTProto 兜底请求（Content 文本；P0 SendText 不携带 markup）。
func plainOf(req domain.SendRequest) domain.SendRequest {
	return domain.SendRequest{
		Chat:    req.Chat,
		Text:    req.Content.Text,
		ReplyTo: req.ReplyTo,
		Silent:  req.Silent,
	}
}

// buildRichParams 构造 sendRichMessage 请求体（03 §2.4）：chat_id（botapi
// 三态编码）、rich_message.markdown、reply_parameters（仅首块；禁
// message_thread_id，ADR-008 硬限制）、reply_markup（inline keyboard URL
// 按钮，03 §2.6）、disable_notification。
func buildRichParams(markdown string, req domain.SendRequest, first bool) (map[string]any, error) {
	chatID, err := botapi.ChatID(req.Chat)
	if err != nil {
		return nil, fmt.Errorf("rich chat_id 编码: %w", err)
	}
	params := map[string]any{
		"chat_id": chatID,
		"rich_message": map[string]any{
			"markdown":              markdown,
			"skip_entity_detection": false,
		},
	}
	if first && req.ReplyTo != 0 {
		params["reply_parameters"] = map[string]any{"message_id": req.ReplyTo}
	}
	if req.Markup != nil {
		rows := make([]any, 0, len(req.Markup.Rows))
		for _, row := range req.Markup.Rows {
			buttons := make([]any, 0, len(row))
			for _, b := range row {
				buttons = append(buttons, map[string]any{"text": b.Text, "url": b.URL})
			}
			rows = append(rows, buttons)
		}
		params["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	if req.Silent {
		params["disable_notification"] = true
	}
	return params, nil
}
