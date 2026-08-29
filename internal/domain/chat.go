package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// PeerKind 是 Telegram peer 类型。
// 依据：Telegram 官方 peer 语义——user/chat/channel 的裸 ID 数值空间重叠，
// 任何 chat 身份必须携带类型（docs/design/02-storage.md §1.1 ChatRef 原则）。
type PeerKind uint8

const (
	PeerUser    PeerKind = iota // 私聊用户
	PeerChat                    // basic group
	PeerChannel                 // channel 与 supergroup（MTProto 同类）
)

func (k PeerKind) String() string {
	switch k {
	case PeerUser:
		return "user"
	case PeerChat:
		return "chat"
	case PeerChannel:
		return "channel"
	default:
		return "unknown"
	}
}

// PeerKindFromString 解析持久化层的 chat_type 列（user/chat/channel）。
func PeerKindFromString(s string) (PeerKind, bool) {
	switch s {
	case "user":
		return PeerUser, true
	case "chat":
		return PeerChat, true
	case "channel":
		return PeerChannel, true
	default:
		return 0, false
	}
}

// MarshalJSON 将 PeerKind 序列化为字符串（与持久化层 chat_type 列及 API DTO 约定一致）。
func (k PeerKind) MarshalJSON() ([]byte, error) {
	return []byte(`"` + k.String() + `"`), nil
}

// UnmarshalJSON 从字符串解析 PeerKind（未知值报错，防止静默归零）。
func (k *PeerKind) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	got, ok := PeerKindFromString(s)
	if !ok {
		return fmt.Errorf("未知 PeerKind: %q", s)
	}
	*k = got
	return nil
}

// ChatRef 是全项目统一的 chat 引用（docs/design/01-runtime-and-components.md §4.1）。
// ID 为 MTProto 裸 ID（正数）。Bot API 的编码（user→+ID、chat→-ID、
// channel→-(1000000000000+ID)）仅在 platform/botapi 出站时构造，库内任何表
// 不出现编码后 ID。
type ChatRef struct {
	Kind PeerKind `json:"kind"`
	ID   int64    `json:"id"`
}

func NewChatRef(kind PeerKind, id int64) ChatRef {
	return ChatRef{Kind: kind, ID: id}
}

// String 提供 "channel:12345" 形式的日志友好表示。
func (c ChatRef) String() string {
	return c.Kind.String() + ":" + strconv.FormatInt(c.ID, 10)
}
