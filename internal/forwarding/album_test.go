package forwarding

import (
	"testing"
	"time"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// fakeClock 手动推进时间；After 返回的通道由测试在推进时关闭。
type fakeClock struct {
	mu     time.Time
	timers []fakeTimer
}

type fakeTimer struct {
	fireAt time.Time
	ch     chan time.Time
}

func (c *fakeClock) Now() time.Time { return c.mu }
func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, fakeTimer{fireAt: c.mu.Add(d), ch: ch})
	return ch
}

// advance 推进时间：触发全部到期 timer（非阻塞写）。
func (c *fakeClock) advance(d time.Duration) {
	c.mu = c.mu.Add(d)
	for _, t := range c.timers {
		if !t.fireAt.After(c.mu) {
			select {
			case t.ch <- c.mu:
			default:
			}
		}
	}
}

func newTestAgg(cfg AlbumConfig, handle AlbumHandler) (*AlbumAggregator, *fakeClock) {
	fc := &fakeClock{mu: time.Unix(1700000000, 0)}
	agg := NewAlbumAggregator(cfg, handle)
	agg.clock = fc
	return agg, fc
}

func TestAlbumQuietTimeoutFlush(t *testing.T) {
	// 静默窗口：最后一条到达后 quiet 无新成员 → flush
	var flushed [][]domain.ChannelMessage
	agg, fc := newTestAgg(AlbumConfig{QuietMs: 450, HardDeadlineMs: 2000, MaxSize: 10},
		func(msgs []domain.ChannelMessage) { flushed = append(flushed, msgs) })

	agg.Add(albumMsg(1, "首条", "photo", false))
	agg.Add(albumMsg(2, "", "photo", false))
	if agg.Pending() != 1 {
		t.Fatalf("暂存中: pending=%d", agg.Pending())
	}

	// 未到静默窗口：不 flush
	fc.advance(200 * time.Millisecond)
	if due := agg.FlushDue(); len(due) != 0 {
		t.Fatalf("静默未到不应 flush: %d", len(due))
	}
	// 静默重置：再添一条后 200ms 前的计时作废
	agg.Add(albumMsg(3, "", "video", false))
	fc.advance(200 * time.Millisecond)
	if len(agg.FlushDue()) != 0 {
		t.Fatal("第三条后 200ms < quiet 450ms 不应 flush")
	}
	// 静默到达
	fc.advance(300 * time.Millisecond)
	due := agg.FlushDue()
	if len(due) != 1 || len(due[0]) != 3 {
		t.Fatalf("静默 flush: %d 组", len(due))
	}
	if agg.Pending() != 0 {
		t.Error("flush 后应清空")
	}
}

func TestAlbumFullAtMaxFlush(t *testing.T) {
	// 集满 10 条（Telegram 上限）→ 立即同步 flush，不等窗口
	agg, _ := newTestAgg(AlbumConfig{QuietMs: 450, HardDeadlineMs: 2000, MaxSize: 3},
		func([]domain.ChannelMessage) {})

	agg.Add(albumMsg(1, "", "photo", false))
	agg.Add(albumMsg(2, "", "photo", false))
	if group, ready := agg.Add(albumMsg(3, "", "video", false)); !ready || len(group) != 3 {
		t.Fatalf("集满同步 flush: ready=%v len=%d", ready, len(group))
	}
	if agg.Pending() != 0 {
		t.Error("集满 flush 后应清空")
	}
}

func TestAlbumHardDeadline(t *testing.T) {
	// 持续有新成员（静默不断重置）但达硬上限 → flush
	agg, fc := newTestAgg(AlbumConfig{QuietMs: 450, HardDeadlineMs: 1000, MaxSize: 10},
		func([]domain.ChannelMessage) {})

	agg.Add(albumMsg(1, "", "photo", false))
	for i := 2; i <= 4; i++ {
		fc.advance(300 * time.Millisecond) // 每条间隔 300ms < quiet 450ms → 静默不触发
		if done := agg.FlushDue(); len(done) != 0 {
			t.Fatalf("静默期间不应 flush（第 %d 条后）", i)
		}
		agg.Add(albumMsg(int64(i), "", "photo", false))
	}
	// 4 条后 now=900ms；再进 200ms → 1100ms ≥ hard 1000ms：FlushDue 硬上限兜底 flush
	fc.advance(200 * time.Millisecond)
	due := agg.FlushDue()
	if len(due) != 1 || len(due[0]) != 4 {
		t.Fatalf("硬上限 flush: %d 组 %d 条", len(due), len(due[0]))
	}
	if agg.Pending() != 0 {
		t.Error("硬上限 flush 后应清空")
	}
}

func TestAlbumLateMemberAfterFlush(t *testing.T) {
	// 窗口后迟到的同 grouped_id 成员：不并入旧组（已 flush），也不 panic
	agg, fc := newTestAgg(AlbumConfig{QuietMs: 100, HardDeadlineMs: 500, MaxSize: 10},
		func([]domain.ChannelMessage) {})

	agg.Add(albumMsg(1, "", "photo", false))
	fc.advance(200 * time.Millisecond)
	if due := agg.FlushDue(); len(due) != 1 {
		t.Fatal("首组应已 flush")
	}
	// 迟到成员：Add 返回 not-ready（调用方按独立新消息处理）
	if group, ready := agg.Add(albumMsg(2, "", "photo", false)); ready || group != nil {
		t.Errorf("迟到成员应独立处理: ready=%v", ready)
	}
	// 但它自己开启新组聚合（防御后续迟到链）
	if agg.Pending() != 1 {
		t.Errorf("迟到成员开启新组: pending=%d", agg.Pending())
	}
}

func TestAlbumNonAlbumPassthrough(t *testing.T) {
	agg, _ := newTestAgg(DefaultAlbumConfig(), func([]domain.ChannelMessage) {
		t.Error("非相册消息不应触发聚合回调")
	})
	single := albumMsg(1, "单条", "", false)
	single.GroupedID = 0
	if group, ready := agg.Add(single); ready || group != nil {
		t.Error("GroupedID==0 直接透传")
	}
	if agg.Pending() != 0 {
		t.Error("非相册不入暂存")
	}
}

func TestAlbumInvalidConfigFallsBack(t *testing.T) {
	// 非法配置回落默认（QuietMs>Hard / 负值）
	agg := NewAlbumAggregator(AlbumConfig{QuietMs: 2000, HardDeadlineMs: 100, MaxSize: -1}, func([]domain.ChannelMessage) {})
	if agg.cfg != DefaultAlbumConfig() {
		t.Errorf("非法配置应回落默认: %+v", agg.cfg)
	}
}
