package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeService 记录生命周期调用顺序，可编程 Run 行为。
type fakeService struct {
	name string

	mu        sync.Mutex
	runCalls  int
	events    *[]string // 全局事件记录（共享切片）
	runFn     func(ctx context.Context, n int) error
	ready     chan struct{}
	readyOnce sync.Once
	autoReady bool // Run 进入即视为就绪（不参与分步 barrier 验证的测试用）
}

func newFake(name string, events *[]string) *fakeService {
	return &fakeService{name: name, events: events, ready: make(chan struct{})}
}

func (f *fakeService) Name() string { return f.name }

func (f *fakeService) Run(ctx context.Context) error {
	f.mu.Lock()
	f.runCalls++
	n := f.runCalls
	f.mu.Unlock()
	if f.autoReady {
		f.signalReady()
	}
	if f.runFn == nil {
		<-ctx.Done()
		return nil
	}
	return f.runFn(ctx, n)
}

func (f *fakeService) Shutdown(ctx context.Context) error {
	*f.events = append(*f.events, "shutdown:"+f.name)
	return nil
}

func (f *fakeService) Ready() <-chan struct{} { return f.ready }

func (f *fakeService) signalReady() {
	f.readyOnce.Do(func() { close(f.ready) })
}

func fastOpts(events *[]string) Options {
	return Options{
		ShutdownTimeout:   time.Second,
		RestartBackoff:    5 * time.Millisecond,
		RestartBackoffMax: 20 * time.Millisecond,
		StableRun:         10 * time.Millisecond,
	}
}

func TestGracefulShutdownReverseOrder(t *testing.T) {
	var events []string
	a := New(fastOpts(&events))
	s1 := newFake("s1", &events)
	s2 := newFake("s2", &events)
	s3 := newFake("s3", &events)
	s1.autoReady, s2.autoReady, s3.autoReady = true, true, true
	a.Register(s1, Core)
	a.Register(s2, Core)
	a.Register(s3, Degradable)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- a.Run(ctx) }()

	<-a.Ready() // 无 Readiness 实现的 Core → 立即就绪
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("退出码 = %d，期望 0", code)
	}
	want := []string{"shutdown:s3", "shutdown:s2", "shutdown:s1"}
	if len(events) != 3 {
		t.Fatalf("事件 = %v", events)
	}
	for i, w := range want {
		if events[i] != w {
			t.Fatalf("关闭顺序 = %v，期望 %v（逆序）", events, want)
		}
	}
}

func TestCoreFatalExitsOne(t *testing.T) {
	var events []string
	a := New(fastOpts(&events))
	core := newFake("core", &events)
	core.runFn = func(ctx context.Context, n int) error {
		return errors.New("boom")
	}
	okCore := newFake("ok", &events)
	okCore.autoReady = true
	a.Register(core, Core)
	a.Register(okCore, Core)

	done := make(chan int)
	go func() { done <- a.Run(context.Background()) }()

	if code := <-done; code != 1 {
		t.Fatalf("CORE fatal 退出码 = %d，期望 1", code)
	}
	// 两个服务都被关闭（逆序）
	if len(events) != 2 || events[0] != "shutdown:ok" || events[1] != "shutdown:core" {
		t.Fatalf("事件 = %v", events)
	}
}

func TestDegradableRestartsWithBackoff(t *testing.T) {
	var events []string
	a := New(fastOpts(&events))
	d := newFake("degradable", &events)
	failures := make(chan struct{}, 5)
	d.runFn = func(ctx context.Context, n int) error {
		if n <= 3 {
			failures <- struct{}{}
			return errors.New("transient")
		}
		<-ctx.Done()
		return nil
	}
	a.Register(d, Degradable)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- a.Run(ctx) }()

	// 三次失败后第四次稳定运行：进程不退出
	for i := 0; i < 3; i++ {
		<-failures
	}
	time.Sleep(50 * time.Millisecond) // 允许退避重启到第 4 次

	d.mu.Lock()
	calls := d.runCalls
	d.mu.Unlock()
	if calls < 4 {
		t.Fatalf("应至少重启到第 4 次运行，实际 %d", calls)
	}
	select {
	case code := <-done:
		t.Fatalf("Degradable 失败不应退出进程（code=%d）", code)
	default:
	}

	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("退出码 = %d，期望 0", code)
	}
}

func TestPanicCoreIsFatal(t *testing.T) {
	var events []string
	a := New(fastOpts(&events))
	core := newFake("core", &events)
	core.runFn = func(ctx context.Context, n int) error {
		panic("invariant violation")
	}
	a.Register(core, Core)

	done := make(chan int)
	go func() { done <- a.Run(context.Background()) }()
	if code := <-done; code != 1 {
		t.Fatalf("panic 的 CORE 服务应致 exit 1，实际 %d", code)
	}
}

func TestPanicDegradableRestarts(t *testing.T) {
	var events []string
	a := New(fastOpts(&events))
	d := newFake("d", &events)
	d.runFn = func(ctx context.Context, n int) error {
		if n == 1 {
			panic("first run panics")
		}
		<-ctx.Done()
		return nil
	}
	a.Register(d, Degradable)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	d.mu.Lock()
	calls := d.runCalls
	d.mu.Unlock()
	if calls < 2 {
		t.Fatalf("panic 后应重启，实际运行 %d 次", calls)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("退出码 = %d，期望 0", code)
	}
}

func TestRequestRestartExits75(t *testing.T) {
	var events []string
	a := New(fastOpts(&events))
	s := newFake("s", &events)
	s.autoReady = true
	a.Register(s, Core)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int)
	go func() { done <- a.Run(ctx) }()
	<-a.Ready()

	a.RequestRestart()
	if code := <-done; code != 75 {
		t.Fatalf("重启请求退出码 = %d，期望 75", code)
	}
}

func TestReadinessBarrierWaitsAllCore(t *testing.T) {
	var events []string
	a := New(fastOpts(&events))
	core1 := newFake("core1", &events)
	core2 := newFake("core2", &events)
	degr := newFake("degr", &events) // 不参与 barrier
	a.Register(core1, Core)
	a.Register(core2, Core)
	a.Register(degr, Degradable)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int)
	go func() { done <- a.Run(ctx) }()

	select {
	case <-a.Ready():
		t.Fatal("两个 Core 均未就绪，Ready 不应关闭")
	case <-time.After(30 * time.Millisecond):
	}

	core1.signalReady()
	select {
	case <-a.Ready():
		t.Fatal("core2 未就绪，Ready 不应关闭")
	case <-time.After(20 * time.Millisecond):
	}
	core2.signalReady()
	select {
	case <-a.Ready():
	case <-time.After(time.Second):
		t.Fatal("全部 Core 就绪后 Ready 应关闭")
	}
	cancel()
	<-done
}
