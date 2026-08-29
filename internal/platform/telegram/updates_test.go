package telegram

import (
	"testing"

	"github.com/gotd/td/tg"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// TestConvertMessage 覆盖 tg.Message → domain.ChannelMessage 映射要点
// （peer kind、grouped/thread/sender/转发头、媒体分类、实体）。
func TestConvertMessage(t *testing.T) {
	m := &tg.Message{
		ID:       42,
		PeerID:   &tg.PeerChannel{ChannelID: 100200},
		Message:  "内容",
		Date:     1700000000,
		EditDate: 1700000100,
		GroupedID: func() int64 {
			return 777
		}(),
		FromID: &tg.PeerUser{UserID: 555},
		ReplyTo: &tg.MessageReplyHeader{
			ReplyToTopID: 9001,
		},
		FwdFrom: tg.MessageFwdHeader{
			FromName: "来源频道",
		},
		Media: &tg.MessageMediaPhoto{Photo: &tg.Photo{
			ID:            1,
			FileReference: []byte("ref"),
		}},
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityBold{Offset: 0, Length: 2},
		},
	}
	m.SetFwdFrom(tg.MessageFwdHeader{FromName: "来源频道"})

	got := ConvertMessage(m)

	if got.Ref.Chat.Kind != domain.PeerChannel || got.Ref.Chat.ID != 100200 || got.Ref.MessageID != 42 {
		t.Errorf("Ref = %+v", got.Ref)
	}
	if got.SourceType != "channel_message" {
		t.Errorf("SourceType = %s", got.SourceType)
	}
	if got.GroupedID != 777 {
		t.Errorf("GroupedID = %d", got.GroupedID)
	}
	if got.ThreadTopID != 9001 {
		t.Errorf("ThreadTopID = %d", got.ThreadTopID)
	}
	if got.SenderUserID != 555 {
		t.Errorf("SenderUserID = %d", got.SenderUserID)
	}
	if got.ForwardFrom == nil || got.ForwardFrom.FromTitle != "来源频道" {
		t.Errorf("ForwardFrom = %+v（非空=二手转发标记）", got.ForwardFrom)
	}
	if len(got.Media) != 1 || got.Media[0].Type != "photo" || got.Media[0].Key != "photo:0" {
		t.Errorf("Media = %+v", got.Media)
	}
	if len(got.Entities) != 1 || got.Entities[0].Type != "bold" || got.Entities[0].Length != 2 {
		t.Errorf("Entities = %+v", got.Entities)
	}
	if got.PublishedAt.IsZero() || got.EditedAt == nil {
		t.Errorf("时间四件套: published=%v edited=%v", got.PublishedAt, got.EditedAt)
	}
}

func TestConvertMessageDocumentTypes(t *testing.T) {
	cases := []struct {
		name  string
		attrs []tg.DocumentAttributeClass
		want  string
	}{
		{"video", []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{W: 1, H: 1, Duration: 3}}, "video"},
		{"round", []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{RoundMessage: true}}, "video_note"},
		{"gif", []tg.DocumentAttributeClass{&tg.DocumentAttributeAnimated{}, &tg.DocumentAttributeVideo{}}, "animation"},
		{"sticker", []tg.DocumentAttributeClass{&tg.DocumentAttributeSticker{}}, "sticker"},
		{"voice", []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{Voice: true}}, "voice"},
		{"song", []tg.DocumentAttributeClass{&tg.DocumentAttributeAudio{}}, "audio"},
		{"plain", nil, "document"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := &tg.Message{
				ID:     1,
				PeerID: &tg.PeerChannel{ChannelID: 1},
				Media: &tg.MessageMediaDocument{Document: &tg.Document{
					ID:            1,
					MimeType:      "application/octet-stream",
					FileReference: []byte("r"),
					Attributes:    c.attrs,
				}},
			}
			got := ConvertMessage(m)
			if len(got.Media) != 1 || got.Media[0].Type != c.want {
				t.Errorf("type = %+v 期望 %s", got.Media, c.want)
			}
		})
	}
}

func TestChatFromPeer(t *testing.T) {
	cases := []struct {
		peer tg.PeerClass
		want domain.PeerKind
	}{
		{&tg.PeerUser{UserID: 1}, domain.PeerUser},
		{&tg.PeerChat{ChatID: 2}, domain.PeerChat},
		{&tg.PeerChannel{ChannelID: 3}, domain.PeerChannel},
	}
	for _, c := range cases {
		if got := chatFromPeer(c.peer); got.Kind != c.want {
			t.Errorf("chatFromPeer(%T) kind = %v 期望 %v", c.peer, got.Kind, c.want)
		}
	}
}
