package webapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// 会话鉴权与失败防护（04 §4 冻结：server-side opaque session，不用 JWT）：
// crypto/rand 256-bit token、内存 store（重启即失效）、会话固定 12h、
// 同 IP 5 次失败锁 10 分钟、IP 取真实 RemoteAddr（无 trusted proxy 配置，
// 不采信 X-Forwarded-For）、成功登录写审计。
const (
	sessionCookieName = "sn_session"
	sessionTTL        = 12 * time.Hour
	maxLoginFailures  = 5
	lockoutDuration   = 10 * time.Minute
)

// AuditSink 是审计写入最小面（mysql.AuditRepo 凭结构类型满足；01 §2.3）。
type AuditSink interface {
	Append(ctx context.Context, e domain.AuditEntry) error
}

// ---------- 会话存储 ----------

type sessionStore struct {
	mu       sync.Mutex
	now      func() time.Time
	sessions map[string]time.Time // token → 过期时刻
}

func newSessionStore(now func() time.Time) *sessionStore {
	return &sessionStore{now: now, sessions: map[string]time.Time{}}
}

func (s *sessionStore) create() (string, time.Time) {
	var b [32]byte // 256-bit
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败属系统级异常：宁可拒绝登录也不降级为弱 token
		panic(fmt.Errorf("webapi: crypto/rand: %w", err))
	}
	token := base64.RawURLEncoding.EncodeToString(b[:])
	expiry := s.now().Add(sessionTTL)
	s.mu.Lock()
	s.sessions[token] = expiry
	s.mu.Unlock()
	return token, expiry
}

func (s *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.sessions[token]
	if !ok {
		return false
	}
	if !s.now().Before(expiry) {
		delete(s.sessions, token) // 惰性清理过期会话
		return false
	}
	return true
}

func (s *sessionStore) drop(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// ---------- 登录失败锁定 ----------

type loginLimiter struct {
	mu    sync.Mutex
	now   func() time.Time
	fails map[string]*failState
}

type failState struct {
	count       int
	lockedUntil time.Time
}

func newLoginLimiter(now func() time.Time) *loginLimiter {
	return &loginLimiter{now: now, fails: map[string]*failState{}}
}

func (l *loginLimiter) locked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.fails[ip]
	if !ok {
		return false
	}
	if l.now().Before(st.lockedUntil) {
		return true
	}
	if !st.lockedUntil.IsZero() {
		delete(l.fails, ip) // 锁定过期：全新窗口
	}
	return false
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.fails[ip]
	if st == nil {
		st = &failState{}
		l.fails[ip] = st
	}
	st.count++
	if st.count >= maxLoginFailures {
		st.lockedUntil = l.now().Add(lockoutDuration)
		st.count = 0
	}
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}

// clientIP 取真实 TCP 来源（无 trusted proxy 配置：不采信代理头，04 §4）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- 处理器 ----------

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.limiter.locked(ip) {
		s.writeError(w, http.StatusTooManyRequests, "AUTH_LOCKED", "失败次数过多，账号来源已锁定 10 分钟")
		return
	}
	var req loginRequest
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", "请求体非法")
		return
	}
	// 凭据比较恒时（04 §4；.env 凭据在构造期注入）。
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(s.username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(req.Password), []byte(s.password)) == 1
	if !userOK || !passOK || req.Username == "" || req.Password == "" {
		s.limiter.fail(ip)
		s.writeError(w, http.StatusUnauthorized, "AUTH_FAILED", "用户名或密码错误")
		return
	}
	s.limiter.reset(ip)

	token, expiry := s.sessions.create()
	// Secure 标志依 TLS 连接判定（04 §4「Secure（TLS 时）」）。
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil,
	})
	s.audit(r, "auth.login")
	s.writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "expires_at": expiry.UTC().Format(time.RFC3339)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		s.sessions.drop(c.Value)
	}
	// 清除客户端 Cookie（Max-Age<0 即时过期）。
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
	s.audit(r, "auth.logout")
	s.writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
}

// requireSession 是 Cookie 会话中间件（04 §4；豁免路由不经此包装）。
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil || !s.sessions.valid(c.Value) {
			s.writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "需要登录")
			return
		}
		next(w, r)
	}
}

// auditWrap 为注册路由的写操作追加审计（04 §2：全部写操作落 system_audit_logs）。
func (s *Server) auditWrap(method, pattern string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r)
		if isWriteMethod(method) {
			s.auditAction(r, strings.ToLower(method)+" "+pattern, rec.status)
		}
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) audit(r *http.Request, action string) { s.auditAction(r, action, http.StatusOK) }

func (s *Server) auditAction(_ *http.Request, action string, status int) {
	if s.auditSink == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 审计失败不阻断请求（记日志交由运维发现）。
	if err := s.auditSink.Append(ctx, domain.AuditEntry{
		Actor:  "webui:" + s.username,
		Action: action,
		Detail: map[string]any{"status": status},
	}); err != nil {
		s.log.Warn("审计写入失败", "action", action, "err", err)
	}
}

func isWriteMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	}
	return false
}

// ---------- 响应工具（DTO 约定 04 §3） ----------

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, code int, errCode, msg string) {
	s.writeJSON(w, code, map[string]any{
		"error": map[string]any{"code": errCode, "message": msg},
	})
}
