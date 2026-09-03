package forwarding

import "testing"

func TestCursorAdvanceOnTerminal(t *testing.T) {
	tr := newCursorTracker(99)
	tr.observe(100)
	cur, advanced := tr.terminal(100)
	if !advanced || cur != 100 {
		t.Fatalf("terminal(100) 应推进到 100，得到 cur=%d advanced=%v", cur, advanced)
	}
	if tr.current() != 100 || tr.pending() != 0 {
		t.Fatalf("current=%d pending=%d，应为 100/0", tr.current(), tr.pending())
	}
}

// P0 Plan §6 冻结示例：100 failed；101、102 success → cursor 保持 99；
// 100 重试成功后连续推进至 102。
func TestCursorTransientFailureBlocksAdvance(t *testing.T) {
	tr := newCursorTracker(99)
	tr.observe(100)
	tr.observe(101)
	tr.observe(102)

	// 101、102 终结：被 unresolved 100 阻挡
	if cur, moved := tr.terminal(101); moved || cur != 99 {
		t.Fatalf("terminal(101) 不应推进（100 未终结），得到 cur=%d moved=%v", cur, moved)
	}
	if cur, moved := tr.terminal(102); moved || cur != 99 {
		t.Fatalf("terminal(102) 不应推进（100 未终结），得到 cur=%d moved=%v", cur, moved)
	}
	if tr.current() != 99 {
		t.Fatalf("cursor 应保持 99，实际 %d", tr.current())
	}
	if tr.pending() != 1 {
		t.Fatalf("pending 应为 1（仅 100），实际 %d", tr.pending())
	}

	// 100 恢复终结 → 连续推进至 102
	cur, moved := tr.terminal(100)
	if !moved || cur != 102 {
		t.Fatalf("terminal(100) 后应连续推进至 102，得到 cur=%d moved=%v", cur, moved)
	}
}

// ID 空洞不阻挡（Telegram message ID 空洞属正常；连续性针对观察到的流）。
func TestCursorHolesDoNotBlock(t *testing.T) {
	tr := newCursorTracker(99)
	tr.observe(105) // 100–104 未观察到
	if cur, moved := tr.terminal(105); !moved || cur != 105 {
		t.Fatalf("terminal(105) 应推进至 105，得到 cur=%d moved=%v", cur, moved)
	}
}

func TestCursorTerminalBelowCursorIsNoop(t *testing.T) {
	tr := newCursorTracker(99)
	if cur, moved := tr.terminal(50); moved || cur != 99 {
		t.Fatalf("低于 cursor 的终结应为 no-op，得到 cur=%d moved=%v", cur, moved)
	}
}

// 终结后重投（backfill 重放）不回退、不重复推进。
func TestCursorRedeliveryAfterTerminal(t *testing.T) {
	tr := newCursorTracker(99)
	tr.observe(100)
	tr.terminal(100)
	// backfill 重放：重新 observe + terminal
	tr.observe(100)
	if cur, moved := tr.terminal(100); moved || cur != 100 {
		t.Fatalf("重放 terminal(100) 不应再次推进，得到 cur=%d moved=%v", cur, moved)
	}
	if tr.current() != 100 {
		t.Fatalf("cursor 应为 100，实际 %d", tr.current())
	}
}

func TestCursorMixedAlbumMembers(t *testing.T) {
	tr := newCursorTracker(0)
	for _, id := range []int64{10, 11, 12} {
		tr.observe(id)
	}
	// 相册整组一次终结：全部 terminal 后推进至最大成员
	var last int64
	var moved bool
	for _, id := range []int64{10, 11, 12} {
		last, moved = tr.terminal(id)
	}
	if !moved || last != 12 {
		t.Fatalf("相册全成员终结应推进至 12，得到 cur=%d moved=%v", last, moved)
	}
}
