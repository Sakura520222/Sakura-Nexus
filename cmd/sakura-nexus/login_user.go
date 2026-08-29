package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/Sakura520222/Sakura-Nexus/internal/config"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/mysql"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/telegram"
)

// runLoginUser 是 T1.2 交付的交互式 User 登录子命令（UserAuthService 状态机的
// CLI presentation；WebUI 向导 T5.3 复用同一状态机，不另写）。
//
// 用法（在自己的终端运行，验证码不经过任何中间方）：
//
//	go run ./cmd/sakura-nexus login-user
func runLoginUser(args []string) {
	env, err := config.Load()
	if err != nil {
		fail("配置加载失败: %v", err)
	}

	ctx := context.Background()
	db, err := mysql.Connect(ctx, mysql.Options{
		Host: env.MySQLHost, Port: env.MySQLPort,
		User: env.MySQLUser, Password: env.MySQLPassword, Database: env.MySQLDatabase,
		MaxOpenConns: env.MySQLMaxOpenConns,
	})
	if err != nil {
		fail("MySQL 连接: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := mysql.MigrateUp(ctx, db.DB); err != nil {
		fail("goose 迁移: %v", err)
	}

	storage := mysql.NewSessionStorage(db, "user")
	user := telegram.NewUserClient(int(env.TelegramAPIID), env.TelegramAPIHash, storage)

	in := bufio.NewReader(os.Stdin)
	err = user.Run(ctx, func(ctx context.Context) error {
		auth := telegram.NewUserAuth(user.Raw())

		phone := env.UserbotPhoneNumber
		if phone == "" {
			fmt.Print("手机号（国际格式，如 +8613800138000）: ")
			line, err := in.ReadString('\n')
			if err != nil {
				return err
			}
			phone = strings.TrimSpace(line)
		}

		if err := auth.StartLogin(ctx, phone); err != nil {
			if errors.Is(err, telegram.ErrAlreadyAuthorized) {
				return printSelf(ctx, user)
			}
			return err
		}

		fmt.Print("验证码（Telegram 客户端收到）: ")
		codeLine, err := in.ReadString('\n')
		if err != nil {
			return err
		}
		code := strings.TrimSpace(codeLine)

		if err := auth.SubmitCode(ctx, code); err != nil {
			if errors.Is(err, telegram.ErrPasswordNeeded) {
				fmt.Print("两步验证密码（2FA）: ")
				pwLine, err := in.ReadString('\n')
				if err != nil {
					return err
				}
				if err := auth.SubmitPassword(ctx, strings.TrimSpace(pwLine)); err != nil {
					return err
				}
			} else {
				return err
			}
		}
		return printSelf(ctx, user)
	})
	if err != nil {
		fail("login-user: %v", err)
	}
	fmt.Println("✓ 登录完成，session 已落库（account='user'）")
}

func printSelf(ctx context.Context, user *telegram.UserClient) error {
	self, err := user.Self(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("✓ User 身份: id=%d username=@%s name=%q\n", self.ID, self.Username, self.FirstName)
	return nil
}
