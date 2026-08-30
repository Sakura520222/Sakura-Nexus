// Package app 承担应用组合与生命周期：service 注册/逆序关闭/supervisor、
// readiness barrier 与退出码（0/1/2/75）。设计：docs/design/01 §1。
package app

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Service 是全部受管组件的生命周期抽象（01 §1.2）。
// Run 阻塞直至 ctx 取消；返回 error = OWN_FATAL（见 Criticality）。
type Service interface {
	Name() string
	Run(ctx context.Context) error
	Shutdown(ctx context.Context) error
}

// Criticality 决定 OWN_FATAL 的处置（01 §1.3）：
//   - Core：全局 cancel → 优雅退出 → exit 1（systemd Restart=on-failure 兜底）
//   - Degradable：supervisor 指数退避重启**仅该服务**，进程存活
type Criticality int

const (
	Core Criticality = iota
	Degradable
)

// Readiness 由希望参与 readiness barrier 的服务可选实现
// （就绪后关闭返回的 channel，恰好一次）。
type Readiness interface {
	Ready() <-chan struct{}
}

// Options 调优参数（测试可缩短；生产取默认或来自 .env）。
type Options struct {
	ShutdownTimeout   time.Duration // 关闭总预算（默认 30s，.env SHUTDOWN_TIMEOUT_SECONDS）
	RestartBackoff    time.Duration // Degradable 重启退避基数（默认 100ms）
	RestartBackoffMax time.Duration // 退避上限（默认 30s）
	StableRun         time.Duration // 运行满此时长后重置退避（默认 1min）
	Log               *slog.Logger
}

func (o *Options) fill() {
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = 30 * time.Second
	}
	if o.RestartBackoff <= 0 {
		o.RestartBackoff = 100 * time.Millisecond
	}
	if o.RestartBackoffMax <= 0 {
		o.RestartBackoffMax = 30 * time.Second
	}
	if o.StableRun <= 0 {
		o.StableRun = time.Minute
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
}

// App 管理注册服务的启动、监督与优雅关闭；Run 返回进程退出码。
type App struct {
	opts Options
	regs []struct {
		svc  Service
		crit Criticality
	}

	cancel     context.CancelFunc
	exitCode   int
	fatalOnce  sync.Once
	restartReq bool

	readyOnce sync.Once
	readyCh   chan struct{}
}

func New(opts Options) *App {
	opts.fill()
	return &App{opts: opts, readyCh: make(chan struct{})}
}

// Register 按启动顺序注册服务（关闭按逆序）。
func (a *App) Register(svc Service, crit Criticality) {
	a.regs = append(a.regs, struct {
		svc  Service
		crit Criticality
	}{svc, crit})
}

// RequestRestart 请求以退出码 75 结束（WebUI restart；非零使 systemd
// Restart=on-failure 与 docker unless-stopped 拉起新进程）。
func (a *App) RequestRestart() {
	a.opts.Log.Info("请求重启（exit 75）")
	a.restartReq = true
	a.shutdown()
}

// Ready 在全部实现 Readiness 的 Core 服务就绪后关闭
// （无此类服务则启动后立即关闭）——readiness barrier（01 §1.1 步骤 8）。
func (a *App) Ready() <-chan struct{} { return a.readyCh }

func (a *App) shutdown() { a.cancel() }

// Run 启动全部服务并阻塞至优雅退出或 CORE fatal，返回退出码：
// 0 正常（ctx 取消）/ 1 CORE fatal / 75 重启请求。
func (a *App) Run(parent context.Context) int {
	ctx, cancel := context.WithCancel(parent)
	a.cancel = cancel
	defer cancel()

	var wg sync.WaitGroup
	readyWg := a.startReadinessBarrier(ctx)

	for i := range a.regs {
		reg := a.regs[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.supervise(ctx, reg.svc, reg.crit)
		}()
	}

	wg.Wait() // 全部 supervisor 返回（ctx 取消或 CORE fatal 已触发 cancel）
	a.closeAll(readyWg)

	if a.exitCode != 0 {
		return a.exitCode
	}
	if a.restartReq {
		return 75
	}
	return 0
}

// supervise 是单服务的监督循环：panic 边界 recover、Degradable 退避重启。
func (a *App) supervise(ctx context.Context, svc Service, crit Criticality) {
	backoff := a.opts.RestartBackoff
	lg := a.opts.Log.With("service", svc.Name(), "criticality", critString(crit))
	for {
		start := time.Now()
		err := a.runGuarded(ctx, svc, lg)
		if ctx.Err() != nil {
			return // 优雅退出：Run 尚未返回的错误不再处置
		}
		if crit == Core {
			a.coreFatal(svc, err)
			return
		}
		lg.Error("服务退出（OWN_FATAL），退避后重启", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if time.Since(start) >= a.opts.StableRun {
			backoff = a.opts.RestartBackoff // 稳定运行过则重置退避
		} else if next := backoff * 2; next <= a.opts.RestartBackoffMax {
			backoff = next
		} else {
			backoff = a.opts.RestartBackoffMax
		}
	}
}

// runGuarded 执行一次 Run，panic 记 stack 并转为错误（01 §1.3：
// 仅此边界 recover，禁止业务代码裸 recover 继续跑）。
func (a *App) runGuarded(ctx context.Context, svc Service, lg *slog.Logger) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
			lg.Error("服务 panic（边界 recover，等同 OWN_FATAL）", "panic", r)
		}
	}()
	return svc.Run(ctx)
}

func (a *App) coreFatal(svc Service, err error) {
	a.fatalOnce.Do(func() {
		a.exitCode = 1
		a.opts.Log.Error("CORE 服务不可恢复退出：全局优雅退出（exit 1）",
			"service", svc.Name(), "err", err)
		a.shutdown()
	})
}

// startReadinessBarrier 汇聚 Core 服务的就绪信号。
func (a *App) startReadinessBarrier(ctx context.Context) *sync.WaitGroup {
	var readyWg sync.WaitGroup
	n := 0
	for i := range a.regs {
		if a.regs[i].crit != Core {
			continue
		}
		r, ok := a.regs[i].svc.(Readiness)
		if !ok {
			continue
		}
		n++
		readyWg.Add(1)
		go func(ch <-chan struct{}) {
			defer readyWg.Done()
			select {
			case <-ch:
			case <-ctx.Done():
			}
		}(r.Ready())
	}
	if n == 0 {
		a.markReady()
		return &readyWg
	}
	go func() {
		readyWg.Wait()
		a.markReady()
	}()
	return &readyWg
}

func (a *App) markReady() {
	a.readyOnce.Do(func() { close(a.readyCh) })
}

func (a *App) closeAll(readyWg *sync.WaitGroup) {
	// barrier 未完成时在关闭预算内收尾，避免 goroutine 泄漏
	waitDone := make(chan struct{})
	go func() { readyWg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(a.opts.ShutdownTimeout):
	}
	a.markReady() // 关闭路径也解除等待方阻塞

	// 逆序关闭；总预算依次扣减（前面的服务占用过多则后面的拿到已超时 ctx）
	deadline := time.Now().Add(a.opts.ShutdownTimeout)
	for i := len(a.regs) - 1; i >= 0; i-- {
		svc := a.regs[i].svc
		sctx, scancel := context.WithDeadline(context.Background(), deadline)
		if err := svc.Shutdown(sctx); err != nil {
			a.opts.Log.Warn("服务关闭出错（不致命）", "service", svc.Name(), "err", err)
		}
		scancel()
	}
}

func critString(c Criticality) string {
	if c == Core {
		return "core"
	}
	return "degradable"
}
