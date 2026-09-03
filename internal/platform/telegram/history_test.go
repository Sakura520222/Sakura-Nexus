package telegram

import (
	"testing"

	"github.com/gotd/td/tg"
)

func TestHistoryMessagesFromAllContainerKinds(t *testing.T) {
	msgs := []tg.MessageClass{&tg.Message{ID: 1}, &tg.Message{ID: 2}}
	cases := []tg.MessagesMessagesClass{
		&tg.MessagesMessages{Messages: msgs},
		&tg.MessagesMessagesSlice{Messages: msgs, Count: 2},
		&tg.MessagesChannelMessages{Messages: msgs, Count: 2},
	}
	for i, c := range cases {
		got, ok := historyMessages(c)
		if !ok || len(got) != 2 {
			t.Fatalf("[%d] 应提取 2 条: ok=%v n=%d", i, ok, len(got))
		}
	}
	if _, ok := historyMessages(&tg.MessagesMessagesNotModified{}); ok {
		t.Error("NotModified 应提取失败")
	}
}

func TestHistoryConvertsAndFiltersService(t *testing.T) {
	raw := []tg.MessageClass{
		&tg.Message{ID: 5, Message: "hello", PeerID: &tg.PeerChannel{ChannelID: 100}},
		&tg.MessageService{ID: 6}, // 服务消息不算转发内容
	}
	out := convertHistory(raw)
	if len(out) != 1 || out[0].Ref.MessageID != 5 || out[0].Text != "hello" {
		t.Fatalf("应仅保留 1 条普通消息: %+v", out)
	}
	if out[0].Ref.Chat.Kind.String() != "channel" || out[0].Ref.Chat.ID != 100 {
		t.Errorf("Chat 映射不符: %+v", out[0].Ref.Chat)
	}
}
