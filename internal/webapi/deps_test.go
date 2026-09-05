package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
	"github.com/Sakura520222/Sakura-Nexus/internal/forwarding"
)

// ---------- fakes ----------

type fakeSettings struct {
	snap     map[string]map[string]any
	scopes   []string
	updateFn func(scope string, partial map[string]any) error
	updated  []string
}

func (f *fakeSettings) Snapshot(scope string) (map[string]any, error) {
	s, ok := f.snap[scope]
	if !ok {
		return nil, errors.New("未知 scope: " + scope)
	}
	return s, nil
}

func (f *fakeSettings) Update(_ context.Context, scope string, partial map[string]any) error {
	if f.updateFn != nil {
		return f.updateFn(scope, partial)
	}
	f.updated = append(f.updated, scope)
	return nil
}

func (f *fakeSettings) Scopes() []string { return f.scopes }

type fakeEngine struct {
	paused      bool
	pauses      int
	resumes     int
	backfills   [][2]int64 // {ruleID, limit}
	refreshes   int
	backfillRes forwarding.BackfillResult
	backfillErr error
}

func (f *fakeEngine) Paused() bool { return f.paused }
func (f *fakeEngine) Pause()       { f.pauses++; f.paused = true }
func (f *fakeEngine) Resume()      { f.resumes++; f.paused = false }

func (f *fakeEngine) Backfill(_ context.Context, ruleID int64, limit int) (forwarding.BackfillResult, error) {
	f.backfills = append(f.backfills, [2]int64{ruleID, int64(limit)})
	return f.backfillRes, f.backfillErr
}

func (f *fakeEngine) RefreshRules(context.Context) error { f.refreshes++; return nil }

type fakeAuditReader struct {
	entries []domain.AuditLogEntry
}

func (f *fakeAuditReader) List(_ context.Context, limit int) ([]domain.AuditLogEntry, error) {
	if limit > len(f.entries) {
		limit = len(f.entries)
	}
	return f.entries[:limit], nil
}

// newDepsServer 构造注入全依赖的测试服务。
func newDepsServer(t *testing.T) (*Server, *fakeEngine, *fakeSettings, *fakeAuditReader, *atomic.Int32) {
	sink := &fakeAuditSink{}
	eng := &fakeEngine{}
	set := &fakeSettings{
		snap: map[string]map[string]any{
			"ai":         {"api_key": "sk-abcd1234", "model": "gpt-x"},
			"forwarding": {"dedup_days": 30},
		},
		scopes: []string{"system", "forwarding", "logging", "ai"},
	}
	audit := &fakeAuditReader{entries: []domain.AuditLogEntry{
		{ID: 1, Actor: "webui:admin", Action: "auth.login"},
	}}
	restarts := &atomic.Int32{}
	srv := NewServer("127.0.0.1", 0, nil,
		WithCredentials("admin", "pw-secret"),
		WithAuditSink(sink),
		WithDeps(Deps{
			Settings:       set,
			Engine:         eng,
			Audit:          audit,
			RequestRestart: func() { restarts.Add(1) },
			SetLogLevel:    func(string) error { return nil },
		}),
	)
	srv.Handle("GET", "/api/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	return srv, eng, set, audit, restarts
}

// loginAndServe 登录取会话（httptest 直连 handler）。
func loginAndServe(t *testing.T, srv *Server) (*httptest.Server, *http.Client) {
	ts := serve(t, srv)
	jar, _ := cookiejar.New(nil)
	resp := postJSON(t, ts.URL+"/api/auth/login", `{"username":"admin","password":"pw-secret"}`, jar)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("登录失败: %d", resp.StatusCode)
	}
	return ts, &http.Client{Jar: jar}
}

func doJSON(t *testing.T, client *http.Client, method, url, body string) (*http.Response, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestSystemPauseResume(t *testing.T) {
	srv, eng, _, _, _ := newDepsServer(t)
	ts, client := loginAndServe(t, srv)

	resp, _ := doJSON(t, client, "POST", ts.URL+"/api/system/pause", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pause 应 200: %d", resp.StatusCode)
	}
	if eng.pauses != 1 {
		t.Errorf("引擎应收到 Pause: %d", eng.pauses)
	}

	resp, body := doJSON(t, client, "GET", ts.URL+"/api/system/status", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status 应 200: %d", resp.StatusCode)
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatal(err)
	}
	if status["paused"] != true {
		t.Errorf("pause 后 status.paused 应为 true: %v", status["paused"])
	}

	resp, _ = doJSON(t, client, "POST", ts.URL+"/api/system/resume", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resume 应 200: %d", resp.StatusCode)
	}
	if eng.resumes != 1 {
		t.Errorf("引擎应收到 Resume: %d", eng.resumes)
	}
}

func TestSystemRestartTriggersHook(t *testing.T) {
	srv, _, _, _, restarts := newDepsServer(t)
	ts, client := loginAndServe(t, srv)

	resp, _ := doJSON(t, client, "POST", ts.URL+"/api/system/restart", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("restart 应 200: %d", resp.StatusCode)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && restarts.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if restarts.Load() == 0 {
		t.Error("应异步触发重启钩子（exit 75 链）")
	}
}

func TestLogLevelEndpoint(t *testing.T) {
	var got string
	srv := NewServer("127.0.0.1", 0, nil,
		WithCredentials("admin", "pw-secret"),
		WithDeps(Deps{
			Engine:      &fakeEngine{},
			SetLogLevel: func(l string) error { got = l; return nil },
		}),
	)
	ts, client := loginAndServe(t, srv)

	resp, body := doJSON(t, client, "PUT", ts.URL+"/api/system/log-level", `{"level":"debug"}`)
	if resp.StatusCode != http.StatusOK || got != "debug" {
		t.Fatalf("log-level 应生效: %d %q %q", resp.StatusCode, got, body)
	}
	resp, _ = doJSON(t, client, "PUT", ts.URL+"/api/system/log-level", `{"level":"verbose"}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("非法 level 应 422: %d", resp.StatusCode)
	}
}

func TestSettingsGetMaskingAndUnknownScope(t *testing.T) {
	srv, _, _, _, _ := newDepsServer(t)
	ts, client := loginAndServe(t, srv)

	resp, body := doJSON(t, client, "GET", ts.URL+"/api/settings/ai", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("settings/ai 应 200: %d", resp.StatusCode)
	}
	if !strings.Contains(body, "•••1234") {
		t.Errorf("api_key 应脱敏为 •••+尾4: %s", body)
	}
	if strings.Contains(body, "sk-abcd1234") {
		t.Error("明文 secret 不得出现在响应")
	}
	if !strings.Contains(body, "gpt-x") {
		t.Errorf("非 secret 字段应保留: %s", body)
	}

	resp, _ = doJSON(t, client, "GET", ts.URL+"/api/settings/nope", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("未知 scope 应 404: %d", resp.StatusCode)
	}
}

func TestSettingsPutValidation(t *testing.T) {
	srv, _, set, _, _ := newDepsServer(t)
	set.updateFn = func(scope string, partial map[string]any) error {
		return errors.New("dedup_days 不能为负: -1")
	}
	ts, client := loginAndServe(t, srv)

	resp, body := doJSON(t, client, "PUT", ts.URL+"/api/settings/forwarding", `{"dedup_days":-1}`)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("校验失败应 422: %d", resp.StatusCode)
	}
	if !strings.Contains(body, "VALIDATION_ERROR") || !strings.Contains(body, "dedup_days") {
		t.Errorf("422 载荷应含字段错误: %s", body)
	}

	set.updateFn = nil
	resp, _ = doJSON(t, client, "PUT", ts.URL+"/api/settings/forwarding", `{"dedup_days":7}`)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("合法更新应 200: %d", resp.StatusCode)
	}
	if len(set.updated) != 1 || set.updated[0] != "forwarding" {
		t.Errorf("Update 应收到 partial: %+v", set.updated)
	}
}

func TestAuditLogsEndpoint(t *testing.T) {
	srv, _, _, _, _ := newDepsServer(t)
	ts, client := loginAndServe(t, srv)

	resp, body := doJSON(t, client, "GET", ts.URL+"/api/system/audit-logs?limit=1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("audit-logs 应 200: %d", resp.StatusCode)
	}
	if !strings.Contains(body, `"action":"auth.login"`) {
		t.Errorf("应返回审计条目: %s", body)
	}
}

// 变异验证对象：脱敏与 422 行为（移除脱敏 → 测试必须红）。
func TestMaskSecretMutation(t *testing.T) {
	if got := maskSecret("sk-abcd1234"); got != "•••1234" {
		t.Errorf("maskSecret 不符: %q", got)
	}
	if got := maskSecret("abc"); got != "•••" {
		t.Errorf("短 secret 应全遮: %q", got)
	}
}
