package botapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Options 是 Client 构造配置（BaseURL/HTTPClient 供测试注入 fake server；
// 生产零值即官方端点 + 默认传输）。
type Options struct {
	BaseURL    string       // 空 = https://api.telegram.org
	HTTPClient *http.Client // nil = {Timeout: 30s}（03 §2.8 超时 30s）
	Log        *slog.Logger // nil = slog.Default()
}

const defaultBaseURL = "https://api.telegram.org"

// maxRetries 是 §1.4 HTTP 行的重试上限（初次尝试之外至多 3 次；429 与
// 5xx/网络错误共用同一预算，任一错误类型都不会越过「上限 3 次」）。
const maxRetries = 3

// Client 是 Bot API HTTP 出站传输（ADR-008：net/http 直调、无 SDK、
// 无常驻连接池之外的状态，03 §2.8）。方法调用统一经 Call。
type Client struct {
	token string
	base  *url.URL
	http  *http.Client
	log   *slog.Logger
	sleep func(ctx context.Context, d time.Duration) bool
}

// NewClient 构造客户端。token 即 TELEGRAM_BOT_TOKEN（ADR-008：同一 token，
// 不新增第二个 Bot）。
func NewClient(token string, opts Options) *Client {
	base := defaultBaseURL
	if opts.BaseURL != "" {
		base = opts.BaseURL
	}
	u, _ := url.Parse(base)
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	lg := opts.Log
	if lg == nil {
		lg = slog.Default()
	}
	return &Client{
		token: token,
		base:  u,
		http:  hc,
		log:   lg,
		sleep: sleepCtx,
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

// response 是 Bot API 统一响应封套。
type response struct {
	OK          bool                `json:"ok"`
	Result      json.RawMessage     `json:"result,omitempty"`
	ErrorCode   int                 `json:"error_code,omitempty"`
	Description string              `json:"description,omitempty"`
	Parameters  *responseParameters `json:"parameters,omitempty"`
}

type responseParameters struct {
	RetryAfter int `json:"retry_after"`
}

// APIError 是 Bot API 返回的业务错误（ok=false）。Code/Description 原样透出——
// Rich lazy capability detection 按 Telegram 错误语义判定，不写死 HTTP 状态
// （03 §2.9），调用方依赖此处信息的完整性。
type APIError struct {
	Method      string
	Code        int
	Description string

	retryAfter time.Duration // 429 时从 parameters.retry_after / Retry-After 头提取
}

func (e *APIError) Error() string {
	return fmt.Sprintf("botapi: %s 失败 (code=%d): %s", e.Method, e.Code, e.Description)
}

// transportError 标记 HTTP 传输层失败（§1.4「5xx / 网络错误」同行处理）。
type transportError struct{ err error }

func (e *transportError) Error() string { return e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// Call 调用 Bot API 方法：POST JSON 到 <base>/bot<token>/<method>，解析统一
// 封套并把 result 解入 result（nil 则忽略）。429/5xx 按重试矩阵执行后仍失败
// 的错误返回给调用方（超限语义 = failed + 可补发，03 §1.4）。
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	var body bytes.Buffer
	if params != nil {
		if err := json.NewEncoder(&body).Encode(params); err != nil {
			return fmt.Errorf("botapi: 序列化 %s 参数: %w", method, err)
		}
	}
	payload := body.Bytes()

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			c.log.Warn("botapi 重试",
				"method", method, "attempt", attempt, "delay", retryDelay(lastErr, attempt),
				"err", sanitizeStr(c.token, lastErr.Error()))
			if !c.sleep(ctx, retryDelay(lastErr, attempt)) {
				return ctx.Err()
			}
		}
		env, err := c.once(ctx, method, payload)
		if err == nil {
			return decodeResult(method, env, result)
		}
		lastErr = err
		if !retryable(err) {
			return err
		}
	}
	return lastErr
}

// once 执行单次 HTTP 往返并解析封套（不含重试）。
func (c *Client) once(ctx context.Context, method string, payload []byte) (*response, error) {
	u := c.base.JoinPath("bot"+c.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("botapi: 构造 %s 请求: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// 06 §5：不打印完整 URL（token 在 path 中）——url.Error 形如
		// `Op "URL": cause`，重组为 `Op method: cause`，token 双保险脱敏。
		return nil, &transportError{err: sanitizeTransportErr(c.token, method, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("botapi: 读取 %s 响应: %w", method, err)
	}
	var env response
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("botapi: %s 响应非 JSON (status=%d)", method, resp.StatusCode)
	}
	if !env.OK {
		apiErr := &APIError{Method: method, Description: env.Description}
		if apiErr.Code = env.ErrorCode; apiErr.Code == 0 {
			apiErr.Code = resp.StatusCode
		}
		apiErr.retryAfter = retryAfterOf(&env, resp)
		return nil, apiErr
	}
	return &env, nil
}

func decodeResult(method string, env *response, result any) error {
	if result != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, result); err != nil {
			return fmt.Errorf("botapi: 解码 %s result: %w", method, err)
		}
	}
	return nil
}

// retryable 判定是否按矩阵重试：429 与 5xx 按 §1.4 各自策略；网络错误与
// 5xx 同行；其余（4xx 业务错误、响应损坏）立即失败。
func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusTooManyRequests || apiErr.Code >= 500
	}
	var tErr *transportError
	return errors.As(err, &tErr)
}

// retryDelay 计算第 attempt 次重试（从 1 计）前的入睡时长：
// 429 → 服从 retry_after + 1s（§1.4）；5xx / 网络错误 → 指数退避 1/2/4s。
func retryDelay(err error, attempt int) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusTooManyRequests {
		return apiErr.retryAfter + time.Second
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

// retryAfterOf 提取 429 的 retry_after（秒）：body parameters 优先，缺失回退
// Retry-After 头；两者皆缺按 0 处理（入睡保底 +1s）。
func retryAfterOf(env *response, resp *http.Response) time.Duration {
	if env.Parameters != nil && env.Parameters.RetryAfter > 0 {
		return time.Duration(env.Parameters.RetryAfter) * time.Second
	}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 0
}

// sanitizeErr 确保错误文本不含 token（06 §5：错误信息脱敏——url.Error 会
// 内嵌完整 URL，token 就在其中）。
func sanitizeErr(token string, err error) error {
	msg := err.Error()
	if !strings.Contains(msg, token) {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(msg, token, "<token>"))
}

func sanitizeStr(token, s string) string {
	if token == "" || !strings.Contains(s, token) {
		return s
	}
	return strings.ReplaceAll(s, token, "<token>")
}

// sanitizeTransportErr 将传输层错误转为无 URL、无 token 的文本（06 §5：
// 不打印完整 URL——token 在 path 中）。url.Error 形如 `Op "URL": cause`，
// 重组为 `Op method: cause`。
func sanitizeTransportErr(token, method string, err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return fmt.Errorf("botapi: %s %s: %s", sanitizeStr(token, uerr.Op), method, sanitizeStr(token, uerr.Err.Error()))
	}
	return sanitizeErr(token, fmt.Errorf("botapi: %s 请求失败: %w", method, err))
}
