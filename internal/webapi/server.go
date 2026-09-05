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

	username  string
	password  string
	auditSink AuditSink
	now       func() time.Time

	sessions *sessionStore
	limiter  *loginLimiter
	routes   []route
	deps     *Deps

	mu     sync.Mutex
	srv    *http.Server
	ln     net.Listener
	ready  chan struct{}
	start  time.Time
	closed bool
}

// ServerOption 是可选装配项（凭据/审计/时钟注入）。
type ServerOption func(*Server)

// WithCredentials 注入 .env WebUI 凭据（04 §4：恒时比较的比对源）。
// 未注入 = auth 未配置，除公开路由外全部 401（fail-closed）。
func WithCredentials(username, password string) ServerOption {
	return func(s *Server) { s.username, s.password = username, password }
}

// WithAuditSink 注入审计写入面（04 §2：全部写操作落 system_audit_logs）。
func WithAuditSink(sink AuditSink) ServerOption {
	return func(s *Server) { s.auditSink = sink }
}

// WithNow 注入时钟（会话过期/锁定窗口测试）。
func WithNow(now func() time.Time) ServerOption {
	return func(s *Server) { s.now = now }
}

type route struct {
	method  string
	pattern string
	h       http.HandlerFunc
}

// NewServer 构造；port 0 = 内核分配（测试）。log nil = slog.Default()。
func NewServer(host string, port int, log *slog.Logger, opts ...ServerOption) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		host:  host,
		port:  port,
		log:   log,
		ready: make(chan struct{}),
		start: time.Now(),
		now:   time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.sessions = newSessionStore(s.now)
	s.limiter = newLoginLimiter(s.now)
	return s
}

// Handle 注册受会话保护的业务路由（写方法自动追加审计；04 §2）。
// 须在 Run 之前注册（Run 组装路由快照）。
func (s *Server) Handle(method, pattern string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.log.Warn("路由注册晚于关闭，忽略", "pattern", pattern)
		return
	}
	s.routes = append(s.routes, route{method: method, pattern: pattern, h: h})
}

// handler 组装路由树：公开 = health/login（04 §4 豁免）；其余一律会话保护。
func (s *Server) handler() http.Handler {
	s.mu.Lock()
	defer s.mu.Unlock()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.requireSession(s.handleLogout))
	mux.HandleFunc("GET /api/auth/status", s.requireSession(s.handleStatus))
	for _, rt := range s.routes {
		h := s.requireSession(s.auditWrap(rt.method, rt.pattern, rt.h))
		mux.HandleFunc(rt.method+" "+rt.pattern, h)
	}
	return mux
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
	s.mu.Unlock()

	h := s.handler() // handler() 内部取锁——必须在 s.mu 临界区外组装
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.host, s.port))
	if err != nil {
		return fmt.Errorf("webserver 监听: %w", err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	s.srv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}

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

// handleHealth 公开无鉴权（01 §1.5；Docker HEALTHCHECK 消费）。
// status 聚合（degraded/down）在 T5.3 接入组件状态后完善。
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":         "ok",
		"version":        versionString(),
		"uptime_seconds": time.Since(s.start).Seconds(),
	})
}

func versionString() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "devel"
}
