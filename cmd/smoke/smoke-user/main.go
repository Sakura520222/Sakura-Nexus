// Command smoke-user 是 T1.2 冒烟：验证 User session（login-user 产物）可免登录复用，
// 并能接收一条真实 update（打印后退出）。
//
// 手动执行：go run ./cmd/smoke/smoke-user（需先完成 login-user 登录）
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	tdtelegram "github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"

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
	defer func() { _ = db.Close() }()
	if err := mysql.MigrateUp(ctx, db.DB); err != nil {
		fail("goose 迁移: %v", err)
	}

	// 收到第一条消息即退出（冒烟目的：证明 updates 通道活着）
	got := make(chan *tg.Message, 1)
	handler := tdtelegram.UpdateHandlerFunc(func(ctx context.Context, u tg.UpdatesClass) error {
		for _, m := range extractMessages(u) {
			select {
			case got <- m:
				stop() // 触发优雅退出
			default:
			}
		}
		return nil
	})

	storage := mysql.NewSessionStorage(db, "user")
	user := telegram.NewUserClient(int(env.TelegramAPIID), env.TelegramAPIHash, storage,
		telegram.WithUpdateHandler(handler))

	err = user.Run(ctx, func(ctx context.Context) error {
		authorized, err := user.Authorized(ctx)
		if err != nil {
			return err
		}
		if !authorized {
			fmt.Println("✗ 尚未登录：请先运行 go run ./cmd/sakura-nexus login-user")
			os.Exit(1)
		}
		self, err := user.Self(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("✓ User 身份（免登录复用 session）: id=%d username=@%s name=%q\n",
			self.ID, self.Username, self.FirstName)

		fmt.Println("… 等待一条真实消息（60s 超时；可在任意已加入频道/对话发送一条）")
		select {
		case m := <-got:
			fmt.Printf("✓ 收到 update: msg_id=%d text=%q\n", m.ID, excerpt(m.Message))
			fmt.Println("✓ smoke-user 通过")
			return nil
		case <-time.After(60 * time.Second):
			fmt.Println("⚠ 60s 内未收到消息（无活动对话时属正常）；GATE-1 的收包验证由 smoke-recovery 用真实频道消息完成")
			return nil
		case <-ctx.Done():
			return nil
		}
	})
	if err != nil {
		fail("smoke-user: %v", err)
	}
}

// extractMessages 从 UpdatesClass 中提取 tg.Message（覆盖容器与短更新两类形态）。
func extractMessages(u tg.UpdatesClass) []*tg.Message {
	var out []*tg.Message
	appendMsg := func(mc tg.MessageClass) {
		if m, ok := mc.(*tg.Message); ok {
			out = append(out, m)
		}
	}
	switch tu := u.(type) {
	case *tg.Updates:
		for _, up := range tu.Updates {
			switch u2 := up.(type) {
			case *tg.UpdateNewMessage:
				appendMsg(u2.Message)
			case *tg.UpdateNewChannelMessage:
				appendMsg(u2.Message)
			}
		}
	case *tg.UpdatesCombined:
		for _, up := range tu.Updates {
			switch u2 := up.(type) {
			case *tg.UpdateNewMessage:
				appendMsg(u2.Message)
			case *tg.UpdateNewChannelMessage:
				appendMsg(u2.Message)
			}
		}
	case *tg.UpdateShort:
		if u2, ok := tu.Update.(*tg.UpdateNewMessage); ok {
			appendMsg(u2.Message)
		}
	case *tg.UpdateShortChatMessage, *tg.UpdateShortMessage:
		// 短更新无完整 Message 对象；冒烟目的已由上面覆盖，跳过
	}
	return out
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func fail(format string, a ...any) {
	fmt.Printf("✗ "+format+"\n", a...)
	os.Exit(1)
}
