package webapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// newAuthServer 构造接测试凭据与可注入时钟的服务（未启动监听，经
// httptest Serve 复用 handler）。
func newAuthServer(t *testing.T, now *time.Time) (*Server, *fakeAuditSink) {
	sink := &fakeAuditSink{}
	srv := NewServer("127.0.0.1", 0, nil,
		WithCredentials("admin", "pw-secret"),
		WithAuditSink(sink),
		WithNow(func() time.Time { return *now }),
	)
	srv.Handle("GET", "/api/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	return srv, sink
}

// serve 起 httptest 服务复用 Server 的路由与中间件（绕过自管 listener）。
func serve(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return ts
}

type fakeAuditSink struct {
	entries []domain.AuditEntry
}

func (f *fakeAuditSink) Append(_ context.Context, e domain.AuditEntry) error {
	f.entries = append(f.entries, e)
	return nil
}

func postJSON(t *testing.T, url, body string, jar *cookiejar.Jar) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{}
	if jar != nil {
		client.Jar = jar
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestLoginSuccessSetsSessionCookie(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	srv, _ := newAuthServer(t, &now)
	ts := serve(t, srv)

	resp := postJSON(t, ts.URL+"/api/auth/login", `{"username":"admin","password":"pw-secret"}`, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录应成功: %d", resp.StatusCode)
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应设置一枚 Cookie: %v", cookies)
	}
	c := cookies[0]
	if c.Name != "sn_session" || c.Value == "" {
		t.Fatalf("会话 Cookie 不符: %v", c)
	}
	if !c.HttpOnly {
		t.Error("Cookie 必须 HttpOnly（04 §4）")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Error("Cookie 必须 SameSite=Strict（04 §4）")
	}
	if c.Path != "/" {
		t.Error("Cookie Path 应为 /")
	}
	if c.MaxAge != 12*3600 {
		t.Errorf("会话固定 12h，得 Max-Age=%d", c.MaxAge)
	}
}

func TestLoginFailureAndLockout(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	srv, _ := newAuthServer(t, &now)
	ts := serve(t, srv)

	// 前 5 次失败各 401（「5 次失败锁 10 分钟」：第 5 次失败处理后锁定）。
	for i := 0; i < 5; i++ {
		resp := postJSON(t, ts.URL+"/api/auth/login", `{"username":"admin","password":"wrong"}`, nil)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应 401: %d", i+1, resp.StatusCode)
		}
	}
	// 第 6 次起（无论凭据对错）锁定 429。
	resp := postJSON(t, ts.URL+"/api/auth/login", `{"username":"admin","password":"wrong"}`, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("5 次失败后应 429 锁定: %d", resp.StatusCode)
	}
	// 锁定期内正确凭据也拒绝。
	resp = postJSON(t, ts.URL+"/api/auth/login", `{"username":"admin","password":"pw-secret"}`, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("锁定期正确凭据应 429: %d", resp.StatusCode)
	}
	// 锁定 10 分钟后解锁。
	now = now.Add(10 * time.Minute)
	resp = postJSON(t, ts.URL+"/api/auth/login", `{"username":"admin","password":"pw-secret"}`, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("解锁后应成功: %d", resp.StatusCode)
	}
}

func TestProtectedRoutesRequireSession(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	srv, _ := newAuthServer(t, &now)
	ts := serve(t, srv)

	// 无会话：受保护路由 401（含 /api/auth/status 与测试注册路由）。
	resp, err := http.Get(ts.URL + "/api/auth/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无会话 status 应 401: %d", resp.StatusCode)
	}
	resp, err = http.Get(ts.URL + "/api/ping")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无会话受保护路由应 401: %d", resp.StatusCode)
	}

	// health 豁免鉴权（01 §1.5）。
	resp, err = http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health 应公开: %d", resp.StatusCode)
	}

	// 登录后携带会话访问。
	jar, _ := cookiejar.New(nil)
	resp = postJSON(t, ts.URL+"/api/auth/login", `{"username":"admin","password":"pw-secret"}`, jar)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录失败: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/ping", nil)
	client := &http.Client{Jar: jar}
	hresp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = hresp.Body.Close()
	if hresp.StatusCode != http.StatusOK {
		t.Fatalf("携带会话应 200: %d", hresp.StatusCode)
	}
	// status 载荷 authenticated=true。
	sresp, err := client.Get(ts.URL + "/api/auth/status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sresp.Body.Close() }()
	var body struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.NewDecoder(sresp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Authenticated {
		t.Error("status 应 authenticated=true")
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	srv, _ := newAuthServer(t, &now)
	ts := serve(t, srv)

	jar, _ := cookiejar.New(nil)
	resp := postJSON(t, ts.URL+"/api/auth/login", `{"username":"admin","password":"pw-secret"}`, jar)
	_ = resp.Body.Close()

	client := &http.Client{Jar: jar}
	lresp, err := client.Post(ts.URL+"/api/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = lresp.Body.Close()
	if lresp.StatusCode != http.StatusOK {
		t.Fatalf("登出应 200: %d", lresp.StatusCode)
	}

	sresp, err := client.Get(ts.URL + "/api/auth/status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sresp.Body.Close() }()
	if sresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("登出后会话应失效: %d", sresp.StatusCode)
	}
}

func TestSessionExpiryAfter12h(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	srv, _ := newAuthServer(t, &now)
	ts := serve(t, srv)

	jar, _ := cookiejar.New(nil)
	resp := postJSON(t, ts.URL+"/api/auth/login", `{"username":"admin","password":"pw-secret"}`, jar)
	_ = resp.Body.Close()

	now = now.Add(12*time.Hour + time.Minute) // 越过会话固定时长
	client := &http.Client{Jar: jar}
	sresp, err := client.Get(ts.URL + "/api/auth/status")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sresp.Body.Close() }()
	if sresp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("12h 后会话应过期: %d", sresp.StatusCode)
	}
}

func TestAuditWritesOnLoginAndLogout(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	srv, sink := newAuthServer(t, &now)
	ts := serve(t, srv)

	jar, _ := cookiejar.New(nil)
	resp := postJSON(t, ts.URL+"/api/auth/login", `{"username":"admin","password":"pw-secret"}`, jar)
	_ = resp.Body.Close()
	client := &http.Client{Jar: jar}
	lresp, err := client.Post(ts.URL+"/api/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = lresp.Body.Close()

	if len(sink.entries) != 2 {
		t.Fatalf("登录+登出应各写一条审计，得 %d: %+v", len(sink.entries), sink.entries)
	}
	if sink.entries[0].Actor != "webui:admin" {
		t.Errorf("actor 约定 webui:<username>: %q", sink.entries[0].Actor)
	}
	if sink.entries[0].Action != "auth.login" || sink.entries[1].Action != "auth.logout" {
		t.Errorf("action 不符: %+v", sink.entries)
	}
}
