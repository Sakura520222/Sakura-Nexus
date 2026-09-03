// Package ai 是 OpenAI-compatible LLM provider（ADR-006：所有能力走同一
// provider；openai-go + option.WithBaseURL 支持自建/中转端点）。
// P0 仅交付 Rewrite（转发改写）；Answer/Summarize/Embed 等 P1/P2 追加（05 §1）。
package ai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"

	"github.com/Sakura520222/Sakura-Nexus/internal/domain"
)

// Config 是 provider 构造配置（settings.ai 的 platform 侧形态；接线方订阅
// settings 热更回调后重建 provider——客户端无内部状态，重建即生效）。
type Config struct {
	BaseURL        string // 空 = OpenAI 官方端点
	APIKey         string
	RewriteModel   string
	Temperature    float64
	TimeoutSeconds int // 单次请求超时（含重试中的单次尝试）；0 = 60
}

const (
	// rewriteMaxAttempts：§1.4 AI 行「指数退避 + jitter，3 次」——3 次尝试。
	rewriteMaxAttempts = 3
	// rewriteBackoffBase：退避基准（实际入睡 = 基准×2^k×(0.5+0.5×jitter)）。
	rewriteBackoffBase = time.Second
)

// Provider 实现 P0 的 Rewrite 能力（05 §1 能力表）。
type Provider struct {
	cfg    Config
	cli    openai.Client
	log    *slog.Logger
	sleep  func(ctx context.Context, d time.Duration) bool
	jitter func() float64
}

// NewProvider 构造 provider；opts 允许接线方附加 openai-go 请求选项
// （测试注入 fake HTTP transport 亦经此）。
func NewProvider(cfg Config, log *slog.Logger, opts ...option.RequestOption) *Provider {
	if log == nil {
		log = slog.Default()
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 60
	}
	clientOpts := []option.RequestOption{
		option.WithAPIKey(cfg.APIKey),
		// SDK 内置重试关闭：重试矩阵由本包实现（§1.4：指数退避 + jitter，3 次），
		// 避免双层重试叠加放大延迟。
		option.WithMaxRetries(0),
	}
	if cfg.BaseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(cfg.BaseURL))
	}
	clientOpts = append(clientOpts, opts...)
	return &Provider{
		cfg:    cfg,
		cli:    openai.NewClient(clientOpts...),
		log:    log,
		sleep:  sleepCtx,
		jitter: rand.Float64,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Rewrite 以 system prompt + 原文请求改写（05 §1 Chat(rewrite)）。
// 429/5xx 按指数退避 + jitter 重试（上限 3 次尝试）；4xx 等客户端错误立即失败。
// 失败由调用方降级原文（03 §1.4：转发改写失败 → 降级原文，不是任务失败）。
func (p *Provider) Rewrite(ctx context.Context, prompt, text string) (domain.AIResponse, error) {
	if p.cfg.RewriteModel == "" {
		return domain.AIResponse{}, errors.New("ai.rewrite_model 未配置")
	}
	var lastErr error
	for attempt := 0; attempt < rewriteMaxAttempts; attempt++ {
		if attempt > 0 {
			d := time.Duration(1<<uint(attempt-1)) * rewriteBackoffBase
			d = time.Duration(float64(d) * (0.5 + 0.5*p.jitter()))
			if !p.sleep(ctx, d) {
				return domain.AIResponse{}, ctx.Err()
			}
		}
		resp, err := p.rewriteOnce(ctx, prompt, text)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var apiErr *openai.Error
		if !errors.As(err, &apiErr) || !retryableStatus(apiErr.StatusCode) {
			return domain.AIResponse{}, err
		}
	}
	return domain.AIResponse{}, lastErr
}

func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

func (p *Provider) rewriteOnce(ctx context.Context, prompt, text string) (domain.AIResponse, error) {
	if p.cfg.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(p.cfg.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	params := openai.ChatCompletionNewParams{
		Model: p.cfg.RewriteModel,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(prompt),
			openai.UserMessage(text),
		},
	}
	if p.cfg.Temperature > 0 {
		params.Temperature = param.NewOpt(p.cfg.Temperature)
	}
	res, err := p.cli.Chat.Completions.New(ctx, params)
	if err != nil {
		return domain.AIResponse{}, fmt.Errorf("chat completions: %w", err)
	}
	if len(res.Choices) == 0 {
		return domain.AIResponse{}, fmt.Errorf("模型返回空 choices（model=%s）", res.Model)
	}
	return domain.AIResponse{
		Text: res.Choices[0].Message.Content,
		Metadata: map[string]any{
			"model":         res.Model,
			"finish_reason": res.Choices[0].FinishReason,
			"total_tokens":  res.Usage.TotalTokens,
		},
	}, nil
}
