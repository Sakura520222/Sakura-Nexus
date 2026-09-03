package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openai/openai-go/option"
)

// fakeRT 记录请求并按脚本回放响应（状态码 + 可选 JSON body）。
type fakeRT struct {
	mu       sync.Mutex
	script   []rtResp
	calls    int
	requests []string
}

type rtResp struct {
	status int
	body   string
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	f.mu.Lock()
	i := f.calls
	f.calls++
	f.requests = append(f.requests, req.Method+" "+req.URL.Path+" "+string(body))
	f.mu.Unlock()
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	r := f.script[i]
	return &http.Response{
		StatusCode: r.status,
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

func (f *fakeRT) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRT) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return ""
	}
	return f.requests[len(f.requests)-1]
}

func okBody(content string) string {
	return `{"id":"c1","object":"chat.completion","model":"test-model","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":` + jsonEsc(content) +
		`},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
}

func jsonEsc(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func newTestProvider(rt *fakeRT, cfg Config) *Provider {
	p := NewProvider(cfg, nil, option.WithHTTPClient(&http.Client{Transport: rt}))
	p.sleep = func(context.Context, time.Duration) bool { return true }
	p.jitter = func() float64 { return 0 }
	return p
}

func TestRewriteHappyPath(t *testing.T) {
	rt := &fakeRT{script: []rtResp{{200, okBody("改写后的文本")}}}
	p := newTestProvider(rt, Config{
		BaseURL: "https://ai.example/v1", APIKey: "sk-test",
		RewriteModel: "test-model", Temperature: 0.7,
	})
	resp, err := p.Rewrite(context.Background(), "你是改写器", "原文")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "改写后的文本" {
		t.Errorf("Text 不符: %q", resp.Text)
	}
	if resp.Metadata["model"] != "test-model" {
		t.Errorf("Metadata.model 不符: %v", resp.Metadata["model"])
	}
	body := rt.lastBody()
	if !strings.Contains(body, `"model":"test-model"`) {
		t.Errorf("请求应携带 model: %s", body)
	}
	if !strings.Contains(body, "你是改写器") || !strings.Contains(body, "原文") {
		t.Errorf("请求应含 system prompt 与原文: %s", body)
	}
}

func TestRewriteRetriesOn429And5xx(t *testing.T) {
	rt := &fakeRT{script: []rtResp{
		{429, `{}`},
		{500, `{}`},
		{200, okBody("ok")},
	}}
	p := newTestProvider(rt, Config{APIKey: "k", RewriteModel: "m"})
	var slept []time.Duration
	p.sleep = func(_ context.Context, d time.Duration) bool {
		slept = append(slept, d)
		return true
	}
	resp, err := p.Rewrite(context.Background(), "p", "t")
	if err != nil {
		t.Fatalf("第三次应成功: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text 不符: %q", resp.Text)
	}
	if rt.callCount() != 3 {
		t.Errorf("应有 3 次尝试，实际 %d", rt.callCount())
	}
	if len(slept) != 2 || slept[0] != 500*time.Millisecond || slept[1] != 1*time.Second {
		t.Errorf("退避应为指数+jitter（jitter=0 时 0.5×基准）: %v", slept)
	}
}

func TestRewriteGivesUpAfterThreeAttempts(t *testing.T) {
	rt := &fakeRT{script: []rtResp{{503, `{}`}}}
	p := newTestProvider(rt, Config{APIKey: "k", RewriteModel: "m"})
	if _, err := p.Rewrite(context.Background(), "p", "t"); err == nil {
		t.Fatal("三次均 5xx 应失败")
	}
	if rt.callCount() != 3 {
		t.Errorf("尝试次数应为 3，实际 %d", rt.callCount())
	}
}

func TestRewriteDoesNotRetryClientError(t *testing.T) {
	rt := &fakeRT{script: []rtResp{{400, `{"error":{"message":"bad"}}`}}}
	p := newTestProvider(rt, Config{APIKey: "k", RewriteModel: "m"})
	if _, err := p.Rewrite(context.Background(), "p", "t"); err == nil {
		t.Fatal("4xx 应立即失败")
	}
	if rt.callCount() != 1 {
		t.Errorf("4xx 不应重试，尝试 %d 次", rt.callCount())
	}
}

func TestRewriteEmptyChoicesIsError(t *testing.T) {
	rt := &fakeRT{script: []rtResp{{200, `{"id":"c1","model":"m","choices":[]}`}}}
	p := newTestProvider(rt, Config{APIKey: "k", RewriteModel: "m"})
	if _, err := p.Rewrite(context.Background(), "p", "t"); err == nil {
		t.Fatal("空 choices 应报错（不得返回空文本当作成功）")
	}
}
