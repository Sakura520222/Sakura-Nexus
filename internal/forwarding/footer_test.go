package forwarding

import (
	"testing"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

func TestSourceLinkPublicChannel(t *testing.T) {
	got := SourceLink(domain.NewChatRef(domain.PeerChannel, 111), "news_channel", 42)
	if got != "https://t.me/news_channel/42" {
		t.Errorf("公开频道链接: %s", got)
	}
}

func TestSourceLinkPrivateChannel(t *testing.T) {
	// 私有频道：stripped id = 裸 ID（无 -100 mark，03 §3.6）
	got := SourceLink(domain.NewChatRef(domain.PeerChannel, 123456789), "", 42)
	if got != "https://t.me/c/123456789/42" {
		t.Errorf("私有频道链接: %s", got)
	}
}

func TestRenderFooterReplacesAllPlaceholders(t *testing.T) {
	fc := FooterContext{
		Source:         domain.NewChatRef(domain.PeerChannel, 100),
		SourceUsername: "src",
		SourceTitle:    "源频道",
		Target:         domain.NewChatRef(domain.PeerChannel, 200),
		TargetUsername: "dst",
		TargetTitle:    "目标频道",
		MessageID:      77,
		AssistantBot:   "sakura_bot",
	}
	got := RenderFooter("{source_link}|{source_title}|{target_title}|{source_channel}|{target_channel}|{message_id}|{assistant_bot}", fc)
	want := "https://t.me/src/77|源频道|目标频道|@src|@dst|77|sakura_bot"
	if got != want {
		t.Errorf("占位符渲染:\n got  %s\n want %s", got, want)
	}
}

func TestRenderFooterUnknownPlaceholderLeftAsIs(t *testing.T) {
	got := RenderFooter("keep {unknown} and {source_link}", FooterContext{
		Source: domain.NewChatRef(domain.PeerChannel, 5), MessageID: 1,
	})
	if got != "keep {unknown} and https://t.me/c/5/1" {
		t.Errorf("未知占位符应原样保留: %s", got)
	}
}

func TestRenderFooterFallbacksWhenNoUsername(t *testing.T) {
	fc := FooterContext{
		Source:      domain.NewChatRef(domain.PeerChannel, 100),
		SourceTitle: "只有标题",
		MessageID:   9,
	}
	if got := RenderFooter("{source_channel}", fc); got != "只有标题" {
		t.Errorf("{source_channel} 无 username 应回落 title: %q", got)
	}
	if got := RenderFooter("{source_title}", fc); got != "只有标题" {
		t.Errorf("{source_title}: %q", got)
	}
}

func TestChooseFooterPrecedence(t *testing.T) {
	p := ForwardingParams{ShowDefaultFooter: true}
	r := domain.ForwardRule{}
	if got := ChooseFooter(r, p); got != DefaultFooterTemplate {
		t.Errorf("settings 开 + 无自定义 → 默认模板: %q", got)
	}
	p.ShowDefaultFooter = false
	if got := ChooseFooter(r, p); got != "" {
		t.Errorf("settings 关 + 无自定义 → 无底栏: %q", got)
	}
	r.CustomFooter = "custom {message_id}"
	p.ShowDefaultFooter = false
	if got := ChooseFooter(r, p); got != "custom {message_id}" {
		t.Errorf("自定义优先且不受 settings 开关影响: %q", got)
	}
}
