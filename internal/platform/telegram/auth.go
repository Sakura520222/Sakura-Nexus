package telegram

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	tgerr "github.com/gotd/td/tgerr"
)

// ErrPasswordNeeded 表示验证码通过但账号开启了两步验证，需要调用 SubmitPassword。
var ErrPasswordNeeded = errors.New("需要两步验证密码（2FA）")

// ErrAlreadyAuthorized 表示当前 session 已是登录状态，无需重复流程。
var ErrAlreadyAuthorized = errors.New("已登录，无需重复登录")

// UserAuth 是真实账号登录的状态机（T1.2 唯一实现；WebUI 向导（T5.3）仅作
// presentation adapter 复用，不得另写第二套——P0 Plan §2 原则 6）。
type UserAuth struct {
	client   *telegram.Client
	phone    string
	codeHash string
	started  bool
	password bool // SubmitCode 返回 ErrPasswordNeeded 后置位
}

// NewUserAuth 基于已连接的客户端创建登录状态机。
func NewUserAuth(client *telegram.Client) *UserAuth {
	return &UserAuth{client: client}
}

// StartLogin 对 phone 发送验证码；已登录时返回 ErrAlreadyAuthorized。
func (u *UserAuth) StartLogin(ctx context.Context, phone string) error {
	status, err := u.client.Auth().Status(ctx)
	if err != nil {
		return fmt.Errorf("查询登录状态: %w", err)
	}
	if status.Authorized {
		return ErrAlreadyAuthorized
	}
	sent, err := u.client.Auth().SendCode(ctx, phone, auth.SendCodeOptions{})
	if err != nil {
		return fmt.Errorf("发送验证码: %w", err)
	}
	sc, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return fmt.Errorf("意外的验证码响应类型 %T", sent)
	}
	u.phone = phone
	u.codeHash = sc.PhoneCodeHash
	u.started = true
	return nil
}

// SubmitCode 提交验证码；账号开启 2FA 时返回 ErrPasswordNeeded。
func (u *UserAuth) SubmitCode(ctx context.Context, code string) error {
	if !u.started {
		return errors.New("尚未调用 StartLogin")
	}
	if _, err := u.client.Auth().SignIn(ctx, u.phone, code, u.codeHash); err != nil {
		if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
			u.password = true
			return ErrPasswordNeeded
		}
		return fmt.Errorf("提交验证码: %w", err)
	}
	return nil
}

// SubmitPassword 提交两步验证密码（SubmitCode 返回 ErrPasswordNeeded 后调用）。
func (u *UserAuth) SubmitPassword(ctx context.Context, password string) error {
	if !u.password {
		return errors.New("当前不需要 2FA 密码")
	}
	if _, err := u.client.Auth().Password(ctx, password); err != nil {
		return fmt.Errorf("提交 2FA 密码: %w", err)
	}
	return nil
}

// Phone 返回当前登录流程的手机号（向导回显用）。
func (u *UserAuth) Phone() string { return u.phone }
