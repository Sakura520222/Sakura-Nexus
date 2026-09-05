package botapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newFakeServer 起一个 Bot API 形状的 fake server：记录最近一次请求
// （路径/头/体），按脚本逐次回放响应；脚本耗尽后重复最后一条。
type fakeServer struct {
	mu       sync.Mutex
	srv      *httptest.Server
	script   []fakeResp
	calls    int
	lastPath string
	lastCT   string
	lastBody string
}

type fakeResp struct {
	status int
	body   string
}

func newFakeServer(script ...fakeResp) *fakeServer {
	fs := &fakeServer{script: script}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		fs.mu.Lock()
		i := fs.calls
		if i >= len(fs.script) {
			i = len(fs.script) - 1
		}
		fs.calls++
		fs.lastPath, fs.lastCT, fs.lastBody = r.URL.Path, r.Header.Get("Content-Type"), string(b)
		resp := fs.script[i]
		fs.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}))
	return fs
}

func (fs *fakeServer) Close() { fs.srv.Close() }

func (fs *fakeServer) url() string { return fs.srv.URL }

func (fs *fakeServer) callCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.calls
}

func (fs *fakeServer) snapshot() (path, ct, body string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.lastPath, fs.lastCT, fs.lastBody
}

// newTestClient 构造接 fake server 的客户端，注入假时钟（记录每次入睡时长）。
func newTestClient(fs *fakeServer, sleeps *[]time.Duration) *Client {
	c := NewClient("123:SECRET", Options{BaseURL: fs.url()})
	if sleeps != nil {
		c.sleep = func(_ context.Context, d time.Duration) bool {
			*sleeps = append(*sleeps, d)
			return true
		}
	}
	return c
}

func TestCallSuccessPostsJSONToTokenPath(t *testing.T) {
	fs := newFakeServer(fakeResp{200, `{"ok":true,"result":{"message_id":42}}`})
	defer fs.Close()
	c := newTestClient(fs, nil)

	var result struct {
		MessageID int64 `json:"message_id"`
	}
	params := map[string]any{"chat_id": json.RawMessage("-1001234567890")}
	if err := c.Call(context.Background(), "sendRichMessage", params, &result); err != nil {
		t.Fatalf("成功路径不应报错: %v", err)
	}
	if result.MessageID != 42 {
		t.Errorf("result 解码不符: %+v", result)
	}
	path, ct, body := fs.snapshot()
	if path != "/bot123:SECRET/sendRichMessage" {
		t.Errorf("请求路径不符: %s", path)
	}
	if ct != "application/json" {
		t.Errorf("Content-Type 不符: %s", ct)
	}
	if !strings.Contains(body, `"chat_id":-1001234567890`) {
		t.Errorf("请求体应透传参数: %s", body)
	}
	if fs.callCount() != 1 {
		t.Errorf("成功不应重试，实际 %d 次", fs.callCount())
	}
}

func TestCallRetries429ObeyingRetryAfter(t *testing.T) {
	// §1.4：429 + retry_after → 服从 retry_after + 1s，重试上限 3 次。
	fs := newFakeServer(
		fakeResp{429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":2}}`},
		fakeResp{200, `{"ok":true,"result":{"message_id":7}}`},
	)
	defer fs.Close()
	var slept []time.Duration
	c := newTestClient(fs, &slept)

	var result struct {
		MessageID int64 `json:"message_id"`
	}
	if err := c.Call(context.Background(), "sendRichMessage", map[string]any{"chat_id": 1}, &result); err != nil {
		t.Fatalf("429 后重试应成功: %v", err)
	}
	if result.MessageID != 7 {
		t.Errorf("result 解码不符: %+v", result)
	}
	if fs.callCount() != 2 {
		t.Errorf("应恰好重试一次，实际 %d 次", fs.callCount())
	}
	if len(slept) != 1 || slept[0] != 3*time.Second {
		t.Errorf("应入睡 retry_after+1s=3s，实际 %v", slept)
	}
}

func TestCallGivesUp429AfterThreeRetries(t *testing.T) {
	fs := newFakeServer(
		fakeResp{429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":5}}`},
	)
	defer fs.Close()
	var slept []time.Duration
	c := newTestClient(fs, &slept)

	err := c.Call(context.Background(), "sendRichMessage", map[string]any{"chat_id": 1}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 429 {
		t.Fatalf("重试耗尽应返回 APIError(429)，得 %v", err)
	}
	if fs.callCount() != 4 {
		t.Errorf("初次 + 3 次重试 = 4 次，实际 %d 次", fs.callCount())
	}
	if len(slept) != 3 {
		t.Fatalf("应入睡 3 次，实际 %v", slept)
	}
	for i, d := range slept {
		if d != 6*time.Second {
			t.Errorf("第 %d 次入睡应为 retry_after+1s=6s，实际 %v", i+1, d)
		}
	}
}

func TestCallRetries5xxWithExponentialBackoff(t *testing.T) {
	// §1.4：5xx → 指数退避 1/2/4s，上限 3 次重试。
	fs := newFakeServer(
		fakeResp{500, `{"ok":false,"error_code":500,"description":"internal"}`},
		fakeResp{503, `{"ok":false,"error_code":503,"description":"unavailable"}`},
		fakeResp{200, `{"ok":true,"result":{"message_id":9}}`},
	)
	defer fs.Close()
	var slept []time.Duration
	c := newTestClient(fs, &slept)

	if err := c.Call(context.Background(), "sendRichMessage", nil, nil); err != nil {
		t.Fatalf("第三次尝试应成功: %v", err)
	}
	if fs.callCount() != 3 {
		t.Errorf("应 3 次尝试，实际 %d 次", fs.callCount())
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second}
	if len(slept) != len(want) {
		t.Fatalf("应入睡 2 次，实际 %v", slept)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Errorf("第 %d 次入睡应 %v，实际 %v", i+1, want[i], slept[i])
		}
	}
}

func TestCall5xxExhaustsBackoffSequence(t *testing.T) {
	fs := newFakeServer(
		fakeResp{500, `{"ok":false,"error_code":500,"description":"internal"}`},
	)
	defer fs.Close()
	var slept []time.Duration
	c := newTestClient(fs, &slept)

	err := c.Call(context.Background(), "sendRichMessage", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 500 {
		t.Fatalf("重试耗尽应返回 APIError(500)，得 %v", err)
	}
	if fs.callCount() != 4 {
		t.Errorf("初次 + 3 次重试 = 4 次，实际 %d 次", fs.callCount())
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second}
	if len(slept) != len(want) {
		t.Fatalf("应入睡 3 次，实际 %v", slept)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Errorf("第 %d 次入睡应 %v，实际 %v", i+1, want[i], slept[i])
		}
	}
}

func TestCallRetriesNetworkErrorAndSanitizesToken(t *testing.T) {
	// §1.4：网络错误与 5xx 同行（1/2/4s）；06 §5：错误文本不得含 token
	// （url.Error 内嵌完整 URL，token 就在其中）。
	fs := newFakeServer(fakeResp{200, `{"ok":true,"result":{}}`})
	c := NewClient("123:SECRET", Options{BaseURL: fs.url()})
	fs.Close() // 关闭服务制造 connect refused
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) bool {
		slept = append(slept, d)
		return true
	}

	err := c.Call(context.Background(), "sendRichMessage", nil, nil)
	if err == nil {
		t.Fatal("服务已关，应失败")
	}
	if len(slept) != 3 {
		t.Errorf("网络错误应重试 3 次（入睡 %v）", slept)
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "bot123") {
		t.Errorf("错误文本泄漏 token: %v", err)
	}
	if strings.Contains(err.Error(), fs.url()) {
		t.Errorf("错误文本泄漏完整 URL: %v", err)
	}
}

func TestCallDoesNotRetry4xx(t *testing.T) {
	// 4xx 业务错误（如 400 formatting reject / 404 method-not-supported）
	// 不重试；Code/Description 完整透出——T4.3 lazy capability detection
	// 按 Telegram 错误语义判定（03 §2.9），依赖此处信息不被吞掉。
	fs := newFakeServer(fakeResp{404, `{"ok":false,"error_code":404,"description":"Not Found"}`})
	defer fs.Close()
	c := newTestClient(fs, nil)

	err := c.Call(context.Background(), "sendRichMessage", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("应返回 APIError，得 %v", err)
	}
	if apiErr.Code != 404 || apiErr.Description != "Not Found" || apiErr.Method != "sendRichMessage" {
		t.Errorf("APIError 字段不符: %+v", apiErr)
	}
	if fs.callCount() != 1 {
		t.Errorf("4xx 不应重试，实际 %d 次", fs.callCount())
	}
}

func TestCall429FallsBackToRetryAfterHeader(t *testing.T) {
	// body 无 parameters.retry_after 时回退 Retry-After 头。
	fs := newFakeServer(
		fakeResp{429, `{"ok":false,"error_code":429,"description":"Too Many Requests"}`},
		fakeResp{200, `{"ok":true,"result":{}}`},
	)
	defer fs.Close()
	var slept []time.Duration
	c := newTestClient(fs, &slept)

	if err := c.Call(context.Background(), "sendRichMessage", nil, nil); err != nil {
		t.Fatalf("重试应成功: %v", err)
	}
	if len(slept) != 1 || slept[0] != time.Second {
		t.Errorf("头回退 retry_after 缺省应入睡 0+1s，实际 %v", slept)
	}
}

func TestCallContextCancelDuringRetrySleep(t *testing.T) {
	fs := newFakeServer(fakeResp{429, `{"ok":false,"error_code":429,"description":"rate","parameters":{"retry_after":5}}`})
	defer fs.Close()
	c := newTestClient(fs, nil)
	c.sleep = func(ctx context.Context, _ time.Duration) bool { return false } // 模拟 ctx 取消

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Call(ctx, "sendRichMessage", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("重试入睡被取消应返回 ctx.Err()，得 %v", err)
	}
}

func TestCallLogsContainNoToken(t *testing.T) {
	// 06 §5：HTTP 日志不打印完整 URL（token 在 path 中）。
	var buf safeBuffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	fs := newFakeServer(fakeResp{200, `{"ok":true,"result":{}}`})
	c := NewClient("123:SECRET", Options{BaseURL: fs.url(), Log: lg})
	fs.Close() // 走传输错误路径——token 泄漏风险真正所在（url.Error 内嵌 URL）
	c.sleep = func(context.Context, time.Duration) bool { return true }

	if err := c.Call(context.Background(), "sendRichMessage", nil, nil); err == nil {
		t.Fatal("服务已关，应失败")
	}
	if out := buf.String(); strings.Contains(out, "SECRET") || strings.Contains(out, "/bot") {
		t.Errorf("日志泄漏 token/URL: %s", out)
	}
}

type safeBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

func TestDefaultHTTPClientTimeout30s(t *testing.T) {
	// 03 §2.8：超时 30s（默认传输；注入 HTTPClient 时不覆盖）。
	c := NewClient("t", Options{})
	if c.http.Timeout != 30*time.Second {
		t.Errorf("默认超时应为 30s，实际 %v", c.http.Timeout)
	}
	injected := &http.Client{Timeout: time.Hour}
	c2 := NewClient("t", Options{HTTPClient: injected})
	if c2.http != injected {
		t.Error("注入 HTTPClient 应原样保留")
	}
}

// leakyRT 模拟底层传输返回含完整 URL（含 token）的错误文本——真实场景中
// url.Error.Err 可能携带下层嵌入的 URL。
type leakyRT struct{}

func (leakyRT) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New(`Post "https://api.telegram.org/bot123:SECRET/sendRichMessage": connection refused`)
}

func TestCallRedactsTokenFromInjectedTransportError(t *testing.T) {
	c := NewClient("123:SECRET", Options{HTTPClient: &http.Client{Transport: leakyRT{}}})
	c.sleep = func(context.Context, time.Duration) bool { return true }

	err := c.Call(context.Background(), "sendRichMessage", nil, nil)
	if err == nil {
		t.Fatal("应失败")
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("错误文本泄漏 token: %v", err)
	}
}
