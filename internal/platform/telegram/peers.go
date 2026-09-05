package telegram

import (
	"context"
	"fmt"

	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// PeerBook 是 Bot 账号的 PeerResolver（T5.1 接线；03 §1.5 的 Bot 侧最小面）：
// 经 contrib storage.PeerStorage（telegram_peers 表 bot account 行）解析
// access_hash。表未命中时对 channel 尝试 bot 侧 channels.getChannels 直查
// （bot 对其成员频道可用，GATE-2 冒烟实证）并回写表；其余未命中报错交调用方
// 处置——不静默零 hash。
type PeerBook struct {
	ps  storage.PeerStorage
	api *tg.Client
}

// NewPeerBook 构造；api 传 Bot 客户端 API（nil = 仅查表不回源）。
func NewPeerBook(ps storage.PeerStorage, api *tg.Client) *PeerBook {
	return &PeerBook{ps: ps, api: api}
}

func peerKindOf(kind domain.PeerKind) dialogs.PeerKind {
	switch kind {
	case domain.PeerUser:
		return dialogs.User
	case domain.PeerChat:
		return dialogs.Chat
	default:
		return dialogs.Channel
	}
}

// InputPeer 解析 domain.ChatRef 为 InputPeer（Outbound.PeerResolver 实现）。
func (p *PeerBook) InputPeer(ctx context.Context, ref domain.ChatRef) (tg.InputPeerClass, error) {
	peer, err := p.ps.Find(ctx, storage.PeerKey{Kind: peerKindOf(ref.Kind), ID: ref.ID})
	if err == nil {
		return inputPeerOf(peer, ref)
	}
	if ref.Kind != domain.PeerChannel || p.api == nil {
		return nil, fmt.Errorf("peer 解析 %s: %w", ref, err)
	}
	// bot 侧回源：channels.getChannels（成员频道可用；GATE-2 实证路径）。
	res, gerr := p.api.ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: ref.ID},
	})
	if gerr != nil {
		return nil, fmt.Errorf("peer 解析 %s（表未命中且回源失败）: %w", ref, err)
	}
	chats, ok := res.(*tg.MessagesChats)
	if !ok {
		return nil, fmt.Errorf("peer 解析 %s: getChannels 意外响应 %T", ref, res)
	}
	for _, c := range chats.Chats {
		ch, ok := c.(*tg.Channel)
		if !ok || ch.ID != ref.ID {
			continue
		}
		peer = storage.Peer{Channel: ch}
		if aerr := p.ps.Assign(ctx, fmt.Sprintf("channel:%d", ref.ID), peer); aerr != nil {
			// 回写失败不阻断发送（下次仍回源）
			_ = aerr
		}
		return inputPeerOf(peer, ref)
	}
	return nil, fmt.Errorf("peer 解析 %s: getChannels 未返回该频道", ref)
}

func inputPeerOf(peer storage.Peer, ref domain.ChatRef) (tg.InputPeerClass, error) {
	switch {
	case peer.User != nil:
		return &tg.InputPeerUser{UserID: peer.User.ID, AccessHash: peer.User.AccessHash}, nil
	case peer.Chat != nil:
		return &tg.InputPeerChat{ChatID: peer.Chat.ID}, nil
	case peer.Channel != nil:
		return &tg.InputPeerChannel{ChannelID: peer.Channel.ID, AccessHash: peer.Channel.AccessHash}, nil
	default:
		return nil, fmt.Errorf("peer 解析 %s: 表中无可用实体", ref)
	}
}

// permanentSendCodes 是发送侧「重试无益」的 tgerr 错误码全集
// （§1.4 + P0 Plan §6：permanent → 一次性标记 terminal，避免 cursor 卡死）。
var permanentSendCodes = []string{
	"CHAT_WRITE_FORBIDDEN", "USER_BANNED_IN_CHANNEL", "CHAT_ADMIN_REQUIRED",
	"CHANNEL_PRIVATE", "CHANNEL_INVALID", "CHAT_ID_INVALID", "PEER_ID_INVALID",
	"MESSAGE_ID_INVALID", "MESSAGE_EMPTY", "MEDIA_INVALID", "MEDIA_EMPTY",
	"PHOTO_INVALID", "DOCUMENT_INVALID", "USER_DEACTIVATED_BAN", "INPUT_USER_DEACTIVATED",
}

// IsPermanentSendError 判定发送错误是否永久性（接线层组合为
// forwarding.FailureClassifier；领域包不 import gotd，01 §2.2）。
// FLOOD_WAIT/网络/5xx 一律 transient（§1.4：failed + 可补发）。
func IsPermanentSendError(err error) bool {
	if err == nil {
		return false
	}
	for _, code := range permanentSendCodes {
		if tgerr.Is(err, code) {
			return true
		}
	}
	return false
}
