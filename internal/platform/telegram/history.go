package telegram

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/gotd/td/tg"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// History 是转发 backfill 的历史拉取侧（03 §3.7；实现 forwarding.HistoryFetcher）。
// minID 为排他下界（Telegram getHistory 的 min_id 语义：仅返回 ID 更大的消息）。
type History struct {
	user  *UserClient
	peers PeerResolver
	log   *slog.Logger
}

func NewHistory(user *UserClient, peers PeerResolver, log *slog.Logger) *History {
	if log == nil {
		log = slog.Default()
	}
	return &History{user: user, peers: peers, log: log}
}

// GetHistory 拉取 chat 中 ID > minID 的最近 limit 条消息（新→旧序返回；
// 引擎侧自行升序排序）。服务消息（MessageService）被过滤。
func (h *History) GetHistory(ctx context.Context, chat domain.ChatRef, minID int64, limit int) ([]domain.ChannelMessage, error) {
	if h.peers == nil {
		return nil, fmt.Errorf("History 未注入 PeerResolver")
	}
	peer, err := h.peers.InputPeer(ctx, chat)
	if err != nil {
		return nil, fmt.Errorf("解析源 peer: %w", err)
	}
	if limit <= 0 {
		limit = 1
	}
	res, err := h.user.Raw().API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  peer,
		MinID: int(minID),
		Limit: limit,
	})
	if err != nil {
		return nil, fmt.Errorf("getHistory chat=%s min=%d: %w", chat, minID, err)
	}
	raw, ok := historyMessages(res)
	if !ok {
		return nil, fmt.Errorf("getHistory 响应类型非消息列表: %T", res)
	}
	return convertHistory(raw), nil
}

// historyMessages 从 getHistory 响应容器提取消息列表（三种形态；NotModified 除外）。
func historyMessages(class tg.MessagesMessagesClass) ([]tg.MessageClass, bool) {
	switch c := class.(type) {
	case *tg.MessagesMessages:
		return c.Messages, true
	case *tg.MessagesMessagesSlice:
		return c.Messages, true
	case *tg.MessagesChannelMessages:
		return c.Messages, true
	default:
		return nil, false
	}
}

// convertHistory 将 tg.Message 映射为领域消息（服务消息过滤，01 §2.3 领域无 gotd）。
func convertHistory(raw []tg.MessageClass) []domain.ChannelMessage {
	out := make([]domain.ChannelMessage, 0, len(raw))
	for _, mc := range raw {
		if m, ok := mc.(*tg.Message); ok {
			out = append(out, ConvertMessage(m))
		}
	}
	return out
}
