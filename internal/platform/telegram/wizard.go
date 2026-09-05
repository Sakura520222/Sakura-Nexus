package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram"
)

// WizardService 是 WebUI userbot 登录向导的三步状态机宿主（04 §2；T1.2
// UserAuthService 的 WebUI presentation adapter 后端）。每条向导会话独立
// 连接（与生产 UserService 的主连接并存，Telegram 多会话合法）；runLogin
// 注入化——单测覆盖编舞，真实 gotd 流程由生产构造器提供（S 验证）。

// 向导状态。
const (
	WizardStateAwaitCode     = "await_code"
	WizardStateAwaitPassword = "await_password"
	WizardStateAuthorized    = "authorized"
	WizardStateFailed        = "failed"
)

// wizardSession 是一条登录会话的通道集（容量 1：每步恰好一次投递）。
type wizardSession struct {
	cancel  context.CancelFunc
	state   atomicString
	codeCh  chan string
	pwCh    chan string
	resCh   chan error // 每步结果（nil=成功推进）
	created time.Time
}

type atomicString struct {
	mu sync.Mutex
	v  string
}

func (a *atomicString) Store(v string) { a.mu.Lock(); a.v = v; a.mu.Unlock() }
func (a *atomicString) Load() string   { a.mu.Lock(); defer a.mu.Unlock(); return a.v }

// runLoginFunc 是真实登录流的注入点：阻塞执行 StartLogin → 等验证码 →
// （可选 2FA）→ 结果写入 res（nil=完成；ErrPasswordNeeded=需密码步）。
type runLoginFunc func(ctx context.Context, phone string, s *wizardSession)

// WizardService 管理并发向导会话（上限 3）。
type WizardService struct {
	runLogin runLoginFunc
	statusFn func(ctx context.Context) (authorized bool, username string, tgID int64, err error)

	mu       sync.Mutex
	sessions map[string]*wizardSession
	log      *slog.Logger
}

// NewWizardService 构造生产实例（session 复用 user account 存储；
// statusFn 由接线方以主 user 客户端实现）。
func NewWizardService(apiID int, apiHash string, session telegram.SessionStorage,
	statusFn func(ctx context.Context) (bool, string, int64, error), log *slog.Logger) *WizardService {
	return NewWizardServiceWithRunner(func(ctx context.Context, phone string, s *wizardSession) {
		client := NewUserClient(apiID, apiHash, session)
		_ = client.Run(ctx, func(ctx context.Context) error {
			auth := NewUserAuth(client.Raw())
			if err := auth.StartLogin(ctx, phone); err != nil {
				s.resCh <- err
				return err
			}
			s.state.Store(WizardStateAwaitCode)
			select {
			case code := <-s.codeCh:
				err := auth.SubmitCode(ctx, code)
				if errors.Is(err, ErrPasswordNeeded) {
					s.state.Store(WizardStateAwaitPassword)
					s.resCh <- err
					select {
					case pw := <-s.pwCh:
						if err := auth.SubmitPassword(ctx, pw); err != nil {
							s.resCh <- err
							return err
						}
						s.state.Store(WizardStateAuthorized)
						s.resCh <- nil
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				if err != nil {
					s.resCh <- err
					return err
				}
				s.state.Store(WizardStateAuthorized)
				s.resCh <- nil
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}, statusFn, log)
}

// NewWizardServiceWithRunner 以注入 runner 构造（单测）。
func NewWizardServiceWithRunner(run runLoginFunc,
	statusFn func(ctx context.Context) (bool, string, int64, error), log *slog.Logger) *WizardService {
	if log == nil {
		log = slog.Default()
	}
	return &WizardService{
		runLogin: run, statusFn: statusFn,
		sessions: map[string]*wizardSession{}, log: log,
	}
}

// Status 经主 user 客户端报告授权状态（04 §2 GET /userbot/status）。
func (w *WizardService) Status(ctx context.Context) (bool, string, int64, error) {
	if w.statusFn == nil {
		return false, "", 0, errors.New("statusFn 未接线")
	}
	return w.statusFn(ctx)
}

// LoginStart 发起登录（独立连接；返回 requestID）。
func (w *WizardService) LoginStart(ctx context.Context, phone string) (string, error) {
	phone = strings.TrimSpace(phone)
	if !strings.HasPrefix(phone, "+") {
		return "", errors.New("手机号须为国际格式（+ 开头）")
	}
	w.mu.Lock()
	// 清理超时会话（10 分钟未完成即回收）。
	for id, s := range w.sessions {
		if time.Since(s.created) > 10*time.Minute {
			s.cancel()
			delete(w.sessions, id)
		}
	}
	if len(w.sessions) >= 3 {
		w.mu.Unlock()
		return "", errors.New("登录会话过多，请稍后再试")
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		w.mu.Unlock()
		return "", fmt.Errorf("生成 requestID: %w", err)
	}
	id := hex.EncodeToString(b[:])
	ws := &wizardSession{
		codeCh: make(chan string, 1), pwCh: make(chan string, 1), resCh: make(chan error, 1),
		created: time.Now(),
	}
	w.sessions[id] = ws
	w.mu.Unlock()

	wctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	ws.cancel = cancel
	go func() {
		defer cancel()
		w.runLogin(wctx, phone, ws)
		w.mu.Lock()
		if s := w.sessions[id]; s != nil && s.state.Load() != WizardStateAuthorized {
			s.state.Store(WizardStateFailed)
		}
		w.mu.Unlock()
	}()
	return id, nil
}

// wizardStep 投递步骤输入并等待结果（统一编舞）。
func wizardStep(ws *wizardSession, ch chan string, input string) (string, error) {
	select {
	case ch <- input:
	case <-time.After(5 * time.Second):
		return "", errors.New("登录会话未就绪")
	}
	select {
	case err := <-ws.resCh:
		return ws.state.Load(), err
	case <-time.After(90 * time.Second):
		return "", errors.New("登录步骤超时")
	}
}

// LoginCode 提交验证码；passwordRequired=true 时继续 LoginPassword。
func (w *WizardService) LoginCode(_ context.Context, requestID, code string) (passwordRequired bool, err error) {
	ws, err := w.lookup(requestID)
	if err != nil {
		return false, err
	}
	state, err := wizardStep(ws, ws.codeCh, strings.TrimSpace(code))
	if err != nil {
		if errors.Is(err, ErrPasswordNeeded) {
			return true, nil
		}
		return false, err
	}
	_ = state
	return false, nil
}

// LoginPassword 提交两步验证密码。
func (w *WizardService) LoginPassword(_ context.Context, requestID, password string) error {
	ws, err := w.lookup(requestID)
	if err != nil {
		return err
	}
	_, err = wizardStep(ws, ws.pwCh, password)
	return err
}

// Complete 返回会话是否已完成登录（code/password 步后的确认查询）。
func (w *WizardService) Complete(requestID string) (bool, error) {
	ws, err := w.lookup(requestID)
	if err != nil {
		return false, err
	}
	return ws.state.Load() == WizardStateAuthorized, nil
}

func (w *WizardService) lookup(requestID string) (*wizardSession, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ws, ok := w.sessions[requestID]
	if !ok {
		return nil, errors.New("登录会话不存在或已过期")
	}
	return ws, nil
}
