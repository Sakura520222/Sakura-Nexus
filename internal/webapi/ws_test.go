package webapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Sakura520222/Sakura-Nexus/internal/logging"
)

func TestWSLogStream(t *testing.T) {
	ring := logging.NewRing(16)
	lg := slog.New(ring)
	lg.Info("快照日志") // 回放内容

	srv := NewServer("127.0.0.1", 0, nil,
		WithCredentials("admin", "pw-secret"),
		WithLogRing(ring),
	)
	ts, client := loginAndServe(t, srv)

	// 从 jar 取会话 token，经 Cookie 头直传（coder/websocket 的自定义
	// HTTPClient 对 101 的透传在本组合下返回 501，见测试注释）。
	var token string
	u, perr := url.Parse(ts.URL)
	if perr != nil {
		t.Fatal(perr)
	}
	for _, c := range client.Jar.Cookies(u) {
		if c.Name == "sn_session" {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("会话 Cookie 缺失")
	}

	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dcancel()
	conn, _, err := websocket.Dial(dctx, "ws://"+ts.Listener.Addr().String()+"/api/ws",
		&websocket.DialOptions{HTTPHeader: http.Header{"Cookie": []string{"sn_session=" + token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	readMsg := func() (string, error) {
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		_, data, err := conn.Read(rctx)
		return string(data), err
	}

	// ① 快照回放。
	msg, err := readMsg()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "快照日志") {
		t.Fatalf("应先回放快照: %s", msg)
	}

	// ② 实时推送。
	lg.Warn("实时日志", "component", "bot")
	msg, err = readMsg()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "实时日志") || !strings.Contains(msg, `"component":"bot"`) {
		t.Fatalf("实时推送不符: %s", msg)
	}

	// ③ subscribe 过滤（components=["engine"] → bot 组件被滤除）。
	// subscribe 后 ping→pong 同步：读泵串行消费，pong 到达即订阅已生效。
	_ = conn.Write(dctx, websocket.MessageText, []byte(`{"type":"subscribe","components":["engine"]}`))
	_ = conn.Write(dctx, websocket.MessageText, []byte(`{"type":"ping"}`))
	msg, err = readMsg()
	if err != nil || !strings.Contains(msg, `"type":"ping"`) {
		t.Fatalf("subscribe 同步 pong 缺失: %s %v", msg, err)
	}
	lg.Warn("bot 组件日志", "component", "bot")
	lg.Info("engine 组件日志", "component", "engine")
	msg, err = readMsg()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg, "bot 组件日志") {
		t.Fatalf("bot 组件不应通过过滤: %s", msg)
	}
	if !strings.Contains(msg, "engine 组件日志") {
		t.Fatalf("engine 组件应通过: %s", msg)
	}

	// ④ ping→pong。
	_ = conn.Write(dctx, websocket.MessageText, []byte(`{"type":"ping"}`))
	msg, err = readMsg()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, `"type":"ping"`) {
		t.Fatalf("应收到 pong: %s", msg)
	}
}

func TestWSRequiresSession(t *testing.T) {
	ring := logging.NewRing(4)
	srv := NewServer("127.0.0.1", 0, nil,
		WithCredentials("admin", "pw-secret"),
		WithLogRing(ring),
	)
	ts := serve(t, srv)

	// 无会话 → HTTP 401（升级前拒绝）。
	resp, err := http.Get("http://" + ts.Listener.Addr().String() + "/api/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无会话 WS 应 401: %d", resp.StatusCode)
	}

	// 会话有效（jar 登录）但跨域 Origin → 403。
	jar, _ := cookiejar.New(nil)
	resp2 := postJSON(t, "http://"+ts.Listener.Addr().String()+"/api/auth/login",
		`{"username":"admin","password":"pw-secret"}`, jar)
	_ = resp2.Body.Close()
	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()
	_, _, err = websocket.Dial(cctx, "ws://"+ts.Listener.Addr().String()+"/api/ws",
		&websocket.DialOptions{
			HTTPClient: &http.Client{Jar: jar},
			HTTPHeader: http.Header{"Origin": []string{"https://evil.example"}},
		})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("跨域 Origin 应 403: %v", err)
	}
}
