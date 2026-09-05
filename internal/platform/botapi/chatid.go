package botapi

import (
	"fmt"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// ChatID 将 domain.ChatRef 编码为 Bot API chat_id 三态（internal/domain/chat.go
// 冻结编码：user→+ID、chat→-ID、channel→-(1000000000000+ID)）。编码后 ID 仅在
// platform/botapi 出站时构造，库内任何表不出现（01 §4.1）。
func ChatID(ref domain.ChatRef) (int64, error) {
	if ref.ID <= 0 {
		return 0, fmt.Errorf("botapi: chat_id 编码要求 MTProto 裸正 ID，得 %s", ref)
	}
	switch ref.Kind {
	case domain.PeerUser:
		return ref.ID, nil
	case domain.PeerChat:
		return -ref.ID, nil
	case domain.PeerChannel:
		return -(1_000_000_000_000 + ref.ID), nil
	default:
		return 0, fmt.Errorf("botapi: 未知 PeerKind %d", ref.Kind)
	}
}
