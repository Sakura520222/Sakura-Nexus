package telegram

import (
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

func TestSplitLongTextShort(t *testing.T) {
	segs := SplitLongText("短文本", nil, 4096)
	if len(segs) != 1 || segs[0].Text != "短文本" {
		t.Errorf("短文本应单段: %+v", segs)
	}
}

func TestSplitLongTextByRuneBoundary(t *testing.T) {
	// 5000 rune 纯文本 → 按上限切
	long := strings.Repeat("字", 5000)
	segs := SplitLongText(long, nil, 2000)
	if len(segs) != 3 {
		t.Fatalf("段数 = %d 期望 3", len(segs))
	}
	for i, s := range segs {
		want := 2000
		if i == 2 {
			want = 1000
		}
		if len([]rune(s.Text)) != want {
			t.Errorf("段 %d 长度 = %d 期望 %d", i, len([]rune(s.Text)), want)
		}
	}
}

func TestSplitLongTextPrefersEntityBoundary(t *testing.T) {
	// 3000 rune；实体 [1000, 1800) 完整落在首段上限内。
	// 切点应落在实体终点 1800 而非裸上限 2000（不切破实体）。
	long := strings.Repeat("a", 3000)
	entities := []domain.Entity{{Type: "bold", Offset: 1000, Length: 800}}
	segs := SplitLongText(long, entities, 2000)
	if len(segs) != 2 {
		t.Fatalf("段数 = %d 期望 2", len(segs))
	}
	if got := len(segs[0].Text); got != 1800 {
		t.Errorf("首段应在实体终点 1800 切，实际 %d", got)
	}
	// 首段携带完整实体（起点不变——段起点为 0）
	if len(segs[0].Entities) != 1 || segs[0].Entities[0].Offset != 1000 || segs[0].Entities[0].Length != 800 {
		t.Errorf("首段实体: %+v", segs[0].Entities)
	}
	// 次段无实体
	if len(segs[1].Entities) != 0 || len(segs[1].Text) != 1200 {
		t.Errorf("次段: text=%d entities=%v", len(segs[1].Text), segs[1].Entities)
	}
}

func TestSplitLongTextEntitySpansSegments(t *testing.T) {
	// 实体跨段 [1900, 2100)，切点 2000 → 段1 带 [1900,2000)，段2 带 [0,100)
	long := strings.Repeat("b", 3000)
	entities := []domain.Entity{{Type: "italic", Offset: 1900, Length: 200}}
	segs := SplitLongText(long, entities, 2000)
	if len(segs) < 2 {
		t.Fatalf("段数 = %d", len(segs))
	}
	// 实体终点 2100 > 2000：不作为边界（边界必须是完整实体终点 ≤ 上限）
	// 段1 切在 2000（裸上限），实体被切分
	if len(segs[0].Entities) != 1 || segs[0].Entities[0].Offset != 1900 || segs[0].Entities[0].Length != 100 {
		t.Errorf("段1 实体: %+v", segs[0].Entities)
	}
	if len(segs[1].Entities) != 1 || segs[1].Entities[0].Offset != 0 || segs[1].Entities[0].Length != 100 {
		t.Errorf("段2 实体: %+v", segs[1].Entities)
	}
}

func TestTgEntitiesMapping(t *testing.T) {
	entities := []domain.Entity{
		{Type: "bold", Offset: 0, Length: 2},
		{Type: "text_link", Offset: 3, Length: 4, URL: "https://example.com"},
		{Type: "text_mention", Offset: 8, Length: 2, UserID: 42},
		{Type: "unknown_type", Offset: 10, Length: 1}, // 退化为 nil 跳过
	}
	got := tgEntities(entities)
	if len(got) != 3 {
		t.Fatalf("映射数 = %d（未知类型应跳过）", len(got))
	}
	if b, ok := got[0].(*tg.MessageEntityBold); !ok || b.Length != 2 {
		t.Errorf("bold: %#v", got[0])
	}
	if l, ok := got[1].(*tg.MessageEntityTextURL); !ok || l.URL != "https://example.com" {
		t.Errorf("text_link: %#v", got[1])
	}
	if m, ok := got[2].(*tg.MessageEntityMentionName); !ok || m.UserID != 42 {
		t.Errorf("text_mention: %#v", got[2])
	}
}

func TestReplyHeader(t *testing.T) {
	if replyHeader(0) != nil {
		t.Error("无回复应为 nil")
	}
	h := replyHeader(12345)
	if r, ok := h.(*tg.InputReplyToMessage); !ok || r.ReplyToMsgID != 12345 {
		t.Errorf("replyHeader: %#v", h)
	}
}
