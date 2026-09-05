package logging

import (
	"log/slog"
	"testing"
	"time"
)

func TestRingHandlerStoresAndSnapshots(t *testing.T) {
	ring := NewRing(4)
	lg := slog.New(ring)

	for i := 0; i < 6; i++ {
		lg.Info("消息", "component", "engine", "n", i)
	}
	snap := ring.Snapshot()
	if len(snap) != 4 {
		t.Fatalf("环形容量 4 应只留最近 4 条，得 %d", len(snap))
	}
	// 最早两条被挤出。
	if snap[0].Message != "消息" {
		t.Fatalf("消息体不符: %+v", snap[0])
	}
	// component 属性提升为字段，且不重复出现在 attrs。
	if snap[0].Component != "engine" {
		t.Errorf("component 应提升: %+v", snap[0])
	}
	if snap[3].Level != slog.LevelInfo {
		t.Errorf("level 不符: %+v", snap[3])
	}
	if snap[3].Time.IsZero() {
		t.Error("时间不应为零值")
	}
}

func TestRingHandlerSubscribesBroadcastsAndDropsSlow(t *testing.T) {
	ring := NewRing(8)
	sub1 := ring.Subscribe()
	sub2 := ring.Subscribe()

	lg := slog.New(ring)
	lg.Warn("广播一条", "component", "bot")

	for _, sub := range []<-chan Record{sub1, sub2} {
		select {
		case rec := <-sub:
			if rec.Message != "广播一条" || rec.Level != slog.LevelWarn || rec.Component != "bot" {
				t.Errorf("订阅流不符: %+v", rec)
			}
		case <-time.After(time.Second):
			t.Fatal("订阅者应收到广播")
		}
	}

	// 慢消费者不阻塞发布：灌满后直接再写不悬挂。
	for i := 0; i < cap(sub1)+8; i++ {
		lg.Info("洪泛", "component", "x")
	}
	// 订阅 channel 持续可读（丢弃中间值但保留最新写入机会）。
	deadline := time.After(time.Second)
	for {
		select {
		case <-sub1:
		case <-deadline:
			t.Fatal("洪泛后订阅流不应枯竭或阻塞发布方")
		default:
			goto done
		}
	}
done:
	ring.Close()
}

func TestNamedLoggerAddsComponent(t *testing.T) {
	ring := NewRing(2)
	lg := Named(slog.New(ring), "forwarding")
	lg.Info("组件日志")
	snap := ring.Snapshot()
	if len(snap) != 1 || snap[0].Component != "forwarding" {
		t.Fatalf("Named 应注入 component 字段: %+v", snap)
	}
}
