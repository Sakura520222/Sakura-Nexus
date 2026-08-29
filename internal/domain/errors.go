package domain

import "errors"

// ErrNotABot 表示 Self 返回的身份不是 Bot 账号（ADR-001：进程内唯一 Bot，
// Bot 槽登录了普通用户属于配置错误）。
var ErrNotABot = errors.New("telegram 身份不是 Bot 账号")
