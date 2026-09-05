package telegram

import (
	"regexp"
	"strings"
)

// RichBlockKind 是 03 §2.3 block 流的九类块。
type RichBlockKind uint8

const (
	RichHeading   RichBlockKind = iota // # 标题
	RichParagraph                      // 段落（含 --- 分隔线）
	RichListItem                       // 列表项（每项一块，Bot API §9 计入块数）
	RichTable                          // 表格（markdown 管道式 / HTML 式；行计入块数）
	RichCode                           // 围栏代码块
	RichQuote                          // 引用（> 行组；每块计入）
	RichFormula                        // $$..$$ 或 ```math 围栏
	RichFootnote                       // [^x]: 定义
	RichMedia                          // 媒体块（![]() / <img|video|audio> / collage）
)

// String 提供日志友好表示。
func (k RichBlockKind) String() string {
	switch k {
	case RichHeading:
		return "heading"
	case RichParagraph:
		return "paragraph"
	case RichListItem:
		return "list_item"
	case RichTable:
		return "table"
	case RichCode:
		return "code"
	case RichQuote:
		return "quote"
	case RichFormula:
		return "formula"
	case RichFootnote:
		return "footnote"
	case RichMedia:
		return "media"
	default:
		return "unknown"
	}
}

// RichBlock 是 block 流的一个元素。Text 为规范化文本中的原始行（不含块间
// 空行），序列化时按序拼接即还原文档。Count 为协议块计数贡献（Bot API §9：
// 列表项、表格行均计入）；Media 为媒体附件数；Cols 为表格列数；Depth 为
// 嵌套层级（顶层块 = 1）。
type RichBlock struct {
	Kind  RichBlockKind
	Text  string
	Depth int
	Count int
	Media int
	Cols  int
}

var (
	rePipeRow     = regexp.MustCompile(`^\|.*\|$`)
	rePipeDivider = regexp.MustCompile(`^\|(\s*:?-{2,}:?\s*\|)+$`)
	reFootnoteDef = regexp.MustCompile(`^\[\^[^\]]+\]:\s*`)
	reMediaMD     = regexp.MustCompile(`^!\[[^\]]*\]\([^)]+\)`)
	reMediaHTML   = regexp.MustCompile(`(?i)<(img|video|audio)[\s/>]`)
	reMediaCount  = regexp.MustCompile(`(?i)<(img|video|audio)[\s/>]`)
	reHTMLRow     = regexp.MustCompile(`(?i)<tr[\s>]`)
	reHTMLCell    = regexp.MustCompile(`(?i)</t([hd])>`)
	reMathFence   = regexp.MustCompile("^(`{3,}|~{3,})math\\s*$")
)

// ParseRichBlocks 把规范化文本解析为 block 流（03 §2.3）。行级结构解析器，
// 仅识别 Rich Markdown 的块级构造；解析不可判定的行归入段落（validator 的
// 计数与切分语义不受影响）。
func ParseRichBlocks(text string) []RichBlock {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	var blocks []RichBlock
	var para []string // 段落行缓冲

	flushPara := func() {
		if len(para) == 0 {
			return
		}
		blocks = append(blocks, RichBlock{Kind: RichParagraph, Text: strings.Join(para, "\n"), Depth: 1, Count: 1})
		para = nil
	}

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			flushPara()
			continue
		}
		switch {
		case strings.HasPrefix(line, "#") && isRichHeading(line):
			flushPara()
			blocks = append(blocks, RichBlock{Kind: RichHeading, Text: line, Depth: 1, Count: 1})

		case strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~"):
			flushPara()
			kind := RichCode
			if reMathFence.MatchString(line) {
				kind = RichFormula
			}
			fenceMark := line[:3]
			body := []string{line}
			for i++; i < len(lines); i++ {
				body = append(body, lines[i])
				if strings.HasPrefix(lines[i], fenceMark) {
					break
				}
			}
			blocks = append(blocks, RichBlock{Kind: kind, Text: strings.Join(body, "\n"), Depth: 1, Count: 1})

		case strings.HasPrefix(line, "$$"):
			flushPara()
			text := line
			if !strings.HasSuffix(strings.TrimSpace(line), "$$") || strings.TrimSpace(line) == "$$" {
				for i++; i < len(lines); i++ {
					text += "\n" + lines[i]
					if strings.HasSuffix(strings.TrimSpace(lines[i]), "$$") {
						break
					}
				}
			}
			blocks = append(blocks, RichBlock{Kind: RichFormula, Text: text, Depth: 1, Count: 1})

		case rePipeRow.MatchString(line) && i+1 < len(lines) && rePipeDivider.MatchString(lines[i+1]):
			// markdown 管道表格：表头 + 分隔行 + 数据行。
			flushPara()
			body := []string{line, lines[i+1]}
			cols := countPipeCells(line)
			for i += 2; i < len(lines) && rePipeRow.MatchString(lines[i]); i++ {
				body = append(body, lines[i])
			}
			i--
			blocks = append(blocks, RichBlock{
				Kind: RichTable, Text: strings.Join(body, "\n"), Depth: 1,
				Count: len(body) - 1, // 渲染行 = 表头 + 数据（分隔行不计）
				Cols:  cols,
			})

		case strings.HasPrefix(strings.ToLower(line), "<table"):
			// HTML 表格：消费至 </table>，列数取最宽行。
			flushPara()
			body := []string{line}
			for !strings.Contains(strings.ToLower(line), "</table>") && i+1 < len(lines) {
				i++
				line = lines[i]
				body = append(body, line)
			}
			joined := strings.Join(body, "\n")
			blocks = append(blocks, RichBlock{
				Kind: RichTable, Text: joined, Depth: 1,
				Count: len(reHTMLRow.FindAllString(joined, -1)),
				Cols:  htmlTableCols(joined),
			})

		case reMediaMD.MatchString(line):
			flushPara()
			blocks = append(blocks, RichBlock{Kind: RichMedia, Text: line, Depth: 1, Count: 1, Media: 1})

		case reMediaHTML.MatchString(line):
			flushPara()
			n := len(reMediaCount.FindAllString(line, -1))
			// collage/slideshow 容器行：媒体数 = 容器内附件数。
			blocks = append(blocks, RichBlock{Kind: RichMedia, Text: line, Depth: 1, Count: 1, Media: max(n, 1)})

		case strings.HasPrefix(line, ">"):
			flushPara()
			depth := quoteDepth(line)
			quote := []string{line}
			for i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), ">") &&
				quoteDepth(lines[i+1]) == depth {
				i++
				quote = append(quote, lines[i])
			}
			blocks = append(blocks, RichBlock{Kind: RichQuote, Text: strings.Join(quote, "\n"), Depth: depth, Count: 1})

		case isRichListItem(line):
			flushPara()
			item := []string{line}
			// 缩进续行归并到当前项。
			for i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" &&
				!isRichListItem(lines[i+1]) && strings.HasPrefix(lines[i+1], " ") {
				i++
				item = append(item, lines[i])
			}
			blocks = append(blocks, RichBlock{
				Kind: RichListItem, Text: strings.Join(item, "\n"),
				Depth: listItemDepth(line), Count: 1,
			})

		case reFootnoteDef.MatchString(line):
			flushPara()
			foot := []string{line}
			for i+1 < len(lines) && strings.HasPrefix(lines[i+1], " ") {
				i++
				foot = append(foot, lines[i])
			}
			blocks = append(blocks, RichBlock{Kind: RichFootnote, Text: strings.Join(foot, "\n"), Depth: 1, Count: 1})

		default:
			para = append(para, line)
		}
	}
	flushPara()
	return blocks
}

func isRichHeading(line string) bool {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	return i >= 1 && i <= 6 && (i == len(line) || line[i] == ' ')
}

func isRichListItem(line string) bool {
	t := strings.TrimLeft(line, " ")
	if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "+ ") {
		return true
	}
	if len(t) > 2 && t[0] >= '0' && t[0] <= '9' {
		j := 1
		for j < len(t) && t[j] >= '0' && t[j] <= '9' {
			j++
		}
		if j < len(t) && (t[j] == '.' || t[j] == ')') && t[j+1] == ' ' {
			return true
		}
	}
	// 任务清单 `- [ ]` / `- [x]` 已被 "- " 前缀覆盖。
	return false
}

// listItemDepth 列表项嵌套层级：顶层 = 1，每 2 空格缩进加一层（上限防御性
// 收敛到 maxRichDepth+1 供 validator 报错）。
func listItemDepth(line string) int {
	n := 0
	for n < len(line) && line[n] == ' ' {
		n++
	}
	return n/2 + 1
}

func quoteDepth(line string) int {
	n := 0
	for n < len(line) && line[n] == '>' {
		n++
	}
	return max(n, 1)
}

func countPipeCells(row string) int {
	return strings.Count(row, "|") - 1
}

// htmlTableCols 取 HTML 表格最宽行的列数。
func htmlTableCols(table string) int {
	cols := 0
	for _, row := range strings.Split(table, "\n") {
		if n := len(reHTMLCell.FindAllString(row, -1)); n > cols {
			cols = n
		}
	}
	return cols
}
