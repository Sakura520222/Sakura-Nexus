package telegram

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gotd/contrib/storage"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/query/dialogs"
	"github.com/gotd/td/telegram/updates"
	updhook "github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// MessageSink 消费 dispatcher 映射后的领域消息事件——由接线方（app/smoke）
// 连接到 canonical writer 与领域回调；platform/telegram 不依赖具体实现。
type MessageSink interface {
	OnNew(ctx context.Context, m domain.ChannelMessage) error
	OnEdit(ctx context.Context, m domain.ChannelMessage) error
	OnDelete(ctx context.Context, ref domain.MessageRef) error
}

// RecoverySink 是 updates.Manager 恢复回调的处置接口（03 §1.3：channel 级补抓 /
// global 全量 reconciliation / inaccessible → unavailable 停止补抓）。
type RecoverySink interface {
	ChannelGap(ctx context.Context, channelID int64)
	GlobalGap(ctx context.Context)
	ChannelInaccessible(ctx context.Context, channelID int64)
}

// UpdatesConfig 是 SetupUserUpdates 的装配依赖。
type UpdatesConfig struct {
	State updates.StateStorage
	Peers storage.PeerStorage
	Sink  MessageSink
	Log   *slog.Logger // 可选；默认 slog.Default
}

// SetupUserUpdates 一站式装配（T1.3）：
// 构造 Manager（去重/gap recovery）+ Dispatcher（领域映射）+ Recovery（定向补抓），
// 并返回已挂好 UpdateHandler/Middleware 的 UserClient。
//
// 调用方在 client.Run 回调内：self := user.Self(ctx) 后执行
// manager.Run(ctx, user.Raw().API(), self.ID, updates.AuthOptions{})。
func SetupUserUpdates(apiID int, apiHash string, session telegram.SessionStorage, cfg UpdatesConfig) (*UserClient, *updates.Manager) {
	lg := cfg.Log
	if lg == nil {
		lg = slog.Default()
	}
	recovery := &Recovery{lg: lg, peers: cfg.Peers, sink: cfg.Sink}
	dispatcher := &Dispatcher{sink: cfg.Sink, lg: lg}

	manager := updates.New(updates.Config{
		// contrib UpdateHook：从 updates 收集 users/chats 入 PeerStorage，再传 dispatcher
		Handler: storage.UpdateHook(dispatcher, cfg.Peers),
		Storage: cfg.State,
		// 恢复回调（R3.1.1 scope 语义；gotd 回调不透传 ctx，适配为 background 补抓）
		OnChannelTooLong:         func(channelID int64) { recovery.ChannelGap(context.Background(), channelID) },
		OnLoadChannelStateFailed: func(channelID int64) { recovery.ChannelGap(context.Background(), channelID) },
		OnTooLong:                func() { recovery.GlobalGap(context.Background()) },
		OnLoadUserStateFailed:    func() { recovery.GlobalGap(context.Background()) },
		OnChannelInaccessible:    func(channelID int64) { recovery.ChannelInaccessible(context.Background(), channelID) },
	})

	user := NewUserClient(apiID, apiHash, session,
		WithUpdateHandler(manager),
		WithMiddleware(
			updhook.UpdateHook(manager.Handle), // 自身请求产生的 UpdatesBox 喂回 manager
			updhook.AffectedHook(manager),      // affectedMessages 同步本地 PTS
		))
	recovery.client = user.Raw()
	return user, manager
}

// Dispatcher 将去重后的 tg 更新解包为领域消息事件（canonical writer 的唯一喂入点）。
type Dispatcher struct {
	sink MessageSink
	lg   *slog.Logger
}

// Handle 实现 telegram.UpdateHandler。
func (d *Dispatcher) Handle(ctx context.Context, u tg.UpdatesClass) error {
	switch tu := u.(type) {
	case *tg.Updates:
		return d.handleList(ctx, tu.Updates)
	case *tg.UpdatesCombined:
		return d.handleList(ctx, tu.Updates)
	case *tg.UpdateShort:
		return d.handleList(ctx, []tg.UpdateClass{tu.Update})
	case *tg.UpdateShortMessage, *tg.UpdateShortChatMessage, *tg.UpdateShortSentMessage:
		// 私聊短消息：P0 无消费方（conversation 是 P2），记录调试即可
		d.lg.Debug("跳过私聊短消息更新", "type", fmt.Sprintf("%T", tu))
	}
	return nil
}

func (d *Dispatcher) handleList(ctx context.Context, list []tg.UpdateClass) error {
	for _, up := range list {
		switch t := up.(type) {
		case *tg.UpdateNewMessage:
			if err := d.emitNew(ctx, t.Message); err != nil {
				return err
			}
		case *tg.UpdateNewChannelMessage:
			if err := d.emitNew(ctx, t.Message); err != nil {
				return err
			}
		case *tg.UpdateEditMessage:
			if err := d.emitEdit(ctx, t.Message); err != nil {
				return err
			}
		case *tg.UpdateEditChannelMessage:
			if err := d.emitEdit(ctx, t.Message); err != nil {
				return err
			}
		case *tg.UpdateDeleteChannelMessages:
			d.emitDelete(ctx, t.ChannelID, toInt64Slice(t.Messages))
		case *tg.UpdateDeleteMessages:
			// 普通对话删除（P2 conversation 范畴），P0 记录
			d.lg.Debug("跳过非频道删除更新", "messages", t.Messages)
		}
	}
	return nil
}

func (d *Dispatcher) emitNew(ctx context.Context, mc tg.MessageClass) error {
	m, ok := mc.(*tg.Message)
	if !ok {
		return nil // MessageService 等非文本消息
	}
	if err := d.sink.OnNew(ctx, ConvertMessage(m)); err != nil {
		return fmt.Errorf("sink OnNew msg=%d: %w", m.ID, err)
	}
	return nil
}

func (d *Dispatcher) emitEdit(ctx context.Context, mc tg.MessageClass) error {
	m, ok := mc.(*tg.Message)
	if !ok {
		return nil
	}
	if err := d.sink.OnEdit(ctx, ConvertMessage(m)); err != nil {
		return fmt.Errorf("sink OnEdit msg=%d: %w", m.ID, err)
	}
	return nil
}

func (d *Dispatcher) emitDelete(ctx context.Context, channelID int64, ids []int64) {
	ref := domain.MessageRef{Chat: domain.NewChatRef(domain.PeerChannel, channelID)}
	for _, id := range ids {
		if err := d.sink.OnDelete(ctx, domain.MessageRef{Chat: ref.Chat, MessageID: id}); err != nil {
			d.lg.Error("sink OnDelete 失败", "channel", channelID, "msg", id, "err", err)
		}
	}
}

// ConvertMessage 将 tg.Message 映射为 domain.ChannelMessage（含 peer kind 判定、
// 相册/线程/转发头/媒体元数据/实体/时间四件套）。
func ConvertMessage(m *tg.Message) domain.ChannelMessage {
	out := domain.ChannelMessage{
		Ref:        domain.MessageRef{Chat: chatFromPeer(m.PeerID), MessageID: int64(m.ID)},
		SourceType: "channel_message",
		Text:       m.Message,
		Entities:   convertEntities(m.Entities),
		Media:      convertMedia(m.Media),
	}
	if m.GroupedID != 0 {
		out.GroupedID = m.GroupedID
	}
	if m.Date != 0 {
		out.PublishedAt = unixTime(m.Date)
	}
	if m.EditDate != 0 {
		t := unixTime(m.EditDate)
		out.EditedAt = &t
	}
	if from, ok := m.FromID.(*tg.PeerUser); ok {
		out.SenderUserID = from.UserID
	}
	if hdr, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok {
		if hdr.ReplyToTopID != 0 {
			out.ThreadTopID = int64(hdr.ReplyToTopID)
		} else if hdr.ReplyToMsgID != 0 {
			out.ThreadTopID = int64(hdr.ReplyToMsgID)
		}
	}
	if fwd, ok := m.GetFwdFrom(); ok {
		h := &domain.ForwardHeader{FromTitle: fwd.FromName}
		if fwd.FromID != nil {
			switch f := fwd.FromID.(type) {
			case *tg.PeerUser:
				h.FromUserID = f.UserID
			case *tg.PeerChannel:
				h.FromChatID = f.ChannelID
			}
		}
		out.ForwardFrom = h
	}
	return out
}

func chatFromPeer(peer tg.PeerClass) domain.ChatRef {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return domain.NewChatRef(domain.PeerUser, p.UserID)
	case *tg.PeerChat:
		return domain.NewChatRef(domain.PeerChat, p.ChatID)
	case *tg.PeerChannel:
		return domain.NewChatRef(domain.PeerChannel, p.ChannelID)
	}
	return domain.ChatRef{}
}

func convertEntities(list []tg.MessageEntityClass) []domain.Entity {
	if len(list) == 0 {
		return nil
	}
	out := make([]domain.Entity, 0, len(list))
	for _, e := range list {
		ent := domain.Entity{Type: entityTypeName(e)}
		switch t := e.(type) {
		case *tg.MessageEntityTextURL:
			ent.URL = t.URL
		case *tg.MessageEntityMentionName:
			ent.UserID = t.UserID
		}
		// 通用 offset/length（全部实体共有字段，用接口内省太啰嗦——按常见类型断言）
		switch t := e.(type) {
		case *tg.MessageEntityBold:
			ent.Offset, ent.Length = t.Offset, t.Length
		case *tg.MessageEntityItalic:
			ent.Offset, ent.Length = t.Offset, t.Length
		case *tg.MessageEntityCode:
			ent.Offset, ent.Length = t.Offset, t.Length
		case *tg.MessageEntityPre:
			ent.Offset, ent.Length = t.Offset, t.Length
		case *tg.MessageEntityTextURL:
			ent.Offset, ent.Length = t.Offset, t.Length
		case *tg.MessageEntityURL:
			ent.Offset, ent.Length = t.Offset, t.Length
		case *tg.MessageEntityMentionName:
			ent.Offset, ent.Length = t.Offset, t.Length
		}
		out = append(out, ent)
	}
	return out
}

func entityTypeName(e tg.MessageEntityClass) string {
	switch e.(type) {
	case *tg.MessageEntityBold:
		return "bold"
	case *tg.MessageEntityItalic:
		return "italic"
	case *tg.MessageEntityCode:
		return "code"
	case *tg.MessageEntityPre:
		return "pre"
	case *tg.MessageEntityTextURL:
		return "text_link"
	case *tg.MessageEntityURL:
		return "url"
	case *tg.MessageEntityMention:
		return "mention"
	case *tg.MessageEntityMentionName:
		return "text_mention"
	case *tg.MessageEntityHashtag:
		return "hashtag"
	case *tg.MessageEntityBlockquote:
		return "blockquote"
	default:
		return "unknown"
	}
}

func convertMedia(media tg.MessageMediaClass) []domain.MediaRef {
	switch t := media.(type) {
	case *tg.MessageMediaPhoto:
		if ph, ok := t.Photo.(*tg.Photo); ok {
			return []domain.MediaRef{{
				Key:     "photo:0",
				Type:    "photo",
				FileRef: base64.StdEncoding.EncodeToString(ph.FileReference),
				Size:    int64(len(ph.FileReference)), // 占位；真实尺寸在下载时刷新
			}}
		}
	case *tg.MessageMediaDocument:
		if doc, ok := t.Document.(*tg.Document); ok {
			ref := domain.MediaRef{
				Key:      "doc:0",
				MimeType: doc.MimeType,
				FileRef:  base64.StdEncoding.EncodeToString(doc.FileReference),
				Size:     doc.Size,
			}
			// 一条文档可能带多个 attributes（GIF = Animated+Video），按优先级定类型
			var isAnim, isSticker, isVoice, isAudio, isVideo, isRound bool
			for _, a := range doc.Attributes {
				switch attr := a.(type) {
				case *tg.DocumentAttributeVideo:
					isVideo = true
					isRound = attr.RoundMessage
					ref.Width, ref.Height = attr.W, attr.H
					ref.Duration = int(attr.Duration)
				case *tg.DocumentAttributeAnimated:
					isAnim = true
				case *tg.DocumentAttributeSticker:
					isSticker = true
				case *tg.DocumentAttributeAudio:
					if attr.Voice {
						isVoice = true
					} else {
						isAudio = true
					}
					ref.Duration = int(attr.Duration)
				case *tg.DocumentAttributeFilename:
					ref.FileName = attr.FileName // 上传重放保真（T3.6 经新鲜引用携带）
				}
			}
			switch {
			case isAnim:
				ref.Type = "animation"
			case isSticker:
				ref.Type = "sticker"
			case isVoice:
				ref.Type = "voice"
			case isAudio:
				ref.Type = "audio"
			case isRound:
				ref.Type = "video_note"
			case isVideo:
				ref.Type = "video"
			default:
				ref.Type = "document"
			}
			return []domain.MediaRef{ref}
		}
	case *tg.MessageMediaWebPage:
		// 网页预览不算媒体（03 §3.2），走纯文本
		return nil
	}
	return nil
}

func unixTime(sec int) time.Time {
	return time.Unix(int64(sec), 0).UTC()
}

func toInt64Slice(s []int) []int64 {
	out := make([]int64, len(s))
	for i, v := range s {
		out[i] = int64(v)
	}
	return out
}

// Recovery 实现 RecoverySink：定向 GetHistory 补抓走同一 canonical 管线
// （幂等靠 messages 唯一键吸收——GATE-1 恢复语义）。
type Recovery struct {
	client *telegram.Client
	peers  storage.PeerStorage
	sink   MessageSink
	lg     *slog.Logger
}

// ChannelGap 对指定频道做定向历史补抓（backfillLimit 防风暴）。
func (r *Recovery) ChannelGap(ctx context.Context, channelID int64) {
	peer, err := r.peers.Find(ctx, storage.PeerKey{Kind: dialogs.Channel, ID: channelID})
	if err != nil {
		if errors.Is(err, storage.ErrPeerNotFound) {
			r.lg.Warn("channel gap 但 peer 未知（无 access hash，无法补抓）", "channel", channelID)
			return
		}
		r.lg.Error("channel gap 补抓：查询 peer 失败", "channel", channelID, "err", err)
		return
	}
	if peer.Channel == nil {
		r.lg.Warn("channel gap 但 peer 非 channel 类型", "channel", channelID)
		return
	}
	r.backfill(ctx, &tg.InputPeerChannel{ChannelID: channelID, AccessHash: peer.Channel.AccessHash})
}

// GlobalGap 对全部已知频道做定向 reconciliation（串行限流）。
func (r *Recovery) GlobalGap(ctx context.Context) {
	r.lg.Warn("global gap：开始全量频道 reconciliation")
	iter, err := r.peers.Iterate(ctx)
	if err != nil {
		r.lg.Error("global gap：遍历 peers 失败", "err", err)
		return
	}
	defer func() { _ = iter.Close() }()
	n := 0
	for iter.Next(ctx) {
		p := iter.Value()
		if p.Channel == nil {
			continue
		}
		r.backfill(ctx, &tg.InputPeerChannel{ChannelID: p.Channel.ID, AccessHash: p.Channel.AccessHash})
		n++
	}
	if err := iter.Err(); err != nil {
		r.lg.Error("global gap：迭代 peers 失败", "err", err)
	}
	r.lg.Warn("global gap：reconciliation 完成", "channels", n)
}

// ChannelInaccessible 标记频道不可用（被踢/删除）并停止补抓——不做无意义重试。
func (r *Recovery) ChannelInaccessible(ctx context.Context, channelID int64) {
	r.lg.Error("频道不可访问（被踢出/已删除）：停止该频道恢复，WebUI degraded（P0 无 UI，以日志为准）",
		"channel", channelID)
}

const backfillLimit = 50

func (r *Recovery) backfill(ctx context.Context, input *tg.InputPeerChannel) {
	res, err := r.client.API().MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  input,
		Limit: backfillLimit,
	})
	if err != nil {
		r.lg.Error("补抓 getHistory 失败", "channel", input.ChannelID, "err", err)
		return
	}
	msgs, ok := res.(*tg.MessagesChannelMessages)
	if !ok {
		r.lg.Warn("补抓响应类型非频道消息", "channel", input.ChannelID, "type", fmt.Sprintf("%T", res))
		return
	}
	backfilled := 0
	for _, mc := range msgs.Messages {
		m, ok := mc.(*tg.Message)
		if !ok {
			continue
		}
		if err := r.sink.OnNew(ctx, ConvertMessage(m)); err != nil {
			r.lg.Error("补抓消息入 canonical 失败", "channel", input.ChannelID, "msg", m.ID, "err", err)
			continue
		}
		backfilled++
	}
	r.lg.Info("channel 补抓完成", "channel", input.ChannelID, "fetched", len(msgs.Messages), "backfilled", backfilled)
}
