package telegram

import (
	"regexp"
	"strings"
)

// Rich Markdown renderer（07 §1.1 R3.1 路径修正：renderer 位于 platform/telegram）：
// RichMarkdownNormalizer（03 §2.2）+ validator + block-aware 切分（03 §2.3）。
// 协议限制与块语义依据 docs/telegram-bot-api-10.2-rich-markdown-zh.md（ADR-008 来源）。
//
// 铁律（ADR-008）：LLM formatting instruction ≠ protocol validation——模型
// 「尽量生成正确」，normalizer + validator 保证「一定能发送」。

// richSupportedTags 是官方列出会被解析的 Rich HTML 标签全集（格式文档 §7）。
// 白名单之外的标签按 §2.2「剥离不支持的 HTML/裸标签」移除（保留内嵌文本）。
var richSupportedTags = map[string]bool{
	// 行内
	"b": true, "strong": true, "i": true, "em": true, "u": true, "ins": true,
	"s": true, "strike": true, "del": true, "code": true, "mark": true,
	"sub": true, "sup": true, "tg-spoiler": true,
	// 链接/引用
	"a": true, "tg-reference": true, "tg-emoji": true, "tg-time": true, "tg-math": true,
	// 块
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"p": true, "pre": true, "footer": true, "hr": true, "ul": true, "ol": true,
	"li": true, "input": true, "blockquote": true, "aside": true,
	"details": true, "summary": true,
	// 结构
	"table": true, "caption": true, "tr": true, "th": true, "td": true,
	"tg-math-block": true,
	// 媒体与组合
	"img": true, "video": true, "audio": true, "figure": true, "figcaption": true,
	"cite": true, "tg-map": true, "tg-collage": true, "tg-slideshow": true,
}

var (
	reRichTag       = regexp.MustCompile(`</?([a-zA-Z][a-zA-Z0-9-]*)((?:\s[^<>]*)?)/?>`)
	reHeadClamp     = regexp.MustCompile(`^#{7,}\s*(.*)$`)
	reHeadClosing   = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#+$`)
	reHeadNoSpace   = regexp.MustCompile(`^(#{1,6})([^#\s])`)
	reAngleAutolnk  = regexp.MustCompile(`<(https?://[^<>\s]+)>`)
	reBareURL       = regexp.MustCompile(`https?://[^\s<>()\[\]"']+`)
	reSetextEq      = regexp.MustCompile(`^={3,}$`)
	reSetextDash    = regexp.MustCompile(`^-{3,}$`)
	reFenceOpen     = regexp.MustCompile("^(`{3,}|~{3,})")
	reDangerousHref = regexp.MustCompile(`\[([^\]]*)\]\(\s*(?:javascript|data|vbscript):[^)]*\)`)
)

// NormalizeRichMarkdown 是 RichMarkdownNormalizer（03 §2.2，deterministic 纯函数）：
// ①剥离不支持的 HTML/裸标签 → ②统一标题层级 → ③链接规范化 →
// ④代码块补语言标注 → ⑤空白规整。围栏内容为代码即真理，除开栏语言标注外
// 一律不改写；行内代码 span 内同样不改写。输出幂等。
func NormalizeRichMarkdown(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")

	out := make([]string, 0, len(lines))
	inFence := false
	for i := 0; i < len(lines); i++ {
		line := lines[i]

		if inFence {
			out = append(out, line)
			if reFenceOpen.MatchString(line) {
				inFence = false
			}
			continue
		}
		// ②统一标题层级：Setext（文字行 + ===/--- 下划线）→ ATX。
		if i+1 < len(lines) && richIsPlainText(line) {
			if reSetextEq.MatchString(lines[i+1]) {
				out = append(out, "# "+strings.TrimSpace(line))
				i++
				continue
			}
			if reSetextDash.MatchString(lines[i+1]) {
				out = append(out, "## "+strings.TrimSpace(line))
				i++
				continue
			}
		}
		// 围栏开关：开栏补语言标注（④）；栏内行原样保留。
		if m := reFenceOpen.FindStringSubmatch(line); m != nil {
			info := strings.TrimSpace(strings.TrimPrefix(line, m[1]))
			if info == "" {
				line = m[1] + "text"
			}
			out = append(out, line)
			inFence = true
			continue
		}
		out = append(out, richNormalizeLine(line))
	}

	return richCollapseBlanks(out)
}

// richNormalizeLine 对单行做 ①剥不支持标签、②ATX 标题修补、③链接规范化
// （行内代码 span 内不改写）。
func richNormalizeLine(line string) string {
	line = richHeadings(line)
	return transformOutsideCodeSpans(line, func(s string) string {
		s = richStripTags(s)
		s = reAngleAutolnk.ReplaceAllString(s, "[${1}](${1})")
		s = richBareURLs(s)
		// ③危险 scheme 剥链接留文本（javascript:/data:/vbscript:）。
		s = reDangerousHref.ReplaceAllString(s, "${1}")
		return s
	})
}

// richHeadings 统一 ATX 标题层级：7 级以上收敛到 6 级 → 剥闭合格式尾部 # →
// `#Title` 补空格。
func richHeadings(line string) string {
	if m := reHeadClamp.FindStringSubmatch(line); m != nil {
		line = "###### " + strings.TrimSpace(m[1])
	}
	if m := reHeadClosing.FindStringSubmatch(line); m != nil {
		line = m[1] + " " + strings.TrimSpace(m[2])
	}
	return reHeadNoSpace.ReplaceAllString(line, "$1 $2")
}

// richStripTags 移除白名单之外的 HTML/裸标签（保留内嵌文本）。
func richStripTags(s string) string {
	return reRichTag.ReplaceAllStringFunc(s, func(tag string) string {
		name := strings.ToLower(reRichTag.FindStringSubmatch(tag)[1])
		if richSupportedTags[name] {
			return tag
		}
		return ""
	})
}

// richBareURLs 把裸 URL 转显式链接 [url](url)；已在链接/自动链接内的 URL
// （前缀 `](`、`[`、`<`）跳过，保证幂等。
func richBareURLs(s string) string {
	var b strings.Builder
	for {
		loc := reBareURL.FindStringIndex(s)
		if loc == nil {
			b.WriteString(s)
			return b.String()
		}
		prefix := s[:loc[0]]
		url := s[loc[0]:loc[1]]
		// 前缀为 "](" / "[" / "<"（已是链接）或 "\""（HTML 属性值）的 URL 不改写。
		if strings.HasSuffix(prefix, "](") || strings.HasSuffix(prefix, "[") ||
			strings.HasSuffix(prefix, "<") || strings.HasSuffix(prefix, `"`) {
			b.WriteString(prefix)
			b.WriteString(url)
		} else {
			b.WriteString(prefix)
			b.WriteString("[" + url + "](" + url + ")")
		}
		s = s[loc[1]:]
	}
}

// richIsPlainText 判定可作 Setext 标题文字的行：非空且不以任何块级标记开头。
func richIsPlainText(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	for _, p := range []string{"#", "```", "~~~", ">", "|", "<", "- ", "* ", "+ "} {
		if strings.HasPrefix(t, p) {
			return false
		}
	}
	if len(t) > 1 && t[0] >= '0' && t[0] <= '9' && (t[1] == '.' || t[1] == ')') {
		return false
	}
	return true
}

// transformOutsideCodeSpans 仅对行内代码 span（反引号围合）之外的片段施加 f。
func transformOutsideCodeSpans(s string, f func(string) string) string {
	parts := strings.Split(s, "`")
	for i := 0; i < len(parts); i += 2 {
		parts[i] = f(parts[i])
	}
	return strings.Join(parts, "`")
}

// richCollapseBlanks 实现 ⑤空白规整：剥行尾空白（围栏内除外）、连续空行
// 收敛为一行、去首尾空行。
func richCollapseBlanks(lines []string) string {
	var b strings.Builder
	inFence := false
	prevBlank := true // 抑制首部空行
	for _, line := range lines {
		if reFenceOpen.MatchString(line) {
			inFence = !inFence
			b.WriteString(line)
			b.WriteByte('\n')
			prevBlank = false
			continue
		}
		if inFence {
			b.WriteString(line)
			b.WriteByte('\n')
			continue
		}
		trimmed := strings.TrimRight(line, " \t")
		if trimmed == "" {
			if prevBlank {
				continue
			}
			prevBlank = true
			b.WriteByte('\n')
			continue
		}
		prevBlank = false
		b.WriteString(trimmed)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
