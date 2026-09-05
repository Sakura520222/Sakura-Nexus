package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/telegram/updates"
)

// UserService / BotService 是 production lifecycle 的 gotd 客户端包装
// （01 §1.1 步骤 4b/4c + §1.3）：凭结构类型满足 app.Service，不 import app
//（依赖方向 01 §2.2——gotd 全部封在 platform 层）。

// UserService 是 userbot 服务（LENIENT，01 §1.1 4c：未登录 → degraded 等待
// WebUI 登录向导，不退出不重启；授权后启动 updates.Manager）。
type UserService struct {
	user    *UserClient
	manager *updates.Manager
	log     *slog.Logger
}

// NewUserService 构造（manager 由 SetupUserUpdates 一站式装配返回）。
func NewUserService(user *UserClient, manager *updates.Manager, log *slog.Logger) *UserService {
	if log == nil {
		log = slog.Default()
	}
	return &UserService{user: user, manager: manager, log: log}
}

// Name 实现 app.Service。
func (s *UserService) Name() string { return "user" }

// Run 阻塞至 ctx 取消。未授权 = DEPENDENCY_UNAVAILABLE 语义：30s 重查等待
// （登录向导 T5.3 接入后可运行中恢复）；授权后 manager.Run 接管
// （重连/gap recovery 由 gotd updates 与 Recovery 处置）。
func (s *UserService) Run(ctx context.Context) error {
	return s.user.Run(ctx, func(ctx context.Context) error {
		for {
			authed, err := s.user.Authorized(ctx)
			if err == nil && authed {
				break
			}
			if err != nil {
				s.log.Warn("user 授权检查失败（degraded 等待）", "err", err)
			} else {
				s.log.Warn("userbot 未登录，等待登录（WebUI 向导或 login-user CLI）；30s 重查")
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(30 * time.Second):
			}
		}
		self, err := s.user.Self(ctx)
		if err != nil {
			return fmt.Errorf("user Self: %w", err)
		}
		s.log.Info("user 已授权，启动 updates", "id", self.ID, "username", self.Username)
		return s.manager.Run(ctx, s.user.Raw().API(), self.ID, updates.AuthOptions{})
	})
}

// Shutdown 实现 app.Service（gotd Run 随 ctx 收尾：session/update state 落库）。
func (s *UserService) Shutdown(context.Context) error { return nil }

// BotService 是 bot 服务（STRICT，01 §1.1 4b：登录/校验失败 = OWN_FATAL →
// CORE fatal → exit 1，systemd 兜底）。onAuthed 在 Self 校验后回调一次
// （接线方据此填充 {assistant_bot} 占位符取值；签名用 username string，
// 组合根不触碰 gotd 类型）。
type BotService struct {
	bot      *BotClient
	token    string
	onAuthed func(username string)
	ready    chan struct{}
	readyOne sync.Once
	log      *slog.Logger
}

// NewBotService 构造。onAuthed 可为 nil。
func NewBotService(bot *BotClient, token string, onAuthed func(username string), log *slog.Logger) *BotService {
	if log == nil {
		log = slog.Default()
	}
	return &BotService{
		bot: bot, token: token, onAuthed: onAuthed,
		ready: make(chan struct{}), log: log,
	}
}

// Name 实现 app.Service。
func (s *BotService) Name() string { return "bot" }

// Ready 实现 app.Readiness：Bot 授权成功后关闭（readiness barrier 的 CORE 信号）。
func (s *BotService) Ready() <-chan struct{} { return s.ready }

// Run 阻塞至 ctx 取消。
func (s *BotService) Run(ctx context.Context) error {
	return s.bot.Run(ctx, func(ctx context.Context) error {
		if err := s.bot.AuthBot(ctx, s.token); err != nil {
			return fmt.Errorf("bot 登录: %w", err)
		}
		self, err := s.bot.VerifySelf(ctx)
		if err != nil {
			return fmt.Errorf("bot 身份校验: %w", err)
		}
		s.log.Info("bot 已连接", "id", self.ID, "username", self.Username)
		if s.onAuthed != nil {
			s.onAuthed(self.Username)
		}
		s.readyOne.Do(func() { close(s.ready) })
		<-ctx.Done()
		return nil
	})
}

// Shutdown 实现 app.Service。
func (s *BotService) Shutdown(context.Context) error { return nil }

// UsernameHolder 是 {assistant_bot} 占位符的运行时取值（bot 登录后填充；
// 引擎 AssistantBotFn 消费）。
type UsernameHolder struct{ v atomic.Value }

// Set 登录后填充（空 username 存空串）。
func (h *UsernameHolder) Set(username string) { h.v.Store(username) }

// Get 读取（未填充返回空串）。
func (h *UsernameHolder) Get() string {
	s, _ := h.v.Load().(string)
	return s
}
