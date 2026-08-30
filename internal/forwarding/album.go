package forwarding

import (
	"sync"
	"time"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// AlbumConfig 是相册聚合窗口参数（settings.forwarding 注入，03 §1.6）。
type AlbumConfig struct {
	QuietMs        int // 静默窗口（默认 450）：每来一条重置
	HardDeadlineMs int // 硬上限（默认 2000）
	MaxSize        int // 集满即 flush（Telegram 上限 10）
}

func DefaultAlbumConfig() AlbumConfig {
	return AlbumConfig{QuietMs: 450, HardDeadlineMs: 2000, MaxSize: 10}
}

// clock 是可注入的时间源（生产 wall clock，测试假时钟）。
type clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// AlbumHandler 接收聚合完成的整组消息。
type AlbumHandler func(msgs []domain.ChannelMessage)

// albumState 是单个 grouped_id 的聚合状态。
type albumState struct {
	msgs      []domain.ChannelMessage
	firstSeen time.Time
	timerCh   <-chan time.Time // 当前 quiet timer
	flushed   bool
}

// AlbumAggregator 按 grouped_id 聚合相册（03 §1.6 R3.1 真动态窗口）：
// quiet 超时 OR hard deadline OR 集满 → flush；窗口后迟到成员走常规流程
// （由调用方处理——Add 返回 (nil, false) 表示非相册首条且窗口已过）。
//
// 使用模型：引擎对每条 NewMessage 调用 Add；返回 (group, true) 时整组就绪
// 待处理（首条判定已由调用方完成——Add 不重复回调）。并发安全：单 goroutine
// 消费 update（gotd dispatcher 顺序投递），Aggregator 内部锁仅防御多 handler。
type AlbumAggregator struct {
	cfg    AlbumConfig
	clock  clock
	handle AlbumHandler

	mu     sync.Mutex
	groups map[int64]*albumState
}

func NewAlbumAggregator(cfg AlbumConfig, handle AlbumHandler) *AlbumAggregator {
	if cfg.QuietMs <= 0 || cfg.HardDeadlineMs < cfg.QuietMs || cfg.MaxSize <= 0 {
		cfg = DefaultAlbumConfig()
	}
	return &AlbumAggregator{
		cfg:    cfg,
		clock:  wallClock{},
		handle: handle,
		groups: map[int64]*albumState{},
	}
}

// Add 投递一条消息；返回：
//   - (nil, false)：非相册消息（GroupedID==0），调用方走常规单条流程
//   - (nil, false)：相册成员、窗口未满——已暂存
//   - (group, true)：触发 flush（集满/静默/硬上限由内部 timer 决定；
//     同步 flush 仅在集满时发生，静默/硬上限经 FlushDue 异步驱动）
func (a *AlbumAggregator) Add(m domain.ChannelMessage) ([]domain.ChannelMessage, bool) {
	if m.GroupedID == 0 {
		return nil, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	st, ok := a.groups[m.GroupedID]
	if !ok {
		if m.Ref.MessageID <= 0 {
			return nil, false
		}
		st = &albumState{firstSeen: a.clock.Now()}
		a.groups[m.GroupedID] = st
		st.msgs = append(st.msgs, m)
		// 首条即集满（不可能但防御）与正常路径一致走追加后检查
		a.resetTimerLocked(m.GroupedID, st)
		if len(st.msgs) >= a.cfg.MaxSize {
			return a.flushLocked(m.GroupedID, st), true
		}
		return nil, false
	}

	if st.flushed {
		// 窗口后迟到成员（R3.1：独立新消息走常规流程，记 metric warn 由调用方）
		return nil, false
	}
	st.msgs = append(st.msgs, m)
	a.resetTimerLocked(m.GroupedID, st)
	if len(st.msgs) >= a.cfg.MaxSize {
		return a.flushLocked(m.GroupedID, st), true
	}
	// 硬上限检查：即使静默未到，超时即 flush
	if a.clock.Now().Sub(st.firstSeen) >= time.Duration(a.cfg.HardDeadlineMs)*time.Millisecond {
		return a.flushLocked(m.GroupedID, st), true
	}
	return nil, false
}

// resetTimerLocked 重置静默 timer（每来一条重置——03 §1.6）。
func (a *AlbumAggregator) resetTimerLocked(gid int64, st *albumState) {
	d := time.Duration(a.cfg.QuietMs) * time.Millisecond
	if remain := time.Duration(a.cfg.HardDeadlineMs)*time.Millisecond - a.clock.Now().Sub(st.firstSeen); remain < d {
		d = remain // 静默窗口不得越过硬上限
	}
	if d <= 0 {
		return // 由 Add 的硬上限分支处理
	}
	st.timerCh = a.clock.After(d)
}

// flushLocked 弹出并标记完成。
func (a *AlbumAggregator) flushLocked(gid int64, st *albumState) []domain.ChannelMessage {
	st.flushed = true
	delete(a.groups, gid)
	return st.msgs
}

// FlushDue 非阻塞驱动：检查全部未完成分组的 timer 通道/硬上限，
// 到期即 flush（引擎在自己的事件循环里周期调用，或由 timer goroutine 调用）。
// 返回本次 flush 的组数。
func (a *AlbumAggregator) FlushDue() [][]domain.ChannelMessage {
	a.mu.Lock()
	defer a.mu.Unlock()

	var out [][]domain.ChannelMessage
	now := a.clock.Now()
	for gid, st := range a.groups {
		select {
		case <-st.timerCh: // 静默超时
			out = append(out, a.flushLocked(gid, st))
		default:
			if now.Sub(st.firstSeen) >= time.Duration(a.cfg.HardDeadlineMs)*time.Millisecond {
				out = append(out, a.flushLocked(gid, st)) // 硬上限兜底
			}
		}
	}
	return out
}

// Pending 报告当前暂存分组数（指标用）。
func (a *AlbumAggregator) Pending() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.groups)
}
