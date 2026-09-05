// Package webapi 是 WebUI HTTP 层：路由、DTO、auth、WebSocket（01 §2.1）。
// T5.1 交付 Server 生命周期壳（app.Service 结构满足 + readiness）；路由/auth
// 由 T5.2/T5.3 逐层完善。
package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"
)

// Server 是 WebServer 服务（01 §1.1 步骤 6：普通注册 service，CORE）。
// 凭结构类型满足 app.Service 与 app.Readiness，不 import app（依赖方向 01 §2.2）。
type Server struct {
	host string
	port int
	log  *slog.Logger

	mu     sync.Mutex
	srv    *http.Server
	ln     net.Listener
	ready  chan struct{}
	start  time.Time
	closed bool
}

// NewServer 构造；port 0 = 内核分配（测试）。log nil = slog.Default()。
func NewServer(host string, port int, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		host:  host,
		port:  port,
		log:   log,
		ready: make(chan struct{}),
		start: time.Now(),
	}
}

// Name 实现 app.Service。
func (s *Server) Name() string { return "webserver" }

// Ready 实现 app.Readiness：监听绑定后关闭（恰好一次）。
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Addr 返回实际监听地址（port 0 时为内核分配值；未监听返回空串）。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Run 阻塞服务直至 ctx 取消（HTTP 层错误 = OWN_FATAL → CORE fatal → exit 1）。
func (s *Server) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("webserver 已关闭，不可重启")
	}
	mux := http.NewServeMux()
	// /api/health 公开无鉴权（01 §1.5；Docker HEALTHCHECK 消费）。
	// status 聚合（degraded/down）与组件细项在 T5.2/T5.3 完善形状。
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":         "ok",
			"version":        versionString(),
			"uptime_seconds": time.Since(s.start).Seconds(),
		})
	})

	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.host, s.port))
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("webserver 监听: %w", err)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.mu.Unlock()

	close(s.ready) // readiness barrier 信号（恰好一次）
	s.log.Info("webserver 监听", "addr", ln.Addr().String())

	// Service 契约：ctx 取消 → 优雅停机（幂等，closeAll 的二次 Shutdown 无害）。
	go func() {
		<-ctx.Done()
		_ = s.Shutdown(context.Background())
	}()

	err = s.srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil // ctx 取消路径：Shutdown 由 Shutdown/closeAll 驱动
	}
	return err
}

// Shutdown 实现 app.Service（幂等）。
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	srv := s.srv
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

func versionString() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "devel"
}
