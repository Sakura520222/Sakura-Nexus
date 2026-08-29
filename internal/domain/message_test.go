package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPeerKindRoundTrip(t *testing.T) {
	for _, k := range []PeerKind{PeerUser, PeerChat, PeerChannel} {
		got, ok := PeerKindFromString(k.String())
		if !ok || got != k {
			t.Errorf("PeerKind %d String/FromString 往返失败: got %v ok=%v", k, got, ok)
		}
	}
	if _, ok := PeerKindFromString("bogus"); ok {
		t.Error("未知字符串应返回 ok=false")
	}
}

func TestChatRefString(t *testing.T) {
	c := NewChatRef(PeerChannel, 12345)
	if c.String() != "channel:12345" {
		t.Errorf("String() = %q", c.String())
	}
}

func TestChatRefJSON(t *testing.T) {
	b, err := json.Marshal(NewChatRef(PeerChat, 77))
	if err != nil {
		t.Fatal(err)
	}
	// 约定：kind 为字符串、id 为数字（Telegram ID 在 JSON 里以字符串传输的约定适用于 API DTO；
	// domain 内部 marshaling 仅用于内部日志/调试，保持 Go 原生形态）
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["kind"] != "chat" {
		t.Errorf("kind 序列化不符: %v", m)
	}
}

func TestPeerKindJSON(t *testing.T) {
	b, err := json.Marshal(PeerChannel)
	if err != nil || string(b) != `"channel"` {
		t.Fatalf("Marshal = %s err=%v（应为字符串 \"channel\"）", b, err)
	}
	var k PeerKind
	if err := json.Unmarshal([]byte(`"user"`), &k); err != nil || k != PeerUser {
		t.Fatalf("Unmarshal user: k=%v err=%v", k, err)
	}
	if err := json.Unmarshal([]byte(`"bogus"`), &k); err == nil {
		t.Fatal("未知值应报错（防静默归零）")
	}
}

func TestMessageRefJSON(t *testing.T) {
	ref := MessageRef{Chat: NewChatRef(PeerUser, 42), MessageID: 1001}
	b, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"chat":{"kind":"user","id":42},"messageId":1001}`
	if string(b) != want {
		t.Errorf("MessageRef JSON 不符:\n got  %s\n want %s", b, want)
	}
}

func TestChannelMessageOmitEmpty(t *testing.T) {
	m := ChannelMessage{
		Ref:         MessageRef{Chat: NewChatRef(PeerChannel, 5), MessageID: 9},
		PublishedAt: time.Time{},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// 非相册消息不应出现 groupedId 等零值字段
	for _, field := range []string{`"groupedId"`, `"media"`, `"forwardFrom"`, `"editedAt"`} {
		if strings.Contains(string(b), field) {
			t.Errorf("零值字段 %s 不应出现: %s", field, b)
		}
	}
}

func TestForwardHeaderMarksNonOriginal(t *testing.T) {
	// forward_original_only 过滤依据：ForwardFrom 非空 = 二手转发（03 §3.2）
	original := ChannelMessage{}
	if original.ForwardFrom != nil {
		t.Error("原创消息 ForwardFrom 应为 nil")
	}
	forwarded := ChannelMessage{ForwardFrom: &ForwardHeader{FromChatID: 7}}
	if forwarded.ForwardFrom == nil {
		t.Error("转发消息 ForwardFrom 应非 nil")
	}
}
