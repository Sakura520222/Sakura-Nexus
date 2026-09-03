package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// limitedWriter 包装目标 Writer，越过剩余配额即报 domain.ErrMediaTooLarge
// （流式大小硬保护，03 §3.9——声明尺寸不可信时的最后防线）。
type limitedWriter struct {
	w         io.Writer
	remaining int64
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.remaining {
		return 0, domain.ErrMediaTooLarge
	}
	n, err := l.w.Write(p)
	l.remaining -= int64(n)
	return n, err
}

// largestPhotoSize 返回 photo 各尺寸档中的最大档（字节/宽高/尺寸类型）。
// PhotoSizeProgressive 取其末端（最大），Stripped/PATH 类（内联小图）跳过。
func largestPhotoSize(ph *tg.Photo) (bytes int64, w, h int, sizeType string, ok bool) {
	for _, s := range ph.Sizes {
		switch sz := s.(type) {
		case *tg.PhotoSize:
			if int64(sz.Size) > bytes {
				bytes, w, h, sizeType, ok = int64(sz.Size), sz.W, sz.H, sz.Type, true
			}
		case *tg.PhotoSizeProgressive:
			if len(sz.Sizes) > 0 {
				last := int64(sz.Sizes[len(sz.Sizes)-1])
				if last > bytes {
					bytes, w, h, sizeType, ok = last, sz.W, sz.H, sz.Type, true
				}
			}
		}
	}
	return
}

// freshMediaRef 从新鲜 tg 媒体产出领域元数据（photo 补真实尺寸——canonical 存储
// 中的 Size 是占位值；document 含文件名/尺寸/时长）。
func freshMediaRef(media tg.MessageMediaClass) (domain.MediaRef, bool) {
	refs := convertMedia(media)
	if len(refs) == 0 {
		return domain.MediaRef{}, false
	}
	ref := refs[0]
	if ph, ok := media.(*tg.MessageMediaPhoto); ok {
		if p, isPhoto := ph.Photo.(*tg.Photo); isPhoto {
			if bytes, w, h, _, ok := largestPhotoSize(p); ok {
				ref.Size, ref.Width, ref.Height = bytes, w, h
			}
		}
	}
	return ref, true
}

// mediaRefAndLocation 同时产出新鲜元数据与下载定位（ID/AccessHash/FileReference
// 仅存在于 tg 媒体内，domain.MediaRef 不携带——每次下载前重取，见 MediaDownloader）。
func mediaRefAndLocation(media tg.MessageMediaClass) (domain.MediaRef, tg.InputFileLocationClass, bool) {
	ref, ok := freshMediaRef(media)
	if !ok {
		return domain.MediaRef{}, nil, false
	}
	switch t := media.(type) {
	case *tg.MessageMediaPhoto:
		if ph, isPhoto := t.Photo.(*tg.Photo); isPhoto {
			_, _, _, sizeType, szOk := largestPhotoSize(ph)
			if !szOk {
				return domain.MediaRef{}, nil, false
			}
			return ref, &tg.InputPhotoFileLocation{
				ID:            ph.ID,
				AccessHash:    ph.AccessHash,
				FileReference: ph.FileReference,
				ThumbSize:     sizeType,
			}, true
		}
	case *tg.MessageMediaDocument:
		if doc, isDoc := t.Document.(*tg.Document); isDoc {
			return ref, &tg.InputDocumentFileLocation{
				ID:            doc.ID,
				AccessHash:    doc.AccessHash,
				FileReference: doc.FileReference,
			}, true
		}
	}
	return domain.MediaRef{}, nil, false
}

// extractMessage 从 get_messages 响应中按 ID 提取消息（三种容器形态 + NotModified）。
func extractMessage(class tg.MessagesMessagesClass, id int64) (*tg.Message, bool) {
	var msgs []tg.MessageClass
	switch c := class.(type) {
	case *tg.MessagesMessages:
		msgs = c.Messages
	case *tg.MessagesMessagesSlice:
		msgs = c.Messages
	case *tg.MessagesChannelMessages:
		msgs = c.Messages
	default:
		return nil, false // NotModified 等
	}
	for _, mc := range msgs {
		if m, ok := mc.(*tg.Message); ok && int64(m.ID) == id {
			return m, true
		}
	}
	return nil, false
}

// MediaDownloader 是 User 侧媒体下载器（03 §3.3 ②/§3.9；实现 forwarding.MediaDownloader）。
//
// 引用策略（§1.5）：domain.MediaRef 只缓存 file_reference（无 ID/AccessHash，构造
// 下载定位不充分），且引用会过期——因此每次下载先经 get_messages 取新鲜媒体，
// 天然规避 FILEREF_INVALID；流式中途失效（极端窗口）时再重取一次。
type MediaDownloader struct {
	user     *UserClient
	peers    PeerResolver
	maxBytes int64
	log      *slog.Logger
}

// NewMediaDownloader 构造下载器；maxBytes 为流式硬上限（建议与 settings 的
// media_max_size 一致；≤0 表示不限）。
func NewMediaDownloader(user *UserClient, peers PeerResolver, maxBytes int64, log *slog.Logger) *MediaDownloader {
	if log == nil {
		log = slog.Default()
	}
	return &MediaDownloader{user: user, peers: peers, maxBytes: maxBytes, log: log}
}

// DownloadMedia 取新鲜引用并流式写入 dest；返回刷新后的 MediaRef（真实尺寸/文件名）。
func (d *MediaDownloader) DownloadMedia(ctx context.Context, m domain.ChannelMessage,
	media domain.MediaRef, dest string,
) (domain.MediaRef, error) {
	ref, loc, err := d.locateFresh(ctx, m)
	if err != nil {
		return domain.MediaRef{}, err
	}
	if err := d.stream(ctx, loc, dest); err != nil {
		if tgerr.Is(err, "FILE_REFERENCE_INVALID") { // 刚取的引用在下载窗口内失效：重取一次
			d.log.Warn("file_reference 失效，重取后重试", "msg", m.Ref.MessageID)
			ref2, loc2, err2 := d.locateFresh(ctx, m)
			if err2 != nil {
				return domain.MediaRef{}, err2
			}
			ref, loc = ref2, loc2
			if err := d.stream(ctx, loc, dest); err != nil {
				return domain.MediaRef{}, err
			}
		} else {
			return domain.MediaRef{}, err
		}
	}
	return ref, nil
}

// locateFresh 经 get_messages 重取消息并产出（新鲜元数据, 下载定位）。
func (d *MediaDownloader) locateFresh(ctx context.Context, m domain.ChannelMessage) (domain.MediaRef, tg.InputFileLocationClass, error) {
	if d.peers == nil {
		return domain.MediaRef{}, nil, fmt.Errorf("MediaDownloader 未注入 PeerResolver")
	}
	peer, err := d.peers.InputPeer(ctx, m.Ref.Chat)
	if err != nil {
		return domain.MediaRef{}, nil, fmt.Errorf("解析源 peer: %w", err)
	}
	api := d.user.Raw().API()
	msgIDs := []tg.InputMessageClass{&tg.InputMessageID{ID: int(m.Ref.MessageID)}}
	var res tg.MessagesMessagesClass
	switch m.Ref.Chat.Kind {
	case domain.PeerChannel:
		chPeer, ok := peer.(*tg.InputPeerChannel)
		if !ok {
			return domain.MediaRef{}, nil, fmt.Errorf("channel chat 应解析为 InputPeerChannel，得到 %T", peer)
		}
		res, err = api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: chPeer.ChannelID, AccessHash: chPeer.AccessHash},
			ID:      msgIDs,
		})
	default:
		res, err = api.MessagesGetMessages(ctx, msgIDs)
	}
	if err != nil {
		return domain.MediaRef{}, nil, fmt.Errorf("get_messages msg=%d: %w", m.Ref.MessageID, err)
	}
	msg, ok := extractMessage(res, m.Ref.MessageID)
	if !ok || msg.Media == nil {
		return domain.MediaRef{}, nil, fmt.Errorf("源消息无媒体（可能已删除）: msg=%d", m.Ref.MessageID)
	}
	ref, loc, ok := mediaRefAndLocation(msg.Media)
	if !ok {
		return domain.MediaRef{}, nil, fmt.Errorf("不支持的媒体类型: msg=%d %T", m.Ref.MessageID, msg.Media)
	}
	return ref, loc, nil
}

// stream 将 loc 流式写入 dest（经 limitedWriter 硬限流）。
func (d *MediaDownloader) stream(ctx context.Context, loc tg.InputFileLocationClass, dest string) error {
	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("创建临时文件 %s: %w", dest, err)
	}
	defer func() { _ = f.Close() }()
	var w io.Writer = f
	if d.maxBytes > 0 {
		w = &limitedWriter{w: f, remaining: d.maxBytes}
	}
	if _, err := downloader.NewDownloader().Download(d.user.Raw().API(), loc).Stream(ctx, w); err != nil {
		if errors.Is(err, domain.ErrMediaTooLarge) {
			return fmt.Errorf("%w（流式写入越过上限）", domain.ErrMediaTooLarge)
		}
		return fmt.Errorf("流式下载: %w", err)
	}
	return nil
}
