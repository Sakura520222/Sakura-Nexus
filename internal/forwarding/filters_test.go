package forwarding

import (
	"testing"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// ---------- BuildFilterView（相册聚合视图） ----------

func albumMsg(id int64, text string, mediaType string, forwarded bool) domain.ChannelMessage {
	m := domain.ChannelMessage{
		Ref: domain.MessageRef{Chat: domain.NewChatRef(domain.PeerChannel, 1), MessageID: id},
	}
	if text != "" {
		m.Text = text
	}
	if mediaType != "" {
		m.Media = []domain.MediaRef{{Type: mediaType}}
	}
	if forwarded {
		m.ForwardFrom = &domain.ForwardHeader{FromTitle: "来源"}
	}
	return m
}

func TestBuildFilterViewAggregates(t *testing.T) {
	msgs := []domain.ChannelMessage{
		albumMsg(1, "首条 caption", "photo", false),
		albumMsg(2, "", "photo", false),
		albumMsg(3, "成员三", "video", true),
	}
	v := BuildFilterView(msgs)

	if v.AggregateText != "首条 caption\n成员三" {
		t.Errorf("聚合文本 = %q", v.AggregateText)
	}
	gotTypes := map[string]bool{}
	for _, t := range v.MediaTypes {
		gotTypes[t] = true
	}
	if !gotTypes["photo"] || !gotTypes["video"] || len(v.MediaTypes) != 2 {
		t.Errorf("媒体并集 = %v", v.MediaTypes)
	}
	if !v.IsForwarded {
		t.Error("任一成员带转发头 → 整组标记 IsForwarded")
	}
}

func TestBuildFilterViewSingle(t *testing.T) {
	v := BuildFilterView([]domain.ChannelMessage{albumMsg(1, "文本", "", false)})
	if v.AggregateText != "文本" || len(v.MediaTypes) != 0 || v.IsForwarded {
		t.Errorf("单条视图: %+v", v)
	}
}

// ---------- MatchSource ----------

func TestMatchSource(t *testing.T) {
	rule := domain.ForwardRule{
		Source:         domain.NewChatRef(domain.PeerChannel, 100),
		SourceUsername: "SrcChan",
	}
	cases := []struct {
		name     string
		chat     domain.ChatRef
		username string
		want     bool
	}{
		{"id 精确命中", domain.NewChatRef(domain.PeerChannel, 100), "", true},
		{"kind 不同不命中", domain.NewChatRef(domain.PeerUser, 100), "", false}, // ID 空间重叠（02 §1.1）
		{"username 忽略大小写与@", domain.NewChatRef(domain.PeerChannel, 999), "@srcchan", true},
		{"username 不同", domain.NewChatRef(domain.PeerChannel, 999), "other", false},
		{"规则无 username 辅助时不匹配", domain.NewChatRef(domain.PeerChannel, 999), "srcchan", true},
		{"规则 username 空", domain.ForwardRule{Source: rule.Source}.Source, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MatchSource(rule, c.chat, c.username); got != c.want {
				t.Errorf("MatchSource = %v 期望 %v", got, c.want)
			}
		})
	}
}

// ---------- ShouldForward 过滤链 ----------

func baseRule() domain.ForwardRule {
	return domain.ForwardRule{
		Source: domain.NewChatRef(domain.PeerChannel, 100),
		Target: domain.NewChatRef(domain.PeerChannel, 200),
	}
}

func TestFilterForwardOriginalOnly(t *testing.T) {
	rule := baseRule()
	rule.ForwardOriginalOnly = true

	if ok, reason := ShouldForward(rule, FilterView{AggregateText: "x"}); !ok || reason != "" {
		t.Errorf("原创消息应通过: ok=%v reason=%s", ok, reason)
	}
	if ok, _ := ShouldForward(rule, FilterView{AggregateText: "x", IsForwarded: true}); ok {
		t.Error("二手转发应被拒")
	}
}

func TestFilterKeywords(t *testing.T) {
	rule := baseRule()
	rule.Keywords = []string{"流萤", "Firefly"}

	// 子串、大小写不敏感、任一命中
	if ok, _ := ShouldForward(rule, FilterView{AggregateText: "新版本流萤立绘"}); !ok {
		t.Error("中文子串应命中")
	}
	if ok, _ := ShouldForward(rule, FilterView{AggregateText: "New FIREFLY kit leaked"}); !ok {
		t.Error("英文大小写不敏感应命中")
	}
	if ok, reason := ShouldForward(rule, FilterView{AggregateText: "无关内容"}); ok {
		t.Errorf("无命中应拒绝: %s", reason)
	}
	// 空 = 全过
	empty := baseRule()
	if ok, _ := ShouldForward(empty, FilterView{AggregateText: "任何"}); !ok {
		t.Error("空关键词应全过")
	}
}

func TestFilterPatterns(t *testing.T) {
	rule := baseRule()
	rule.Patterns = []string{`v4\.[0-9]+`}

	if ok, _ := ShouldForward(rule, FilterView{AggregateText: "beta v4.1 泋试"}); !ok {
		t.Error("正则命中")
	}
	if ok, _ := ShouldForward(rule, FilterView{AggregateText: "v4.x"}); ok {
		t.Error("正则不命中应拒")
	}
	// 坏正则：视为不匹配（不 panic、不吞整链）
	bad := baseRule()
	bad.Patterns = []string{"[invalid"}
	if ok, _ := ShouldForward(bad, FilterView{AggregateText: "任何"}); ok {
		t.Error("仅坏正则应按不匹配处理→拒绝（keywords 空时）")
	}
	mixed := baseRule()
	mixed.Patterns = []string{"[invalid", `ok$`}
	if ok, _ := ShouldForward(mixed, FilterView{AggregateText: "test ok"}); !ok {
		t.Error("坏正则不影响其他 pattern 命中")
	}
}

func TestFilterBlacklists(t *testing.T) {
	rule := baseRule()
	rule.Keywords = []string{"爆料"} // 保证前置通过
	rule.Blacklist = []string{"广告"}
	rule.BlacklistPatterns = []string{`加\s*微信`}

	if ok, _ := ShouldForward(rule, FilterView{AggregateText: "重磅爆料"}); !ok {
		t.Error("未触黑名单应通过")
	}
	if ok, reason := ShouldForward(rule, FilterView{AggregateText: "爆料（含广告）"}); ok {
		t.Errorf("词黑名单应拒: %s", reason)
	}
	if ok, reason := ShouldForward(rule, FilterView{AggregateText: "爆料 加 微信123"}); ok {
		t.Errorf("正则黑名单应拒: %s", reason)
	}
}

func TestFilterMediaTypes(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		msg     []string
		want    bool
	}{
		{"空=全部·媒体", nil, []string{"photo"}, true},
		{"空=全部·纯文本", nil, nil, true},
		{"any 只放行含媒体", []string{"any"}, []string{"sticker"}, true},
		{"any 拒纯文本", []string{"any"}, nil, false},
		{"显式列表·命中", []string{"photo", "video"}, []string{"photo"}, true},
		{"显式列表·未列类型拒", []string{"photo"}, []string{"video"}, false},
		{"并集任一未列→整组拒", []string{"photo"}, []string{"photo", "document"}, false},
		{"纯文本需显式 text", []string{"text"}, nil, true},
		{"纯文本无 text 拒", []string{"photo", "video"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rule := baseRule()
			rule.MediaTypes = c.allowed
			if ok, _ := ShouldForward(rule, FilterView{MediaTypes: c.msg, AggregateText: "x"}); ok != c.want {
				t.Errorf("media_types(%v) msg(%v) = %v 期望 %v", c.allowed, c.msg, ok, c.want)
			}
		})
	}
}

func TestFilterChainOrderFirstRejectionWins(t *testing.T) {
	// 多条件同时拒绝时，按链序返回第一个原因（forward_original_only 优先于 keywords）
	rule := baseRule()
	rule.ForwardOriginalOnly = true
	rule.Keywords = []string{"never"}
	ok, reason := ShouldForward(rule, FilterView{AggregateText: "x", IsForwarded: true})
	if ok || reason != "forward_original_only：消息为二手转发" {
		t.Errorf("链序: ok=%v reason=%q", ok, reason)
	}
}
