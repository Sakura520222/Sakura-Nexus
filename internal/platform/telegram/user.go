package telegram

import (
	"context"

	"fmt"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"strings"
)

// UserClient 是真实账号（User）的 gotd MTProto 客户端（ADR-001：唯一抓取者，
// P0 T1.2 最小形态为登录与接收验证；updates/Manager 与 Fetcher 能力在 T1.3 接入）。
type UserClient struct {
	client *telegram.Client
}

// UserOption 定制 UserClient 构造参数。
type UserOption func(*telegram.Options)

// WithUpdateHandler 注册原始 update 处理器（T1.3 的 updates.Manager wiring 将取代
// 直接使用；smoke-user 用于最小收包验证）。
func WithUpdateHandler(h telegram.UpdateHandler) UserOption {
	return func(o *telegram.Options) { o.UpdateHandler = h }
}

// WithMiddleware 附加客户端中间件（T1.3：updhook.UpdateHook/AffectedHook）。
func WithMiddleware(mw ...telegram.Middleware) UserOption {
	return func(o *telegram.Options) { o.Middlewares = append(o.Middlewares, mw...) }
}

// NewUserClient 构造 User 客户端（Device 用 gotd 固定默认值，03 §1.1）。
func NewUserClient(apiID int, apiHash string, storage telegram.SessionStorage, opts ...UserOption) *UserClient {
	o := telegram.Options{SessionStorage: storage}
	for _, f := range opts {
		f(&o)
	}
	return &UserClient{client: telegram.NewClient(apiID, apiHash, o)}
}

// Run 连接并运行 f（阻塞直至 f 返回；负责连接生命周期与 session 落库）。
func (u *UserClient) Run(ctx context.Context, f func(ctx context.Context) error) error {
	return u.client.Run(ctx, f)
}

// Raw 暴露底层客户端——仅供 UserAuth 状态机与（T1.3 的）updates Manager wiring
// 使用；领域代码不得触碰（01 §2.2 依赖方向）。
func (u *UserClient) Raw() *telegram.Client { return u.client }

// Authorized 报告当前 session 是否已登录。
func (u *UserClient) Authorized(ctx context.Context) (bool, error) {
	status, err := u.client.Auth().Status(ctx)
	if err != nil {
		return false, err
	}
	return status.Authorized, nil
}

// Self 返回当前已认证身份。
func (u *UserClient) Self(ctx context.Context) (*tg.User, error) {
	return u.client.Self(ctx)
}

// Logout 退出当前账号（服务端 session 失效；WebUI userbot/logout 消费）。
func (u *UserClient) Logout(ctx context.Context) error {
	_, err := u.client.API().AuthLogOut(ctx)
	return err
}

// JoinChannel 公开频道加入（03 §3.8：规则保存时源频道预检/自动加入）。
func (u *UserClient) JoinChannel(ctx context.Context, username string) error {
	res, err := u.client.API().ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: strings.TrimPrefix(username, "@"),
	})
	if err != nil {
		return fmt.Errorf("解析频道 %s: %w", username, err)
	}
	for _, c := range res.Chats {
		ch, ok := c.(*tg.Channel)
		if !ok {
			continue
		}
		if _, err := u.client.API().ChannelsJoinChannel(ctx, &tg.InputChannel{
			ChannelID: ch.ID, AccessHash: ch.AccessHash,
		}); err != nil {
			return fmt.Errorf("加入频道 %s: %w", username, err)
		}
		return nil
	}
	return fmt.Errorf("解析频道 %s: 未返回频道实体", username)
}
