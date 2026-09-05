package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestServerLifecycleAndHealth(t *testing.T) {
	srv := NewServer("127.0.0.1", 0, nil) // port 0 = 内核分配（测试）
	if srv.Name() != "webserver" {
		t.Errorf("Name 不符: %s", srv.Name())
	}

	runErr := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { runErr <- srv.Run(ctx) }()

	select {
	case <-srv.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("监听就绪超时")
	}

	// /api/health 公开无鉴权（01 §1.5；完整组件聚合在 T5.2 完善形状）。
	addr := srv.Addr()
	resp, err := http.Get("http://" + addr + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health 状态码: %d", resp.StatusCode)
	}
	var body struct {
		Status  string  `json:"status"`
		Version string  `json:"version"`
		Uptime  float64 `json:"uptime_seconds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Version == "" || body.Uptime < 0 {
		t.Errorf("health 载荷不符: %+v", body)
	}

	// 优雅关闭：cancel → Serve 返回 ErrServerClosed → Run 返回 nil。
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("优雅关闭应返回 nil: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("关闭超时")
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("二次 Shutdown 应幂等: %v", err)
	}
}
