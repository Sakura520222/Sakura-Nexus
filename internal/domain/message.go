package domain

import "time"

// MessageRef 唯一定位一条消息。
// 内存形态对应 messages 表 UNIQUE(chat_type, chat_id, message_id)（02-storage §2.3）。
type MessageRef struct {
	Chat      ChatRef `json:"chat"`
	MessageID int64   `json:"messageId"`
}

// Entity 是 Telegram 消息格式化实体（转发复制时原样透传，03-storage §3.3；
// 类型字符串对齐 tg.MessageEntityClass）。
type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`

	URL    string `json:"url,omitempty"`    // text_link
	UserID int64  `json:"userId,omitempty"` // text_mention
}

// MediaRef 描述消息内单个媒体的元数据（非二进制）。
// FileRef 是可刷新的临时缓存引用而非永久凭据（02-storage §2.3）——
// 失效时经 User 客户端重新解析刷新。
type MediaRef struct {
	Key      string `json:"key"` // 消息内唯一标识（如 photo:0）——下载与 vision 分析定位
	Type     string `json:"type"`
	MimeType string `json:"mimeType,omitempty"`
	FileRef  string `json:"fileRef,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
	Duration int    `json:"duration,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// ForwardHeader 表示消息携带的转发来源头（存在即「非原创」，
// 供 forward_original_only 过滤判定，03-storage §3.2）。
type ForwardHeader struct {
	FromUserID int64  `json:"fromUserId,omitempty"`
	FromChatID int64  `json:"fromChatId,omitempty"`
	FromTitle  string `json:"fromTitle,omitempty"`
}

// ChannelMessage 是从 Telegram 更新/历史拉取解析出的领域消息。
// 它是 Fetcher 接口的输出（01 §2.3）——dispatcher 层完成 gotd → domain 的映射，
// 领域与 repository 不接触 gotd 类型（P0 Plan R1 必改 2）。
type ChannelMessage struct {
	Ref               MessageRef     `json:"ref"`
	GroupedID         int64          `json:"groupedId,omitempty"`     // 相册聚合键（0=非相册）
	ThreadTopID       int64          `json:"threadTopId,omitempty"`   // 讨论线程顶层消息（非线程=自身；0=未知）
	SenderUserID      int64          `json:"senderUserId,omitempty"`
	SenderUsername    string         `json:"senderUsername,omitempty"`
	SenderDisplayName string         `json:"senderDisplayName,omitempty"`
	Text              string         `json:"text,omitempty"`
	Entities          []Entity       `json:"entities,omitempty"`
	Media             []MediaRef     `json:"media,omitempty"`
	ForwardFrom       *ForwardHeader `json:"forwardFrom,omitempty"` // 非空 = 二手转发
	PublishedAt       time.Time      `json:"publishedAt"`
	EditedAt          *time.Time     `json:"editedAt,omitempty"`
}
