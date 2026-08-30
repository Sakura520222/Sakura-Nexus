// Package availability 提供可重复的「连接→断线→重连」依赖状态模型
// （01 §1.3）：一次性 close 的 ready channel 无法表达循环，本包以
// mutex + 订阅 channel 实现状态翻转广播。
package availability

import (
	"context"
	"errors"
	"sync"
)

// State 是依赖可用性状态。
type State bool

const (
	Unavailable State = false
	Ready       State = true
)

// ErrClosed 表示 Tracker 已关闭（进程关闭期），等待方应放弃。
var ErrClosed = errors.New("availability tracker 已关闭")

// Availability 是消费方（DEPENDENCY_UNAVAILABLE 状态中的 service）等待依赖
// 恢复的接口（01 §1.3）。
type Availability interface {
	IsReady() bool
	WaitReady(ctx context.Context) error // 阻塞至 Ready 或 ctx 取消
	SubscribeState() <-chan State        // 每次翻转发送新值；容量 1，非阻塞
}

// Tracker 是 Availability 的实现与发布端（platform 客户端内嵌使用）。
// 线程安全；Close 后所有订阅 channel 关闭。
type Tracker struct {
	mu     sync.Mutex
	state  State
	closed bool
	subs   []chan State
}

// NewTracker 以初始状态构造。
func NewTracker(initial State) *Tracker {
	return &Tracker{state: initial}
}

// SetReady 发布状态翻转；向每个订阅 channel 非阻塞发送最新值
// （先排空旧值再发，保证订阅者总能读到最新一次翻转）。
func (t *Tracker) SetReady(v State) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.state == v {
		return
	}
	t.state = v
	for _, ch := range t.subs {
		select {
		case <-ch: // 排空旧值（前一次翻转未被读走）
		default:
		}
		select {
		case ch <- v:
		default: // 订阅方不读也不阻塞发布方
		}
	}
}

// IsReady 返回当前状态。
func (t *Tracker) IsReady() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bool(t.state)
}

// SubscribeState 返回容量 1 的订阅 channel；实现方只发送、永不主动关闭
// （除 Close 外）。订阅方不持有独占读取义务——错过中间翻转时以 IsReady 兜底。
func (t *Tracker) SubscribeState() <-chan State {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		ch := make(chan State)
		close(ch)
		return ch
	}
	ch := make(chan State, 1)
	if t.state == Unavailable {
		// 预置当前值，订阅方 select 能立即感知
		ch <- Unavailable
	}
	t.subs = append(t.subs, ch)
	return ch
}

// WaitReady 阻塞至 Ready；ctx 取消返回 ctx.Err()，Tracker 关闭返回 ErrClosed。
func (t *Tracker) WaitReady(ctx context.Context) error {
	if t.IsReady() {
		return nil
	}
	ch := t.SubscribeState()
	for {
		// 先查状态再 select，避免「订阅后、翻转前」的窗口漏读
		if t.IsReady() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case v, ok := <-ch:
			if !ok {
				return ErrClosed
			}
			if v == Ready {
				return nil
			}
		}
	}
}

// Close 关闭全部订阅 channel（进程优雅退出时调用一次）。
func (t *Tracker) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	for _, ch := range t.subs {
		close(ch)
	}
	t.subs = nil
}
