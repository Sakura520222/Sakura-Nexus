package logging

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// 环形缓冲 handler（01 §2.1、06 §4）：供 WebUI 日志快照与实时流消费。
// 容量固定、写入 O(1)、慢订阅者不阻塞发布方（满则丢弃该订阅者的此次推送）。

// Record 是 WebUI 消费的日志条目形态（04 §5：{ts, level, component, msg}）。
type Record struct {
	Time      time.Time   `json:"ts"`
	Level     slog.Level  `json:"level"`
	Component string      `json:"component"`
	Message   string      `json:"msg"`
	Attrs     []slog.Attr `json:"attrs,omitempty"` // component 之外的属性
}

// componentKey 是组件标识的属性键约定（Named 工厂注入）。
const componentKey = "component"

// Ring 是固定容量环形缓冲的 slog.Handler + 实时订阅广播。
type Ring struct {
	mu     sync.Mutex
	buf    []Record
	cap    int
	subs   []chan Record
	closed bool
}

// NewRing 以容量构造（容量 ≤0 取 1024）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 1024
	}
	return &Ring{cap: capacity}
}

// Enabled 恒真（缓冲全量记录；级别过滤由根 handler 前置完成）。
func (r *Ring) Enabled(context.Context, slog.Level) bool { return true }

// Handle 环形追加并向订阅者非阻塞广播。
func (r *Ring) Handle(ctx context.Context, rec slog.Record) error {
	return r.handle(ctx, rec, nil)
}

func (r *Ring) handle(_ context.Context, rec slog.Record, preformatted []slog.Attr) error {
	entry := Record{
		Time:    rec.Time,
		Level:   rec.Level,
		Message: rec.Message,
	}
	collect := func(a slog.Attr) bool {
		if a.Key == componentKey {
			if v, ok := a.Value.Any().(string); ok {
				entry.Component = v
				return true
			}
		}
		entry.Attrs = append(entry.Attrs, a)
		return true
	}
	for _, a := range preformatted {
		collect(a)
	}
	rec.Attrs(collect)

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	if len(r.buf) < r.cap {
		r.buf = append(r.buf, entry)
	} else {
		copy(r.buf, r.buf[1:])
		r.buf[r.cap-1] = entry
	}
	subs := append([]chan Record(nil), r.subs...)
	r.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub <- entry:
		default: // 慢订阅者丢弃此次推送，不阻塞发布
		}
	}
	return nil
}

// WithAttrs 返回携带预格式化属性的子 handler（Named 的 component 经此进入
// Handle 的属性流——返回自身会丢失 With 链）。
func (r *Ring) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ringChild{ring: r, attrs: attrs}
}

// WithGroup 透传（环形缓冲不展开组；P0 无组用例）。
func (r *Ring) WithGroup(string) slog.Handler { return r }

// ringChild 是 With 链子 handler：Handle 时合并预格式化属性。
type ringChild struct {
	ring  *Ring
	attrs []slog.Attr
}

func (c *ringChild) Enabled(ctx context.Context, l slog.Level) bool { return c.ring.Enabled(ctx, l) }

func (c *ringChild) Handle(ctx context.Context, rec slog.Record) error {
	return c.ring.handle(ctx, rec, c.attrs)
}

func (c *ringChild) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := append(append([]slog.Attr(nil), c.attrs...), attrs...)
	return &ringChild{ring: c.ring, attrs: merged}
}

func (c *ringChild) WithGroup(name string) slog.Handler { return c }

// Snapshot 返回缓冲内容的副本（旧→新序）。
func (r *Ring) Snapshot() []Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Record(nil), r.buf...)
}

// Subscribe 返回容量 8 的实时订阅流（Close 后关闭）。
func (r *Ring) Subscribe() <-chan Record {
	ch := make(chan Record, 8)
	r.mu.Lock()
	r.subs = append(r.subs, ch)
	r.mu.Unlock()
	return ch
}

// Close 关闭全部订阅流（进程退出/WebServer 关闭时调用一次）。
func (r *Ring) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	for _, ch := range r.subs {
		close(ch)
	}
	r.subs = nil
}

// Named 为组件 logger 注入 component 字段（Record 提升用）。
func Named(lg *slog.Logger, component string) *slog.Logger {
	return lg.With(slog.String(componentKey, component))
}
