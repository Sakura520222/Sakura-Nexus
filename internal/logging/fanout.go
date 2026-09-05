package logging

import (
	"context"
	"log/slog"
)

// Fanout 将日志记录广播到多个 handler（根输出 + 环形缓冲的组合形态；
// 01 §2.1 组合根 slog setup）。
type Fanout []slog.Handler

// NewFanout 组合（至少一个 handler）。
func NewFanout(handlers ...slog.Handler) slog.Handler { return Fanout(handlers) }

// Enabled 任一启用即启用。
func (f Fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

// Handle 逐个投递（首错返回，不中断其余投递）。
func (f Fanout) Handle(ctx context.Context, rec slog.Record) error {
	var firstErr error
	for _, h := range f {
		if !h.Enabled(ctx, rec.Level) {
			continue
		}
		if err := h.Handle(ctx, rec.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// WithAttrs 各分支独立派生（保持类型）。
func (f Fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	children := make(Fanout, len(f))
	for i, h := range f {
		children[i] = h.WithAttrs(attrs)
	}
	return children
}

// WithGroup 各分支独立派生。
func (f Fanout) WithGroup(name string) slog.Handler {
	children := make(Fanout, len(f))
	for i, h := range f {
		children[i] = h.WithGroup(name)
	}
	return children
}
