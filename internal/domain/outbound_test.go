package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSendStyleRoundTrip(t *testing.T) {
	for _, s := range []SendStyle{StyleAuto, StylePlain, StyleRich} {
		got, ok := SendStyleFromString(s.String())
		if !ok || got != s {
			t.Errorf("SendStyle %d String/FromString 往返失败: got %v ok=%v", s, got, ok)
		}
	}
	if _, ok := SendStyleFromString("bogus"); ok {
		t.Error("未知值应返回 ok=false")
	}
}

func TestSendRequestJSONContract(t *testing.T) {
	// 统一出站模型序列化契约：ChatRef 字符串 kind + camelCase + omitempty 零值字段不出现
	req := SendRequest{
		Chat:  NewChatRef(PeerChannel, 100),
		Style: StyleAuto,
		Text:  "hello",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["chat"].(map[string]any)["kind"] != "channel" {
		t.Errorf("chat.kind 序列化不符: %v", m)
	}
	for _, absent := range []string{"content", "entities", "media", "replyTo", "markup", "silent"} {
		if _, ok := m[absent]; ok {
			t.Errorf("零值字段 %q 不应序列化: %s", absent, b)
		}
	}
}

func TestMessageContentStandalone(t *testing.T) {
	// MessageContent 可脱离 Telegram 上下文使用（WebUI 复用 AIResponse 的基础）
	c := MessageContent{Text: "总结内容", MediaRefs: []MediaRef{{Key: "photo:0", Type: "photo"}}}
	if c.Text == "" || len(c.MediaRefs) != 1 {
		t.Errorf("MessageContent: %+v", c)
	}
}

func TestAIResponseNoTelegramTypes(t *testing.T) {
	// AIResponse 契约：纯通用类型（无 Telegram 特定字段）——序列化即 WebUI 可消费
	r := AIResponse{
		Text:     "回答",
		Metadata: map[string]any{"model": "deepseek-chat"},
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"model":"deepseek-chat"`) {
		t.Errorf("AIResponse 序列化: %s", b)
	}
}

func TestKeyboardStructure(t *testing.T) {
	k := &Keyboard{Rows: [][]KeyboardButton{
		{{Text: "申请总结", URL: "https://t.me/bot?start=sum"}},
	}}
	if len(k.Rows) != 1 || k.Rows[0][0].URL == "" {
		t.Errorf("Keyboard: %+v", k)
	}
}
