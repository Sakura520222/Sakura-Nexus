package domain

import "errors"

// ErrNotABot 表示 Self 返回的身份不是 Bot 账号（ADR-001：进程内唯一 Bot，
// Bot 槽登录了普通用户属于配置错误）。
var ErrNotABot = errors.New("telegram 身份不是 Bot 账号")

// ErrMediaTooLarge 表示媒体超过单文件大小上限（03 §3.9：上限可配，默认 2GB）。
// 引擎将其归类为永久性失败（terminal，不阻塞 cursor）；platform 下载器在流式
// 写入越过硬上限时返回。
var ErrMediaTooLarge = errors.New("媒体超过单文件大小上限")
