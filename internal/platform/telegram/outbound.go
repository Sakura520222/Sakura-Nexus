package telegram

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// randomID 生成 random_id（Telegram 要求非零且调用间唯一；缺省 0 对 Bot 账号
// 直接 400 RANDOM_ID_EMPTY，User 账号虽容忍仍应正确填充——GATE-2 冒烟实证）。
func randomID() int64 {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return time.Now().UnixNano()
	}
	return int64(binary.LittleEndian.Uint64(b[:]))
}

// PeerResolver 将 domain.ChatRef 解析为 MTProto InputPeer（含 access hash——
// 由引擎注入：bot 账号的 peers 查询走 telegram_peers 表，03 §3.8）。
type PeerResolver interface {
	InputPeer(ctx context.Context, ref domain.ChatRef) (tg.InputPeerClass, error)
}

// Outbound 是 Bot 出站的 MTProto 实现（03 §3.3 三态发送；Rich 路由在 T4.3 接入）。
// 通过 Go structural typing 满足 forwarding.Sender 最小接口（01 §2.3）。
type Outbound struct {
	client *telegram.Client
	peers  PeerResolver
}

func NewOutbound(client *telegram.Client, peers PeerResolver) *Outbound {
	return &Outbound{client: client, peers: peers}
}

// tgEntity 将领域 Entity 映射回 gotd 实体（转发复制：原 entities 透传）。
func tgEntity(e domain.Entity) tg.MessageEntityClass {
	switch e.Type {
	case "bold":
		return &tg.MessageEntityBold{Offset: e.Offset, Length: e.Length}
	case "italic":
		return &tg.MessageEntityItalic{Offset: e.Offset, Length: e.Length}
	case "code":
		return &tg.MessageEntityCode{Offset: e.Offset, Length: e.Length}
	case "pre":
		return &tg.MessageEntityPre{Offset: e.Offset, Length: e.Length, Language: ""}
	case "text_link":
		return &tg.MessageEntityTextURL{Offset: e.Offset, Length: e.Length, URL: e.URL}
	case "url":
		return &tg.MessageEntityURL{Offset: e.Offset, Length: e.Length}
	case "mention":
		return &tg.MessageEntityMention{Offset: e.Offset, Length: e.Length}
	case "text_mention":
		return &tg.MessageEntityMentionName{
			Offset: e.Offset, Length: e.Length, UserID: e.UserID,
		}
	case "hashtag":
		return &tg.MessageEntityHashtag{Offset: e.Offset, Length: e.Length}
	case "blockquote":
		return &tg.MessageEntityBlockquote{Offset: e.Offset, Length: e.Length}
	default:
		// 未知类型退化为 plain（保持字符区间存在但不加格式）
		return nil
	}
}

func tgEntities(entities []domain.Entity) []tg.MessageEntityClass {
	if len(entities) == 0 {
		return nil
	}
	out := make([]tg.MessageEntityClass, 0, len(entities))
	for _, e := range entities {
		if te := tgEntity(e); te != nil {
			out = append(out, te)
		}
	}
	return out
}

// textSegment 是 >4096 长文本的切分段（03 §3.3 ①：按 entity 边界切）。
type textSegment struct {
	Text     string
	Entities []domain.Entity
}

const telegramMessageLimit = 4096

// splitLongText 将长文本按 entity 边界切段（纯函数，可测）：
// 优先在实体边界处切；无实体的纯文本按 UTF-8 rune 边界切；单实体超限按实体长度截断。
func splitLongText(text string, entities []domain.Entity, limit int) []textSegment {
	if len([]rune(text)) <= limit && len(entities) == 0 {
		return []textSegment{{Text: text}}
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return []textSegment{{Text: text, Entities: entities}}
	}

	var segs []textSegment
	start := 0
	for start < len(runes) {
		end := start + limit
		if end >= len(runes) {
			end = len(runes)
		} else {
			// 在 (start, end) 内找最后一个实体边界（实体终点 ≤ end）
			lastBoundary := -1
			for _, e := range entities {
				b := e.Offset + e.Length
				if b > start && b <= end {
					lastBoundary = max(lastBoundary, b)
				}
			}
			if lastBoundary > start {
				end = lastBoundary
			}
			// 避免在 UTF-16 代理对中间切：gotd entities 偏移是 UTF-16 语义，
			// 但我们以 rune 处理（Go 字符串是 UTF-8）；对 BMP 外字符的边界
			// 情形按 rune 边界安全兜底。
		}
		segText := string(runes[start:end])
		var segEntities []domain.Entity
		for _, e := range entities {
			s, en := e.Offset, e.Offset+e.Length
			if en <= start || s >= end {
				continue // 与本段无交集
			}
			segEntities = append(segEntities, domain.Entity{
				Type:   e.Type,
				Offset: max(s, start) - start,
				Length: min(en, end) - max(s, start),
				URL:    e.URL,
				UserID: e.UserID,
			})
		}
		segs = append(segs, textSegment{Text: segText, Entities: segEntities})
		start = end
	}
	return segs
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// inputPeer 委托 PeerResolver（引擎注入；basic group 无需 hash 的特判在 resolver 内）。
func (o *Outbound) inputPeer(ctx context.Context, ref domain.ChatRef) (tg.InputPeerClass, error) {
	if o.peers == nil {
		return nil, fmt.Errorf("Outbound 未注入 PeerResolver")
	}
	return o.peers.InputPeer(ctx, ref)
}

// SendText 发送纯文本（entities 透传；>4096 按 entity 边界分段）。
// 返回首段 SentMessage（后续段日志记录，03 §3.3 ①）。
func (o *Outbound) SendText(ctx context.Context, req domain.SendRequest) (domain.SentMessage, error) {
	peer, err := o.inputPeer(ctx, req.Chat)
	if err != nil {
		return domain.SentMessage{}, err
	}
	api := o.client.API()

	segs := splitLongText(req.Text, req.Entities, telegramMessageLimit)
	var first domain.SentMessage
	for i, seg := range segs {
		res, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:      peer,
			Message:   seg.Text,
			Entities:  tgEntities(seg.Entities),
			ReplyTo:   replyHeader(req.ReplyTo),
			RandomID:  randomID(),
			NoWebpage: true,
			Silent:    req.Silent,
		})
		if err != nil {
			return first, fmt.Errorf("发送文本段 %d/%d: %w", i+1, len(segs), err)
		}
		if first.MessageID == 0 {
			first = domain.SentMessage{Chat: req.Chat, MessageID: extractSentMsgID(res)}
		}
	}
	return first, nil
}

func replyHeader(replyTo int64) tg.InputReplyToClass {
	if replyTo == 0 {
		return nil
	}
	return &tg.InputReplyToMessage{ReplyToMsgID: int(replyTo)}
}

func extractSentMsgID(upd tg.UpdatesClass) int64 {
	switch u := upd.(type) {
	case *tg.UpdateShortSentMessage:
		return int64(u.ID)
	case *tg.Updates:
		for _, e := range u.Updates {
			if nm, ok := e.(*tg.UpdateMessageID); ok {
				return int64(nm.ID)
			}
		}
	}
	return 0
}

// SendFiles 发送媒体（本地临时文件列表；相册整体重建，03 §3.3 ②）。
// 每个文件先经 uploader 上传得到 InputMedia，再多文件 sendMultiMedia / 单文件 sendMedia。
// files 用 domain.LocalFile（forwarding.Sender 消费接口共用类型，01 §2.3）。
func (o *Outbound) SendFiles(ctx context.Context, req domain.SendRequest, files []domain.LocalFile) (domain.SentMessage, error) {
	if len(files) == 0 {
		return domain.SentMessage{}, fmt.Errorf("SendFiles: 空文件列表")
	}
	peer, err := o.inputPeer(ctx, req.Chat)
	if err != nil {
		return domain.SentMessage{}, err
	}

	up := uploader.NewUploader(o.client.API())
	var medias []tg.InputMediaClass
	for i, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			return domain.SentMessage{}, fmt.Errorf("打开临时文件 %s: %w", lf.Path, err)
		}
		file, err := up.Upload(ctx, uploader.NewUpload(lf.Path, f, lf.Meta.Size))
		_ = f.Close()
		if err != nil {
			return domain.SentMessage{}, fmt.Errorf("上传 media[%d]: %w", i, err)
		}
		medias = append(medias, uploadedToInputMedia(file, lf.Meta))
	}

	api := o.client.API()
	if len(medias) == 1 {
		res, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer:     peer,
			Media:    medias[0],
			Message:  req.Caption,
			Entities: tgEntities(req.Entities),
			RandomID: randomID(),
			Silent:   req.Silent,
		})
		if err != nil {
			return domain.SentMessage{}, fmt.Errorf("发送单媒体: %w", err)
		}
		return domain.SentMessage{Chat: req.Chat, MessageID: extractSentMsgID(res)}, nil
	}

	res, err := api.MessagesSendMultiMedia(ctx, &tg.MessagesSendMultiMediaRequest{
		Peer:       peer,
		MultiMedia: singleMedias(registeredMedias(ctx, api, peer, medias), req.Caption, req.Entities),
		Silent:     req.Silent,
	})
	if err != nil {
		return domain.SentMessage{}, fmt.Errorf("发送相册: %w", err)
	}
	return domain.SentMessage{Chat: req.Chat, MessageID: extractSentMsgID(res)}, nil
}

// uploadedToInputMedia 将上传结果转 InputMedia（按媒体类型选择 photo/document）。
func uploadedToInputMedia(f tg.InputFileClass, m domain.MediaRef) tg.InputMediaClass {
	if m.Type == "photo" {
		return &tg.InputMediaUploadedPhoto{File: f}
	}
	attrs := mediaAttributes(m)
	return &tg.InputMediaUploadedDocument{
		File:       f,
		MimeType:   m.MimeType,
		Attributes: attrs,
	}
}

func mediaAttributes(m domain.MediaRef) []tg.DocumentAttributeClass {
	var attrs []tg.DocumentAttributeClass
	switch m.Type {
	case "video", "video_note":
		attrs = append(attrs, &tg.DocumentAttributeVideo{
			W: m.Width, H: m.Height, Duration: float64(m.Duration),
			RoundMessage: m.Type == "video_note",
		})
	case "animation":
		attrs = append(attrs, &tg.DocumentAttributeAnimated{})
	case "audio":
		attrs = append(attrs, &tg.DocumentAttributeAudio{Duration: m.Duration, Voice: false})
	case "voice":
		attrs = append(attrs, &tg.DocumentAttributeAudio{Duration: m.Duration, Voice: true})
	case "sticker":
		attrs = append(attrs, &tg.DocumentAttributeSticker{})
	}
	if m.FileName != "" {
		attrs = append(attrs, &tg.DocumentAttributeFilename{FileName: m.FileName})
	}
	return attrs
}

// registeredMedias 把上传媒体逐个经 messages.uploadMedia 注册为服务端媒体
// （相册 sendMultiMedia 的前置要求）。裸 InputMediaUploadedPhoto* 直送成组
// 会被 400 MEDIA_INVALID，单发送不受影响（gramjs#594 与 GATE-2 冒烟三度实证）。
func registeredMedias(ctx context.Context, api *tg.Client, peer tg.InputPeerClass,
	medias []tg.InputMediaClass,
) []tg.InputMediaClass {
	out := make([]tg.InputMediaClass, 0, len(medias))
	for _, m := range medias {
		up, err := api.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{Peer: peer, Media: m})
		if err != nil {
			// 注册失败不中断整体：该成员保留原样，由 sendMultiMedia 报错统一处置。
			out = append(out, m)
			continue
		}
		reg, err := RegisteredInputMedia(up)
		if err != nil {
			out = append(out, m)
			continue
		}
		out = append(out, reg)
	}
	return out
}

// RegisteredInputMedia 将 messages.uploadMedia 返回的服务端媒体转为相册可用的
// InputMedia（photo→InputMediaPhoto、document→InputMediaDocument；按 ID 引用，
// 服务端免去二次上传处理）。
func RegisteredInputMedia(m tg.MessageMediaClass) (tg.InputMediaClass, error) {
	switch v := m.(type) {
	case *tg.MessageMediaPhoto:
		if ph, ok := v.Photo.(*tg.Photo); ok {
			return &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: ph.ID, AccessHash: ph.AccessHash}}, nil
		}
	case *tg.MessageMediaDocument:
		if d, ok := v.Document.(*tg.Document); ok {
			return &tg.InputMediaDocument{ID: &tg.InputDocument{ID: d.ID, AccessHash: d.AccessHash}}, nil
		}
	}
	return nil, fmt.Errorf("uploadMedia 返回不可注册媒体: %T", m)
}

// singleMedias 构造相册成员（caption 在首条）。
func singleMedias(medias []tg.InputMediaClass, caption string, entities []domain.Entity) []tg.InputSingleMedia {
	out := make([]tg.InputSingleMedia, len(medias))
	for i, m := range medias {
		c := ""
		var ents []tg.MessageEntityClass
		if i == 0 {
			c = caption
			ents = tgEntities(entities)
		}
		out[i] = tg.InputSingleMedia{Media: m, Message: c, Entities: ents, RandomID: randomID()}
	}
	return out
}

// ForwardMessages 原样转发（03 §3.3 ③：copy_mode=forward，前置条件 Bot 可读源）。
func (o *Outbound) ForwardMessages(ctx context.Context, from domain.ChatRef, ids []int64,
	to domain.ChatRef,
) (domain.SentMessage, error) {
	if len(ids) == 0 {
		return domain.SentMessage{}, fmt.Errorf("ForwardMessages: 空 id 列表")
	}
	fromPeer, err := o.inputPeer(ctx, from)
	if err != nil {
		return domain.SentMessage{}, err
	}
	toPeer, err := o.inputPeer(ctx, to)
	if err != nil {
		return domain.SentMessage{}, err
	}
	res, err := o.client.API().MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		FromPeer: fromPeer,
		ID:       idsToIntSlice(ids),
		ToPeer:   toPeer,
	})
	if err != nil {
		return domain.SentMessage{}, fmt.Errorf("转发: %w", err)
	}
	return domain.SentMessage{Chat: to, MessageID: extractSentMsgID(res)}, nil
}

func idsToIntSlice(ids []int64) []int {
	out := make([]int, len(ids))
	for i, v := range ids {
		out[i] = int(v)
	}
	return out
}

// SplitLongText 导出纯函数（engine 的 caption 分段复用 + 测试）。
func SplitLongText(text string, entities []domain.Entity, limit int) []textSegment {
	return splitLongText(text, entities, limit)
}

// TextSegmentText 测试辅助导出（避免直接依赖私有类型断言）。
func (s textSegment) String() string { return strings.TrimSpace(s.Text) }
