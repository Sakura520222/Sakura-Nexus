package domain

// SendStyle 决定出站通道路由（01 §4.2：Auto 默认——有 Content 走 Rich 失败降级；
// Plain 强制 MTProto；Rich 强制 Bot API sendRichMessage）。
type SendStyle uint8

const (
	// StyleAuto：Content != nil → Rich（失败 fallback）；否则 MTProto。
	StyleAuto SendStyle = iota
	// StylePlain：强制 MTProto（entities 透传 / send_file / forward）。
	StylePlain
	// StyleRich：强制 Bot API sendRichMessage（ADR-008 例外通道）。
	StyleRich
)

func (s SendStyle) String() string {
	switch s {
	case StylePlain:
		return "plain"
	case StyleRich:
		return "rich"
	default:
		return "auto"
	}
}

// SendStyleFromString 解析持久化/JSON 值。
func SendStyleFromString(s string) (SendStyle, bool) {
	switch s {
	case "auto", "":
		return StyleAuto, true
	case "plain":
		return StylePlain, true
	case "rich":
		return StyleRich, true
	default:
		return 0, false
	}
}

// MessageContent 是结构化内容（AI 输出的领域形态——AIResponse.Text 的载体）。
// Rich 渲染发生在 platform/telegram 内部（MessageRenderer，01 §2.3），
// 领域只知道「发送这份 MessageContent」。
type MessageContent struct {
	Text      string     `json:"text"`
	MediaRefs []MediaRef `json:"mediaRefs,omitempty"`
}

// KeyboardButton 是 inline keyboard 单按钮。
type KeyboardButton struct {
	Text string `json:"text"`
	URL  string `json:"url,omitempty"` // URL 按钮（P0 仅此一种；callback_data 是 P2 命令体系）
}

// Keyboard 是 inline keyboard 的领域形态（Bot API reply_markup 与 MTProto
// markup 的双映射由 platform 层完成，03 §2.6）。
type Keyboard struct {
	Rows [][]KeyboardButton `json:"rows"`
}

// SendRequest 是全部出站发送的统一模型（01 §4.1）——Rich 不长平行业务 API。
type SendRequest struct {
	Chat     ChatRef         `json:"chat"`
	Style    SendStyle       `json:"style"`
	Content  *MessageContent `json:"content,omitempty"`  // 结构化内容 → Rich 通道
	Text     string          `json:"text,omitempty"`     // Plain 直发文本
	Entities []Entity        `json:"entities,omitempty"` // 原 entities 透传（转发复制语义）
	Media    []MediaRef      `json:"media,omitempty"`    // 本地文件或媒体引用
	Caption  string          `json:"caption,omitempty"`
	ReplyTo  int64           `json:"replyTo,omitempty"` // 回复目标消息
	Markup   *Keyboard       `json:"markup,omitempty"`  // inline keyboard
	Silent   bool            `json:"silent,omitempty"`
}

// SentMessage 是发送结果的统一回执（无论通道）。
type SentMessage struct {
	Chat      ChatRef `json:"chat"`
	MessageID int64   `json:"messageId"`
}

// AIResponse 是 AIProvider 的通用输出契约（05 §2——无 Telegram 类型；
// presentation 层负责渲染，可原样复用于 WebUI）。
type AIResponse struct {
	Text      string         `json:"text"`
	MediaRefs []MediaRef     `json:"mediaRefs,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"` // model / usage / finish_reason
}
