package availability

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFlipCycle(t *testing.T) {
	tr := NewTracker(Unavailable)
	if tr.IsReady() {
		t.Fatal("初始应为 Unavailable")
	}

	// 循环翻转：连接→断线→重连→再断线→再连接（01 §1.3：可重复表达循环）
	ch := tr.SubscribeState()
	for _, v := range []State{Ready, Unavailable, Ready} {
		tr.SetReady(v)
		select {
		case got := <-ch:
			if got != v {
				t.Fatalf("翻转 %v 时订阅收到 %v", v, got)
			}
		default:
			// 容量 1 + 排空语义下应总能读到最新一次翻转
			t.Fatalf("翻转 %v 后订阅未收到", v)
		}
	}

	tr.Close()
	// 关闭后订阅 channel 关闭
	if _, ok := <-ch; ok {
		t.Fatal("Close 后订阅 channel 应关闭")
	}
}

func TestWaitReadyUnblocks(t *testing.T) {
	tr := NewTracker(Unavailable)
	go func() {
		time.Sleep(20 * time.Millisecond)
		tr.SetReady(Ready)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tr.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
}

func TestWaitReadyCtxCancel(t *testing.T) {
	tr := NewTracker(Unavailable)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := tr.WaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应返回 DeadlineExceeded: %v", err)
	}
}

func TestWaitReadyAfterClose(t *testing.T) {
	tr := NewTracker(Unavailable)
	tr.Close()
	if err := tr.WaitReady(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后应返回 ErrClosed: %v", err)
	}
}

func TestSetReadyNoFlipNoSpam(t *testing.T) {
	tr := NewTracker(Ready)
	ch := tr.SubscribeState()
	tr.SetReady(Ready) // 同值不算翻转
	select {
	case v := <-ch:
		t.Fatalf("同值不应发送: %v", v)
	default:
	}
}
