package telegram

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// 03 §2.2 RichMarkdownNormalizer（deterministic）：剥离不支持的 HTML/裸标签 →
// 统一标题层级 → 链接规范化 → 代码块补语言标注 → 空白规整。
// 官方支持标签集见 docs/telegram-bot-api-10.2-rich-markdown-zh.md §7。
func TestNormalizeRichMarkdown(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "剥离不支持标签保留文本",
			in:   "<div> hello <script>alert(1)</script> world </div>",
			want: " hello alert(1) world",
		},
		{
			name: "官方支持标签原样保留",
			in:   "<b>粗体</b> <tg-spoiler>剧透</tg-spoiler> <u>下划线</u>",
			want: "<b>粗体</b> <tg-spoiler>剧透</tg-spoiler> <u>下划线</u>",
		},
		{
			name: "结构/媒体标签原样保留",
			in:   "<table><tr><td>a</td></tr></table>\n<tg-collage><img src=\"https://e/a.jpg\"/></tg-collage>",
			want: "<table><tr><td>a</td></tr></table>\n<tg-collage><img src=\"https://e/a.jpg\"/></tg-collage>",
		},
		{
			name: "统一标题层级：无空格补空格",
			in:   "#Title\n## Sub  ",
			want: "# Title\n## Sub",
		},
		{
			name: "统一标题层级：7 级以上收敛到 6 级",
			in:   "####### 深层标题",
			want: "###### 深层标题",
		},
		{
			name: "统一标题层级：Setext 转 ATX",
			in:   "大标题\n=====\n\n正文\n\n小标题\n-----",
			want: "# 大标题\n\n正文\n\n## 小标题",
		},
		{
			name: "统一标题层级：闭合格式剥尾部 #",
			in:   "## 标题 ##",
			want: "## 标题",
		},
		{
			name: "链接规范化：尖括号自动链接转显式链接",
			in:   "见 <https://example.com/a> 详情",
			want: "见 [https://example.com/a](https://example.com/a) 详情",
		},
		{
			name: "链接规范化：裸 URL 转显式链接",
			in:   "访问 https://example.com/b 即可",
			want: "访问 [https://example.com/b](https://example.com/b) 即可",
		},
		{
			name: "链接规范化：危险 scheme 剥链接留文本",
			in:   "[点我](javascript:pay) 谢谢",
			want: "点我 谢谢",
		},
		{
			name: "代码块补语言标注",
			in:   "```\nprintln(1)\n```",
			want: "```text\nprintln(1)\n```",
		},
		{
			name: "已有语言标注不重复添加",
			in:   "```python\nprint(1)\n```",
			want: "```python\nprint(1)\n```",
		},
		{
			name: "空白规整：CRLF/行尾空白/连续空行/首尾空行",
			in:   "\r\n\r\n第一段\r\n   \r\n\r\n\r\n\r\n第二段  \r\n",
			want: "第一段\n\n第二段",
		},
		{
			name: "围栏内不做规范化（代码即真理）",
			in:   "```\n<div>raw</div>\n# not heading\nhttp://x.example\n```",
			want: "```text\n<div>raw</div>\n# not heading\nhttp://x.example\n```",
		},
		{
			name: "行内代码内的裸 URL 不改写",
			in:   "配置 `http://localhost:8080` 后重启",
			want: "配置 `http://localhost:8080` 后重启",
		},
		{
			name: "组合 golden：完整文档",
			in:   "报告\r\n=====\r\n\n<script>x</script>**状态**：正常 <https://e.io/s>  \n\r\n``` \nraw <b>code</b>\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n",
			want: "# 报告\n\nx**状态**：正常 [https://e.io/s](https://e.io/s)\n\n```text\nraw <b>code</b>\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeRichMarkdown(tc.in)
			if got != tc.want {
				t.Errorf("规范化不符:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestNormalizeRichMarkdownIdempotent(t *testing.T) {
	// deterministic：规范化输出再次规范化必须不变（调用方可安全重入）。
	in := "# 标题\n\n段落 [https://e.io](https://e.io)\n\n```text\ncode\n```"
	if once := NormalizeRichMarkdown(in); once != in {
		t.Errorf("首次规范化即应收敛:\n got: %q\nwant: %q", once, in)
	}
	if twice := NormalizeRichMarkdown(NormalizeRichMarkdown(in)); twice != in {
		t.Errorf("二次规范化应幂等: %q", twice)
	}
}

// 03 §2.3：解析为 block 流（heading/paragraph/list/table/code/quote/formula/
// footnote/media）。Count 语义依 Bot API §9：列表项、表格行均计入块数。
func TestParseRichBlocks(t *testing.T) {
	in := strings.Join([]string{
		"# 标题",
		"",
		"普通段落第一行",
		"续行",
		"",
		"- 项目一",
		"- 项目二",
		"  - 嵌套项",
		"",
		"1. 有序一",
		"2. 有序二",
		"",
		"> 引用一行",
		"> 引用二行",
		">> 深层引用",
		"",
		"| a | b |",
		"|---|---|",
		"| 1 | 2 |",
		"",
		"```python",
		"print(1)",
		"```",
		"",
		"$$E = mc^2$$",
		"",
		"[^1]: 脚注内容",
		"",
		"![](https://e/a.jpg)",
	}, "\n")

	blocks := ParseRichBlocks(in)
	type want struct {
		kind  RichBlockKind
		text  string
		depth int
		count int
		media int
		cols  int
	}
	wants := []want{
		{RichHeading, "# 标题", 1, 1, 0, 0},
		{RichParagraph, "普通段落第一行\n续行", 1, 1, 0, 0},
		{RichListItem, "- 项目一", 1, 1, 0, 0},
		{RichListItem, "- 项目二", 1, 1, 0, 0},
		{RichListItem, "  - 嵌套项", 2, 1, 0, 0},
		{RichListItem, "1. 有序一", 1, 1, 0, 0},
		{RichListItem, "2. 有序二", 1, 1, 0, 0},
		{RichQuote, "> 引用一行\n> 引用二行", 1, 1, 0, 0},
		{RichQuote, ">> 深层引用", 2, 1, 0, 0},
		{RichTable, "| a | b |\n|---|---|\n| 1 | 2 |", 1, 2, 0, 2},
		{RichCode, "```python\nprint(1)\n```", 1, 1, 0, 0},
		{RichFormula, "$$E = mc^2$$", 1, 1, 0, 0},
		{RichFootnote, "[^1]: 脚注内容", 1, 1, 0, 0},
		{RichMedia, "![](https://e/a.jpg)", 1, 1, 1, 0},
	}
	if len(blocks) != len(wants) {
		t.Fatalf("块数不符: got %d want %d\n%+v", len(blocks), len(wants), blocks)
	}
	for i, w := range wants {
		b := blocks[i]
		if b.Kind != w.kind || b.Text != w.text || b.Depth != w.depth ||
			b.Count != w.count || b.Media != w.media || b.Cols != w.cols {
			t.Errorf("块 %d 不符:\n got: {kind=%d text=%q depth=%d count=%d media=%d cols=%d}\nwant: {kind=%d text=%q depth=%d count=%d media=%d cols=%d}",
				i, b.Kind, b.Text, b.Depth, b.Count, b.Media, b.Cols, w.kind, w.text, w.depth, w.count, w.media, w.cols)
		}
	}
}

func TestParseRichBlocksEdges(t *testing.T) {
	t.Run("空输入", func(t *testing.T) {
		if blocks := ParseRichBlocks(""); len(blocks) != 0 {
			t.Errorf("空输入应无块: %+v", blocks)
		}
	})
	t.Run("HTML 表格列数取最宽行", func(t *testing.T) {
		blocks := ParseRichBlocks("<table>\n<tr><td>a</td><td>b</td><td>c</td></tr>\n<tr><td>1</td></tr>\n</table>")
		if len(blocks) != 1 || blocks[0].Kind != RichTable || blocks[0].Cols != 3 || blocks[0].Count != 2 {
			t.Errorf("HTML 表格解析不符: %+v", blocks)
		}
	})
	t.Run("math 围栏计为公式块", func(t *testing.T) {
		blocks := ParseRichBlocks("```math\n\\int_a^b f(x)\\,dx\n```")
		if len(blocks) != 1 || blocks[0].Kind != RichFormula {
			t.Errorf("```math 应为公式块: %+v", blocks)
		}
	})
	t.Run("HTML 媒体行与 collage 多附件", func(t *testing.T) {
		blocks := ParseRichBlocks(strings.Join([]string{
			`<img src="https://e/a.jpg"/>`,
			`<tg-collage><img src="https://e/b.jpg"/><video src="https://e/c.mp4"/></tg-collage>`,
		}, "\n"))
		if len(blocks) != 2 {
			t.Fatalf("应 2 个媒体块: %+v", blocks)
		}
		if blocks[0].Media != 1 || blocks[1].Media != 2 {
			t.Errorf("媒体数不符: %+v", blocks)
		}
	})
	t.Run("列表项续行归并", func(t *testing.T) {
		blocks := ParseRichBlocks("- 首行\n  续行内容\n- 第二项")
		if len(blocks) != 2 || blocks[0].Text != "- 首行\n  续行内容" {
			t.Errorf("续行应归并到当前列表项: %+v", blocks)
		}
	})
}

// RichMessages：validator + block-aware 切分（03 §2.3）。边界集 = 07 §1.1：
// 超限表格、超长代码块、16 层嵌套、50 媒体、32768 字符。
func richMediaDoc(n int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "![](https://e/" + strconv.Itoa(i) + ".jpg)"
	}
	return strings.Join(lines, "\n\n")
}

func richItems(n, size int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "- 项目" + strings.Repeat("x", size)
	}
	return strings.Join(lines, "\n\n")
}

func TestRichMessagesDepthBoundary(t *testing.T) {
	// 16 层嵌套 = 协议上限（Bot API §9）；17 层不可修复 → fallback。
	ok16 := strings.Repeat(">", 16) + " 深层引用"
	msgs, err := RichMessages(ok16)
	if err != nil || len(msgs) != 1 {
		t.Errorf("16 层应可发送: msgs=%d err=%v", len(msgs), err)
	}
	deep17 := strings.Repeat(">", 17) + " 超限"
	if _, err := RichMessages(deep17); !errors.Is(err, ErrRichUnsendable) {
		t.Errorf("17 层应 ErrRichUnsendable，得 %v", err)
	}
}

func TestRichMessagesTableColsBoundary(t *testing.T) {
	mkTable := func(cols int) string {
		header := "|"
		divider := "|"
		for i := 0; i < cols; i++ {
			header += fmt.Sprintf(" c%d |", i)
			divider += "---|"
		}
		return header + "\n" + divider
	}
	if msgs, err := RichMessages(mkTable(20)); err != nil || len(msgs) != 1 {
		t.Errorf("20 列应可发送: msgs=%d err=%v", len(msgs), err)
	}
	if _, err := RichMessages(mkTable(21)); !errors.Is(err, ErrRichUnsendable) {
		t.Errorf("21 列（超限表格）应 ErrRichUnsendable，得 %v", err)
	}
}

func TestRichMessagesMediaBoundary(t *testing.T) {
	// 50 媒体/条：50 → 单条；51 → 两条（50+1）。
	msgs, err := RichMessages(richMediaDoc(50))
	if err != nil || len(msgs) != 1 {
		t.Errorf("50 媒体应单条: msgs=%d err=%v", len(msgs), err)
	}
	msgs, err = RichMessages(richMediaDoc(51))
	if err != nil {
		t.Fatalf("51 媒体应切两条而非报错: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("51 媒体应切两条，得 %d", len(msgs))
	}
	if n := strings.Count(msgs[0], "![]("); n != 50 {
		t.Errorf("第一条应 50 媒体，得 %d", n)
	}
	if n := strings.Count(msgs[1], "![]("); n != 1 {
		t.Errorf("第二条应 1 媒体，得 %d", n)
	}
}

func TestRichMessagesPacksByCharLimit(t *testing.T) {
	// 32768 UTF-8 字符/条：三个 16000 字符段落 → 贪心组装为 2 条
	//（16000+2+16000 = 32002 ≤ 32768，第三段落另起一条）。
	para := strings.Repeat("字", 16000)
	in := strings.Join([]string{para, para, para}, "\n\n")
	msgs, err := RichMessages(in)
	if err != nil {
		t.Fatalf("应可切分: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("16000×3 段落应切 2 条，得 %d", len(msgs))
	}
	for i, m := range msgs {
		if n := utf8.RuneCountInString(m); n > maxRichChars {
			t.Errorf("消息 %d 超字符上限: %d", i, n)
		}
	}
	if utf8.RuneCountInString(msgs[0]) != 16000*2+len("\n\n") {
		t.Errorf("第一条应为两段落贪心组装，得 %d 字符", utf8.RuneCountInString(msgs[0]))
	}
}

func TestRichMessagesPacksByBlockLimit(t *testing.T) {
	// 500 blocks/条（列表项计入）：600 项 → 500 + 100。
	msgs, err := RichMessages(richItems(600, 2))
	if err != nil {
		t.Fatalf("应可切分: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("600 项应切 2 条，得 %d", len(msgs))
	}
	if n := strings.Count(msgs[0], "- 项目"); n != 500 {
		t.Errorf("第一条应 500 块，得 %d", n)
	}
	if n := strings.Count(msgs[1], "- 项目"); n != 100 {
		t.Errorf("第二条应 100 块，得 %d", n)
	}
}

func TestRichMessagesSplitsLongCodeBlock(t *testing.T) {
	// 超长代码块：行级二次切分，逐块重加围栏、保留语言标注；内容行原序保全。
	content := strings.Repeat(strings.Repeat("a", 1000)+"\n", 40) // 40 行 × 1000
	in := "```python\n" + content + "```"
	msgs, err := RichMessages(in)
	if err != nil {
		t.Fatalf("超长代码块应行级切分: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("应切为多条，得 %d", len(msgs))
	}
	var gotLines []string
	for i, m := range msgs {
		if !strings.HasPrefix(m, "```python\n") || !strings.HasSuffix(m, "\n```") {
			t.Fatalf("消息 %d 应重加同语言围栏: %q...", i, m[:min(20, len(m))])
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(m, "```python\n"), "\n```")
		gotLines = append(gotLines, strings.Split(inner, "\n")...)
	}
	want := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if len(gotLines) != len(want) {
		t.Fatalf("内容行数不符: got %d want %d", len(gotLines), len(want))
	}
	for i := range want {
		if gotLines[i] != want[i] {
			t.Fatalf("内容行 %d 原序保全失败", i)
		}
	}
}

func TestRichMessagesSplitsLongParagraphByLines(t *testing.T) {
	// 单段落多行超限 → 行级切分；不按字符硬切（行内不断开）。
	line := strings.Repeat("字", 5000)
	in := strings.Repeat(line+"\n", 10) // 10 行 × 5000 = 50000 字符
	msgs, err := RichMessages(strings.TrimSuffix(in, "\n"))
	if err != nil {
		t.Fatalf("应行级切分: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("50000 字符段落应切多条，得 %d", len(msgs))
	}
	for i, m := range msgs {
		if strings.Contains(m, "\n\n") && i == 0 {
			t.Errorf("切分应沿行边界，不应出现块内空行")
		}
		for _, l := range strings.Split(m, "\n") {
			if utf8.RuneCountInString(l) != 5000 {
				t.Errorf("行必须整行保留，不得字符硬切: %d", utf8.RuneCountInString(l))
			}
		}
	}
}

func TestRichMessagesUnsendableCases(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"单行段落超 32768 且无行边界可切", strings.Repeat("字", 40000)},
		{"表格自身字符超限（表头行超限不可切）", "| " + strings.Repeat("字", 20000) + " | " + strings.Repeat("字", 20000) + " |\n|---|---|"},
		{"围栏内单行超限（行边界可切但单行超限）", "```text\n" + strings.Repeat("字", 40000) + "\n```"},
		{"空内容", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := RichMessages(tc.in); !errors.Is(err, ErrRichUnsendable) {
				t.Errorf("应 ErrRichUnsendable（走 fallback 链），得 %v", err)
			}
		})
	}
}

func TestRichMessagesKitchenSinkGolden(t *testing.T) {
	// 组合 golden：规范化 → 解析 → 切分端到端，内容与结构保全。
	in := strings.Join([]string{
		"# 部署报告",
		"",
		"- [x] 已完成",
		"- [ ] 待验证",
		"",
		"| 组件 | 状态 |",
		"|---|---|",
		"| 引擎 | 正常 |",
		"",
		"```text",
		"systemctl status sakura-nexus",
		"```",
		"",
		"状态：**正常**，耗时 [https://e.io](https://e.io)。",
	}, "\n\n")
	msgs, err := RichMessages(NormalizeRichMarkdown(in))
	if err != nil {
		t.Fatalf("常规文档应单条通过: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("应单条，得 %d", len(msgs))
	}
	m := msgs[0]
	for _, want := range []string{"# 部署报告", "- [x] 已完成", "| 组件 | 状态 |", "```text", "systemctl status sakura-nexus", "**正常**"} {
		if !strings.Contains(m, want) {
			t.Errorf("消息缺内容 %q:\n%s", want, m)
		}
	}
}

func TestRichMessagesSplitsLongTableByRows(t *testing.T) {
	// §2.3「单个 block 自身超限 → 行级二次切分」对表格 = 按行切分：
	// 600 行（60000+ 字符且超 500 blocks）应切为多张合法表，表头逐张重复，
	// 数据行原序保全。单行超限仍不可修复（见 UnsendableCases）。
	header := "| a | b |"
	divider := "|---|---|"
	rows := make([]string, 0, 600)
	for i := 0; i < 600; i++ {
		rows = append(rows, fmt.Sprintf("| r%03d | %s |", i, strings.Repeat("x", 100)))
	}
	in := header + "\n" + divider + "\n" + strings.Join(rows, "\n")

	msgs, err := RichMessages(in)
	if err != nil {
		t.Fatalf("超限表格应行级切分: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("600 行表格应切多张表，得 %d", len(msgs))
	}
	var gotRows []string
	for i, m := range msgs {
		lines := strings.Split(m, "\n")
		if lines[0] != header || lines[1] != divider {
			t.Fatalf("表 %d 应以表头+分隔行开头", i)
		}
		if len(lines)-2 > maxRichBlocks-1 {
			t.Errorf("表 %d 数据行数超上限", i)
		}
		if utf8.RuneCountInString(m) > maxRichChars {
			t.Errorf("表 %d 超字符上限: %d", i, utf8.RuneCountInString(m))
		}
		for _, l := range lines[2:] {
			if l != "" {
				gotRows = append(gotRows, l)
			}
		}
	}
	if len(gotRows) != len(rows) {
		t.Fatalf("数据行数不符: got %d want %d", len(gotRows), len(rows))
	}
	for i := range rows {
		if gotRows[i] != rows[i] {
			t.Fatalf("数据行 %d 原序保全失败", i)
		}
	}
}

func TestRichMessagesSplitsLongHTMLTableByRows(t *testing.T) {
	// HTML 表格同理按 <tr> 行切分。
	var rows []string
	for i := 0; i < 600; i++ {
		rows = append(rows, fmt.Sprintf("<tr><td>r%03d</td><td>%s</td></tr>", i, strings.Repeat("x", 100)))
	}
	in := "<table>\n" + strings.Join(rows, "\n") + "\n</table>"
	msgs, err := RichMessages(in)
	if err != nil {
		t.Fatalf("超长 HTML 表格应行级切分: %v", err)
	}
	if len(msgs) < 2 {
		t.Fatalf("应切多张表，得 %d", len(msgs))
	}
	total := 0
	for _, m := range msgs {
		if !strings.HasPrefix(m, "<table>") || !strings.HasSuffix(m, "</table>") {
			t.Fatalf("每张表应为完整 <table> 包装: %q", m[:min(30, len(m))])
		}
		total += strings.Count(m, "<tr>")
	}
	if total != 600 {
		t.Errorf("<tr> 总数应保全 600，得 %d", total)
	}
}
