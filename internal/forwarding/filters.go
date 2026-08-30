// Package forwarding 是转发领域：engine、filters、相册聚合、规则模型、
// 消费者侧最小接口。设计：docs/design/03-telegram-and-forwarding.md §3。
package forwarding

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// FilterView 是过滤链的输入视图：单条消息即其自身；相册由引擎聚合为
// 「聚合文本 + 媒体类型并集」后传入（03 §1.6 R3.1：整组判定，非首条判定）。
type FilterView struct {
	AggregateText string   // 相册聚合文本（caption 拼接）；单条即 Text
	MediaTypes    []string // 媒体类型并集；纯文本为空
	IsForwarded   bool     // 带 forward 头（forward_original_only 判定）
}

// BuildFilterView 聚合一组消息（单条消息构造单元素视图）。
func BuildFilterView(msgs []domain.ChannelMessage) FilterView {
	if len(msgs) == 0 {
		return FilterView{}
	}
	var texts []string
	types := map[string]struct{}{}
	forwarded := false
	for _, m := range msgs {
		if m.Text != "" {
			texts = append(texts, m.Text)
		}
		for _, media := range m.Media {
			types[media.Type] = struct{}{}
		}
		if m.ForwardFrom != nil {
			forwarded = true
		}
	}
	mediaTypes := make([]string, 0, len(types))
	for t := range types {
		mediaTypes = append(mediaTypes, t)
	}
	return FilterView{
		AggregateText: strings.Join(texts, "\n"),
		MediaTypes:    mediaTypes,
		IsForwarded:   forwarded,
	}
}

// MatchSource 判定消息来源是否命中规则（03 §3.1：ChatRef{kind,id} 精确优先，
// username 归一化辅助次之）。
func MatchSource(rule domain.ForwardRule, chat domain.ChatRef, username string) bool {
	if chat.Kind == rule.Source.Kind && chat.ID == rule.Source.ID {
		return true
	}
	if username == "" || rule.SourceUsername == "" {
		return false
	}
	return strings.EqualFold(strings.TrimPrefix(username, "@"), strings.TrimPrefix(rule.SourceUsername, "@"))
}

// ShouldForward 执行纯函数过滤链（顺序固定，03 §3.2 的 ④→⑧ 段；
// 频道校验/去重/AI 改写在引擎层）。返回 (通过, 拒绝原因)。
func ShouldForward(rule domain.ForwardRule, v FilterView) (bool, string) {
	if rule.ForwardOriginalOnly && v.IsForwarded {
		return false, "forward_original_only：消息为二手转发"
	}
	if !checkKeywords(v.AggregateText, rule.Keywords) {
		return false, "keywords：无关键词命中"
	}
	if !checkPatterns(v.AggregateText, rule.Patterns) {
		return false, "patterns：无正则命中"
	}
	if reason, hit := checkBlacklists(v.AggregateText, rule.Blacklist, rule.BlacklistPatterns); hit {
		return false, reason
	}
	if !checkMediaTypes(v.MediaTypes, rule.MediaTypes) {
		return false, fmt.Sprintf("media_types：媒体类型 %v 不在允许列表 %v", v.MediaTypes, rule.MediaTypes)
	}
	return true, ""
}

// checkKeywords：空=过；子串、大小写不敏感、任一命中（03 §3.2 ⑤）。
func checkKeywords(text string, keywords []string) bool {
	if len(keywords) == 0 {
		return true
	}
	lower := strings.ToLower(text)
	for _, kw := range keywords {
		if kw != "" && strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// checkPatterns：空=过；正则 re.search 任一命中；**编译失败的正则视为不匹配**
// （坏正则记为该条不命中——不因配置错误吞掉整条消息，03 §3.2 ⑥）。
func checkPatterns(text string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			continue // 坏正则：按不匹配处理（WebUI 保存时应校验；此处为运行期兜底）
		}
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// checkBlacklists：词与正则黑名单，任一命中即拒绝。
func checkBlacklists(text string, words, patterns []string) (string, bool) {
	lower := strings.ToLower(text)
	for _, w := range words {
		if w != "" && strings.Contains(lower, strings.ToLower(w)) {
			return fmt.Sprintf("blacklist：命中黑名单词 %q", w), true
		}
	}
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		if re.MatchString(text) {
			return fmt.Sprintf("blacklist_patterns：命中黑名单正则 %q", p), true
		}
	}
	return "", false
}

// checkMediaTypes：空=全部；any=含任意媒体即过（纯文本不过）；
// 纯文本消息当 media_types 非空且不含 text 时被拒（03 §3.2 ⑦）。
func checkMediaTypes(msgTypes, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	allowedSet := map[string]bool{}
	for _, a := range allowed {
		allowedSet[strings.ToLower(a)] = true
	}
	if allowedSet["any"] {
		return len(msgTypes) > 0 // any=只放行含媒体的消息
	}
	if len(msgTypes) == 0 {
		return allowedSet["text"] // 纯文本：仅显式允许 text 时通过
	}
	for _, t := range msgTypes {
		if !allowedSet[strings.ToLower(t)] {
			return false // 并集中任一类型不在允许列表 → 拒（整组判定）
		}
	}
	return true
}
