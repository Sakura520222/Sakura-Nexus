// Package telegram 提供 gotd User/Bot 客户端、updates 分发与 Manager wiring、
// 以及统一出站 Outbound（内部路由 MTProto / Bot API Rich，内含 MessageRenderer）。
// 设计：docs/design/03-telegram-and-forwarding.md §1–§2。
package telegram

import (
	"context"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"

	"github.com/Sakura520222/Sakura-Bot/internal/domain"
)

// BotClient 是唯一 Bot 账号的 gotd MTProto 客户端（ADR-001：默认发送通道、
// 不参与任何频道抓取）。P0 T1.1 最小形态：构造 + 登录验证；
// Availability 与生命周期在 T1.3/T2.0 接入。
type BotClient struct {
	client *telegram.Client
}

// NewBotClient 构造 Bot 客户端。Device 参数使用 gotd 固定默认值
// （03 §1.1：设备指纹恒定，避免随机指纹触发风控）。
func NewBotClient(apiID int, apiHash string, storage telegram.SessionStorage) *BotClient {
	client := telegram.NewClient(apiID, apiHash, telegram.Options{
		SessionStorage: storage,
	})
	return &BotClient{client: client}
}

// Run 连接并运行 f（阻塞直至 f 返回；负责连接生命周期与 session 落库）。
func (b *BotClient) Run(ctx context.Context, f func(ctx context.Context) error) error {
	return b.client.Run(ctx, f)
}

// AuthBot 以 Bot token 登录（已授权时幂等返回）。
func (b *BotClient) AuthBot(ctx context.Context, token string) error {
	_, err := b.client.Auth().Bot(ctx, token)
	return err
}

// Self 返回当前已认证身份（03 §1.1：用 Self 验证，不调 HTTP getMe）。
func (b *BotClient) Self(ctx context.Context) (*tg.User, error) {
	return b.client.Self(ctx)
}

// VerifySelf 校验当前身份确为 Bot 账号，返回 (botUserID, username)。
// GATE-1a 验证内容：self.Bot == true。
func (b *BotClient) VerifySelf(ctx context.Context) (tg.User, error) {
	self, err := b.Self(ctx)
	if err != nil {
		return tg.User{}, err
	}
	if !self.Bot {
		return tg.User{}, domain.ErrNotABot
	}
	return *self, nil
}
