package forwarding

import (
	"strconv"
	"strings"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// DefaultFooterTemplate 是 show_default_footer 开启且规则无自定义时的默认底栏
// （03 §3.6 未规定默认文案，取最小可用的源链接——进度记录备案）。
const DefaultFooterTemplate = "{source_link}"

// FooterContext 是底栏占位符的取值上下文（03 §3.6 七占位符）。
type FooterContext struct {
	Source         domain.ChatRef
	SourceUsername string
	SourceTitle    string
	Target         domain.ChatRef
	TargetUsername string
	TargetTitle    string
	MessageID      int64
	AssistantBot   string
}

// SourceLink 生成源消息链接：公开频道 t.me/{username}/{msg}；
// 私有频道 t.me/c/{stripped_id}/{msg}（stripped = 裸 ID，无 -100 mark）。
func SourceLink(chat domain.ChatRef, username string, msgID int64) string {
	if u := strings.TrimPrefix(username, "@"); u != "" {
		return "https://t.me/" + u + "/" + strconv.FormatInt(msgID, 10)
	}
	return "https://t.me/c/" + strconv.FormatInt(chat.ID, 10) + "/" + strconv.FormatInt(msgID, 10)
}

// channelHandle 是 {source_channel}/{target_channel} 的取值：
// @username 优先，回落 title，再回落空。
func channelHandle(username, title string) string {
	if u := strings.TrimPrefix(username, "@"); u != "" {
		return "@" + u
	}
	return title
}

// RenderFooter 替换已知占位符；未知占位符原样保留（模板拼写错误可见而非静默丢失）。
func RenderFooter(template string, fc FooterContext) string {
	r := strings.NewReplacer(
		"{source_link}", SourceLink(fc.Source, fc.SourceUsername, fc.MessageID),
		"{source_title}", fc.SourceTitle,
		"{target_title}", fc.TargetTitle,
		"{source_channel}", channelHandle(fc.SourceUsername, fc.SourceTitle),
		"{target_channel}", channelHandle(fc.TargetUsername, fc.TargetTitle),
		"{message_id}", strconv.FormatInt(fc.MessageID, 10),
		"{assistant_bot}", fc.AssistantBot,
	)
	return r.Replace(template)
}

// ChooseFooter 决定底栏模板：rule.CustomFooter 优先（不受全局开关影响）；
// 否则 settings.show_default_footer 开启时用默认模板；否则无底栏。
func ChooseFooter(rule domain.ForwardRule, p ForwardingParams) string {
	if rule.CustomFooter != "" {
		return rule.CustomFooter
	}
	if p.ShowDefaultFooter {
		return DefaultFooterTemplate
	}
	return ""
}
