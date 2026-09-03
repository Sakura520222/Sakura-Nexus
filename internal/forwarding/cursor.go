package forwarding

// cursorTracker 跟踪单条转发规则的 contiguous cursor（P0 Plan §6 冻结语义）。
//
// cursor 是「实际观察到的有序消息流中最高连续 terminal 消息 ID」：
//   - terminal（可推进）：filtered/skipped、dedup already-sent、send success、
//     永久性失败（按策略标记 terminal）
//   - non-terminal（不得越过）：transient send failure（FloodWait 超限、
//     重试耗尽、临时错误）——保持 unresolved，cursor 停在其前
//
// 「连续」不要求数值连续：未观察到的 ID 空洞不阻挡推进（Telegram message ID
// 空洞属正常）。并发由持有方（Engine）加锁——本类型非并发安全。
type cursorTracker struct {
	cursor     int64
	unresolved map[int64]struct{}
	terminals  []int64 // 已终结但被更早 unresolved 阻挡的 ID（升序、去重）
}

func newCursorTracker(start int64) *cursorTracker {
	return &cursorTracker{cursor: start, unresolved: map[int64]struct{}{}}
}

// observe 标记 ID 进入处理中（unresolved）；低于已推进 cursor 的重放为 no-op。
func (t *cursorTracker) observe(id int64) {
	if id <= t.cursor {
		return
	}
	t.unresolved[id] = struct{}{}
}

// terminal 标记 ID 终结，并尝试推进 cursor：弹出全部低于最小 unresolved 的
// 终结 ID。返回推进后的 cursor 与本次是否推进。
func (t *cursorTracker) terminal(id int64) (int64, bool) {
	if id <= t.cursor {
		return t.cursor, false // 已越过（重放）
	}
	delete(t.unresolved, id)

	// 升序去重插入 terminals
	i := 0
	for i < len(t.terminals) && t.terminals[i] < id {
		i++
	}
	if i == len(t.terminals) || t.terminals[i] != id {
		t.terminals = append(t.terminals, 0)
		copy(t.terminals[i+1:], t.terminals[i:])
		t.terminals[i] = id
	}

	// 推进至最小 unresolved 之前的最高连续终结 ID
	minUn, hasUn := int64(0), false
	for u := range t.unresolved {
		if !hasUn || u < minUn {
			minUn, hasUn = u, true
		}
	}
	advanced := false
	for len(t.terminals) > 0 && (!hasUn || t.terminals[0] < minUn) {
		t.cursor = t.terminals[0]
		t.terminals = t.terminals[1:]
		advanced = true
	}
	return t.cursor, advanced
}

// current 返回当前 contiguous cursor。
func (t *cursorTracker) current() int64 { return t.cursor }

// pending 返回未终结消息数（诊断用）。
func (t *cursorTracker) pending() int { return len(t.unresolved) }
