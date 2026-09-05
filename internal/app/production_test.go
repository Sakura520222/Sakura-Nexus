package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gotd/td/tgerr"
	"github.com/openai/openai-go/option"

	"github.com/Sakura520222/Sakura-Nexus/internal/config"
	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
	"github.com/Sakura520222/Sakura-Nexus/internal/forwarding"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/ai"
)

func TestForwardingParamsOf(t *testing.T) {
	got := forwardingParamsOf(config.ForwardingSettings{
		ShowDefaultFooter:   false,
		DedupDays:           7,
		ContentDedup:        true,
		DefaultDelayMinSec:  1.5,
		DefaultDelayMaxSec:  3,
		AlbumQuietMs:        300,
		AlbumHardDeadlineMs: 1500,
		MediaMaxSizeMB:      100,
	})
	if got.ShowDefaultFooter || got.DedupDays != 7 || !got.ContentDedup {
		t.Errorf("直映字段不符: %+v", got)
	}
	if got.DefaultDelayMinSec != 1.5 || got.DefaultDelayMaxSec != 3 {
		t.Errorf("延迟区间不符: %+v", got)
	}
	if got.AlbumQuietMs != 300 || got.AlbumHardDeadlineMs != 1500 {
		t.Errorf("相册窗口不符: %+v", got)
	}
	if got.MediaMaxSizeBytes != 100<<20 {
		t.Errorf("MB→字节转换不符: %d", got.MediaMaxSizeBytes)
	}

	// 零值快照 → 默认延迟兜底（settings 加载缺省即此形态）。
	def := forwardingParamsOf(config.ForwardingSettings{MediaMaxSizeMB: 2048})
	if def.MediaMaxSizeBytes != 2<<30 {
		t.Errorf("默认 2048MB 应为 2GB: %d", def.MediaMaxSizeBytes)
	}
}

// captureRT 捕获请求体并回放固定 completion 响应（aiRewriter 适配断言）。
type captureRT struct {
	body string
}

func (c *captureRT) RoundTrip(req *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(req.Body)
	c.body = string(b)
	resp := `{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"改写稿"},"finish_reason":"stop"}],"usage":{"total_tokens":1}}`
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader(resp)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

func TestAIRewriterAdapterUsesRulePromptAndAggregateText(t *testing.T) {
	rt := &captureRT{}
	p := newTestProvider(rt)
	holder := &aiProviderHolder{}
	holder.Store(p)

	r := aiRewriter{holder: holder}
	resp, err := r.Rewrite(context.Background(), domain.ForwardRule{AIPrompt: "你是改写器"},
		forwarding.FilterView{AggregateText: "原文内容"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text != "改写稿" {
		t.Errorf("改写结果不符: %q", resp.Text)
	}
	if !strings.Contains(rt.body, `"content":"你是改写器"`) {
		t.Errorf("system prompt 应取 rule.AIPrompt: %s", rt.body)
	}
	if !strings.Contains(rt.body, `"content":"原文内容"`) {
		t.Errorf("user 文本应取 AggregateText: %s", rt.body)
	}
}

// TestClassifySendFailure 验证 gotd 感知分类的组合映射（permanent 集在
// platform/telegram 单测覆盖，此处验证接线方向正确）。
func TestClassifySendFailure(t *testing.T) {
	if classifySendFailure(nil) != forwarding.FailureTransient {
		t.Error("nil 应 transient")
	}
	if classifySendFailure(errors.New("dial tcp: refused")) != forwarding.FailureTransient {
		t.Error("网络错误应 transient")
	}
	if classifySendFailure(&tgerr.Error{Code: 400, Type: "CHAT_WRITE_FORBIDDEN"}) != forwarding.FailurePermanent {
		t.Error("CHAT_WRITE_FORBIDDEN 应 permanent")
	}
}

// newTestProvider 构造接自定义传输的 ai.Provider（openai-go option 注入）。
func newTestProvider(rt http.RoundTripper) *ai.Provider {
	return ai.NewProvider(ai.Config{
		BaseURL: "https://ai.example/v1", APIKey: "k",
		RewriteModel: "m",
	}, nil, option.WithHTTPClient(&http.Client{Transport: rt}))
}
