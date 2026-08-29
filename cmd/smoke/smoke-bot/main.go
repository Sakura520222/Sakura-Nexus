// Command smoke-bot 是 T1.1 冒烟（P0 Plan §5 Phase 1）：
// MySQL 连接 → goose 迁移 → gotd Bot 登录（Auth().Bot + Self 校验）→ 优雅关闭（session 落库）。
//
// 手动执行（S 类验证，不进 CI）：go run ./cmd/smoke/smoke-bot
// 需要仓库根 .env（参照 .env.example）；重跑应免登录直接验证。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Sakura520222/Sakura-Nexus/internal/config"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/mysql"
	"github.com/Sakura520222/Sakura-Nexus/internal/platform/telegram"
)

func main() {
	env, err := config.Load()
	if err != nil {
		fail("配置加载失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := mysql.Connect(ctx, mysql.Options{
		Host: env.MySQLHost, Port: env.MySQLPort,
		User: env.MySQLUser, Password: env.MySQLPassword, Database: env.MySQLDatabase,
		MaxOpenConns: env.MySQLMaxOpenConns,
	})
	if err != nil {
		fail("MySQL 连接: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			fmt.Printf("⚠ 关闭 MySQL 连接: %v\n", err)
		}
	}()

	if err := mysql.MigrateUp(ctx, db.DB); err != nil {
		fail("goose 迁移: %v", err)
	}
	fmt.Println("✓ 迁移完成（幂等）")

	storage := mysql.NewSessionStorage(db, "bot")
	bot := telegram.NewBotClient(int(env.TelegramAPIID), env.TelegramAPIHash, storage)

	err = bot.Run(ctx, func(ctx context.Context) error {
		if err := bot.AuthBot(ctx, env.TelegramBotToken); err != nil {
			return fmt.Errorf("bot 登录: %w", err)
		}
		self, err := bot.VerifySelf(ctx)
		if err != nil {
			return fmt.Errorf("身份校验: %w", err)
		}
		fmt.Printf("✓ Bot 身份: id=%d username=@%s name=%q\n", self.ID, self.Username, self.FirstName)
		return nil
	})
	if err != nil {
		fail("smoke-bot: %v", err)
	}

	var size int
	if err := db.Get(&size, "SELECT LENGTH(data) FROM gotd_sessions WHERE account='bot'"); err != nil {
		fail("session 落库检查: %v", err)
	}
	fmt.Printf("✓ gotd_sessions(bot) 已落库（%d bytes）\n", size)
	fmt.Println("✓ smoke-bot 通过；重跑可验证免登录与 session 复用")
}

func fail(format string, a ...any) {
	fmt.Printf("✗ "+format+"\n", a...)
	os.Exit(1)
}
