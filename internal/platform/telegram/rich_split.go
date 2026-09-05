package telegram

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// 协议硬限制（ADR-008 / Bot API §9）：32,768 UTF-8 字符、500 blocks（列表项、
// 表格行计入）、16 层嵌套、50 媒体附件/条、表格 ≤20 列。
const (
	maxRichChars  = 32768
	maxRichBlocks = 500
	maxRichDepth  = 16
	maxRichMedia  = 50
	maxRichCols   = 20
)

// ErrRichUnsendable 是不可修复超限的 validation error——调用方（Outbound
// 路由）以此触发 fallback 链：Rich → 普通 formatting → 纯文本（03 §2.7）。
var ErrRichUnsendable = errors.New("rich: 内容超限且不可切分")

// RichMessages 对规范化文本做 validator + block-aware 切分（03 §2.3）：
// 按 block 边界贪心组装成多条合法消息；代码块/表格/公式整体优先不切，单个
// block 自身超限 → 行级二次切分（代码块重加围栏）；仍超限 → ErrRichUnsendable。
// 禁止按字符数硬切——任何消息内部的行都保持完整。
func RichMessages(normalized string) ([]string, error) {
	blocks := ParseRichBlocks(normalized)
	if len(blocks) == 0 {
		return nil, fmt.Errorf("%w: 空内容", ErrRichUnsendable)
	}
	// 结构性校验：切分无法修复的超限先行报错。
	// Depth/Cols/Media 为切分不可修复的结构超限，先行报错；块数（Count）
	// 超限可经表格行切分修复，交由 splitOversizedBlock 处理。
	for _, b := range blocks {
		switch {
		case b.Depth > maxRichDepth:
			return nil, fmt.Errorf("%w: 嵌套 %d 层超上限 %d", ErrRichUnsendable, b.Depth, maxRichDepth)
		case b.Cols > maxRichCols:
			return nil, fmt.Errorf("%w: 表格 %d 列超上限 %d", ErrRichUnsendable, b.Cols, maxRichCols)
		case b.Media > maxRichMedia:
			return nil, fmt.Errorf("%w: 单块 %d 附件超上限 %d", ErrRichUnsendable, b.Media, maxRichMedia)
		}
	}

	// 单块自身字符超限 → 行级二次切分；不可切的原子块报错。
	queue := make([]RichBlock, 0, len(blocks))
	for _, b := range blocks {
		if utf8.RuneCountInString(b.Text) <= maxRichChars {
			queue = append(queue, b)
			continue
		}
		parts, err := splitOversizedBlock(b)
		if err != nil {
			return nil, err
		}
		queue = append(queue, parts...)
	}

	// 按 block 边界贪心组装（§2.3 切分策略）。
	var msgs []string
	acc := &richMsgAcc{}
	flush := func() {
		if msg := acc.emit(); msg != "" {
			msgs = append(msgs, msg)
		}
		acc = &richMsgAcc{} // 重置累加器，否则下条消息重复携带已发内容
	}
	for _, b := range queue {
		if !acc.fits(b) {
			flush()
		}
		acc.add(b)
	}
	flush()
	return msgs, nil
}

// richMsgAcc 是贪心组装的单条消息累加器。
type richMsgAcc struct {
	texts         []string
	blocks, media int
	chars         int
}

func (a *richMsgAcc) fits(b RichBlock) bool {
	if len(a.texts) == 0 {
		return true // 单块必然合法（超限块已先行切分/报错）
	}
	sep := utf8.RuneCountInString("\n\n")
	return a.chars+sep+utf8.RuneCountInString(b.Text) <= maxRichChars &&
		a.blocks+b.Count <= maxRichBlocks &&
		a.media+b.Media <= maxRichMedia
}

func (a *richMsgAcc) add(b RichBlock) {
	sep := 0
	if len(a.texts) > 0 {
		sep = utf8.RuneCountInString("\n\n")
	}
	a.texts = append(a.texts, b.Text)
	a.blocks += b.Count
	a.media += b.Media
	a.chars += sep + utf8.RuneCountInString(b.Text)
}

func (a *richMsgAcc) emit() string {
	if len(a.texts) == 0 {
		return ""
	}
	return strings.Join(a.texts, "\n\n")
}

// splitOversizedBlock 行级二次切分单块（§2.3）。代码块/公式围栏重加围栏、
// 表格按行切片（管道表重发表头/HTML 表重加包装）、段落/引用/列表项/脚注沿
// 行边界分片；媒体/标题为原子块——不可切者超限报错。
func splitOversizedBlock(b RichBlock) ([]RichBlock, error) {
	switch b.Kind {
	case RichCode, RichFormula:
		return splitFencedBlock(b)
	case RichTable:
		return splitTableBlock(b)
	case RichParagraph, RichQuote, RichListItem, RichFootnote:
		return splitLineBlock(b)
	default:
		return nil, fmt.Errorf("%w: %s 块自身超限且不可切分", ErrRichUnsendable, b.Kind)
	}
}

// splitFencedBlock 把超长围栏块按内容行分片，每片重加同语言围栏。
func splitFencedBlock(b RichBlock) ([]RichBlock, error) {
	lines := strings.Split(b.Text, "\n")
	open := lines[0]
	inner := lines[1 : len(lines)-1]
	fenceMark := open[:min(3, len(open))]
	lang := strings.TrimSpace(strings.TrimPrefix(open, fenceMark))
	openFull, closeFull := fenceMark+lang, fenceMark

	// 围栏自身占用：开栏 + 关栏 + 各自换行。
	budget := maxRichChars - utf8.RuneCountInString(openFull) - utf8.RuneCountInString(closeFull) - 2
	if budget <= 0 {
		return nil, fmt.Errorf("%w: 围栏标注过长", ErrRichUnsendable)
	}

	var parts []RichBlock
	var chunk []string
	size := 0 // chunk 内字符 + 换行数
	emit := func() {
		if len(chunk) == 0 {
			return
		}
		text := openFull + "\n" + strings.Join(chunk, "\n") + "\n" + closeFull
		parts = append(parts, RichBlock{Kind: b.Kind, Text: text, Depth: 1, Count: 1})
		chunk = nil
		size = 0
	}
	for _, line := range inner {
		n := utf8.RuneCountInString(line)
		if n > budget {
			return nil, fmt.Errorf("%w: 围栏内单行 %d 字符超上限且无行边界可切", ErrRichUnsendable, n)
		}
		add := n
		if len(chunk) > 0 {
			add++ // 行内换行
		}
		if len(chunk) > 0 && size+add > budget {
			emit()
			add = n
		}
		chunk = append(chunk, line)
		size += add
	}
	emit()
	return parts, nil
}

// splitLineBlock 沿行边界分片多行块（不按字符硬切；单行超限报错）。
func splitLineBlock(b RichBlock) ([]RichBlock, error) {
	lines := strings.Split(b.Text, "\n")
	var parts []RichBlock
	var chunk []string
	size := 0
	emit := func() {
		if len(chunk) == 0 {
			return
		}
		parts = append(parts, RichBlock{Kind: b.Kind, Text: strings.Join(chunk, "\n"), Depth: b.Depth, Count: 1})
		chunk = nil
		size = 0
	}
	for _, line := range lines {
		n := utf8.RuneCountInString(line)
		if n > maxRichChars {
			return nil, fmt.Errorf("%w: 单行 %d 字符超上限且无行边界可切", ErrRichUnsendable, n)
		}
		add := n
		if len(chunk) > 0 {
			add++ // 行内换行
		}
		if len(chunk) > 0 && size+add > maxRichChars {
			emit()
			add = n
		}
		chunk = append(chunk, line)
		size += add
	}
	emit()
	return parts, nil
}

// splitTableBlock 把超限表格按行二次切分（§2.3「行级」对表格的语义）：
// markdown 管道表逐片重发表头+分隔行；HTML 表逐片重加 <table> 包装。
// 单行自身超限仍不可修复。
func splitTableBlock(b RichBlock) ([]RichBlock, error) {
	lines := strings.Split(b.Text, "\n")
	pipe := len(lines) >= 2 && rePipeDivider.MatchString(lines[1])

	var header, divider, wrapOpen, wrapClose string
	var rows []string
	rowsPerChunk := maxRichBlocks - 1 // 管道表：表头占一个渲染行
	overhead := 0
	switch {
	case pipe && len(lines) >= 2:
		header, divider = lines[0], lines[1]
		rows = lines[2:]
		overhead = utf8.RuneCountInString(header) + utf8.RuneCountInString(divider) + 2
	case !pipe && len(lines) >= 3:
		wrapOpen, wrapClose = lines[0], lines[len(lines)-1]
		rows = lines[1 : len(lines)-1]
		rowsPerChunk = maxRichBlocks
		overhead = utf8.RuneCountInString(wrapOpen) + utf8.RuneCountInString(wrapClose) + 2
	default:
		return nil, fmt.Errorf("%w: 表格结构不完整不可行级切分", ErrRichUnsendable)
	}

	budget := maxRichChars - overhead
	if budget <= 0 {
		return nil, fmt.Errorf("%w: 表头自身超字符上限", ErrRichUnsendable)
	}

	var parts []RichBlock
	var chunk []string
	size := 0
	emit := func() {
		if len(chunk) == 0 {
			return
		}
		var text string
		var count int
		if pipe {
			text = header + "\n" + divider + "\n" + strings.Join(chunk, "\n")
			count = 1 + len(chunk) // 表头 + 数据行（分隔行不计）
		} else {
			text = wrapOpen + "\n" + strings.Join(chunk, "\n") + "\n" + wrapClose
			count = len(chunk)
		}
		parts = append(parts, RichBlock{Kind: RichTable, Text: text, Depth: 1, Count: count, Cols: b.Cols})
		chunk = nil
		size = 0
	}
	for _, row := range rows {
		if strings.TrimSpace(row) == "" {
			continue
		}
		n := utf8.RuneCountInString(row)
		if n > budget {
			return nil, fmt.Errorf("%w: 表格单行 %d 字符超上限且无行边界可切", ErrRichUnsendable, n)
		}
		add := n
		if len(chunk) > 0 {
			add++ // 行内换行
		}
		if len(chunk) > 0 && (size+add > budget || len(chunk)+1 > rowsPerChunk) {
			emit()
			add = n
		}
		chunk = append(chunk, row)
		size += add
	}
	emit()
	return parts, nil
}
